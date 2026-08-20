package userclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/userclient"
)

// fixture is the common setup the user-client tests share: an
// in-memory store, a noop-API model manager, a session, and an
// attached user-client.
type fixture struct {
	sess  *session.Session
	store *storemod.SQLiteStore
	user  *userclient.UserClient
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	mgr := modelmanager.New(modelmanager.Config{
		Store:       s,
		APIClient:   &noopAPI{},
		BaseContext: t.Context,
	})
	t.Cleanup(mgr.DetachAll)

	sess := session.New(t.Context, s, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s, userclient.NewStoreReplyLog(s))
	require.NoError(t, user.Attach(t.Context()))

	return &fixture{sess: sess, store: s, user: user}
}

func TestUserClient_reports_operator_capability(t *testing.T) {
	f := newFixture(t)

	require.True(t, f.user.Caps().Has(protocol.CapOperator))
	require.Equal(t, domain.Nick("testuser"), f.user.Nick())
	require.Equal(t, protocol.UserClientID, f.user.Identity())
}

func TestUserClient_attach_is_idempotent(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.user.Attach(t.Context()))
	require.NotNil(t, f.user.Subscription())
}

func TestUserClient_Join_routes_through_dispatcher(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.user.Join(t.Context(), "#general"))

	channels := f.user.Channels()
	require.NotNil(t, channels)
	_, ok := channels.Get("#general")
	require.True(t, ok)
}

// TestUserClient_joining_marks_the_channel_read pins that arriving
// in a channel leaves nothing showing as unread. The read cursor is
// this client's state, so the client is what stamps it; the server
// does not touch it.
//
// Every way the user joins has to behave the same. `/join` builds
// its own [protocol.Join] in the chatcmd grammar and dispatches it
// through `Send`, never touching [userclient.UserClient.Join], so a
// cursor stamped only in `Join` would leave the app's primary join
// command showing a badge the moment you arrive. The unprefixed
// case covers `/join general`, where the dispatcher normalises the
// name and the cursor has to land on the same key the events did.
//
// The rejoin case is the one that matters most in use: a channel
// left and rejoined must not count the backlog it was away for.
func TestUserClient_joining_marks_the_channel_read(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(t *testing.T, f *fixture)
		join  func(t *testing.T, f *fixture)
	}{
		{
			name:  "Join does not count its own arrival",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
			},
		},
		{
			name:  "a JOIN dispatched through Send does not count its own arrival",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				resp, err := f.user.Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"#general"}})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
		},
		{
			name:  "a JOIN naming the channel unprefixed stamps the normalised name",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				resp, err := f.user.Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"general"}})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
		},
		{
			name: "a rejoin does not count the backlog",
			setUp: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
				require.NoError(t, f.user.Part(t.Context(), "#general", "brb"))

				for _, body := range []string{"first", "second", "third"} {
					_, err := f.store.AppendEvent(t.Context(), "#general", domain.Message{
						Target: "#general",
						From:   "botty",
						Body:   body,
					})
					require.NoError(t, err)
				}
			},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setUp(t, f)
			tt.join(t, f)

			unread, err := f.sess.UnreadCount(t.Context(), "#general")
			require.NoError(t, err)
			require.Equal(t, 0, unread)
		})
	}
}

// TestUserClient_a_message_after_joining_counts_as_unread pins the
// other half: the cursor stamped on arrival must not swallow what
// arrives afterwards. This is what puts the badge on an inactive
// channel in the sidebar.
func TestUserClient_a_message_after_joining_counts_as_unread(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.user.Join(ctx, "#general"))

	unread, err := f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, unread)

	_, err = f.store.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general",
		From:   "botty",
		Body:   "hello after you arrived",
	})
	require.NoError(t, err)

	unread, err = f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 1, unread)
}

// TestUserClient_multi_target_join_stamps_only_the_channels_that_joined
// covers the read-cursor half of a multi-target JOIN (RFC 2812
// §3.2.1): a gate refusal on one channel must not withhold the
// cursor stamp the other, successful channel in the same command
// has already earned.
func TestUserClient_multi_target_join_stamps_only_the_channels_that_joined(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	locked := domain.NewChannelWindow("#locked", time.Now())
	locked.Modes = domain.ChannelModes{InviteOnly: true}
	require.NoError(t, f.store.SaveWindow(ctx, locked))

	for _, body := range []string{"first", "second"} {
		_, err := f.store.AppendEvent(ctx, "#locked", domain.Message{
			Target: "#locked",
			From:   "botty",
			Body:   body,
		})
		require.NoError(t, err)
	}

	resp, err := f.user.Send(ctx, protocol.Join{Channels: []domain.ChannelName{"#general", "#locked"}})
	require.NoError(t, err)

	var refused domain.ChannelInviteOnlyError
	require.ErrorAs(t, resp.Err, &refused)
	require.Equal(t, domain.ChannelName("#locked"), refused.Channel)

	generalUnread, err := f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, generalUnread, "the channel that joined must have its cursor stamped")

	lockedUnread, err := f.sess.UnreadCount(ctx, "#locked")
	require.NoError(t, err)
	require.Equal(t, 2, lockedUnread, "the refused channel must not have its cursor stamped")
}

func TestUserClient_JoinAutojoinChannels_emits_aggregate_span(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.store.SetAutojoinChannels(ctx,
		[]domain.ChannelName{"#alpha", "#beta"}))

	require.NoError(t, f.user.JoinAutojoinChannels(ctx))

	span := oteltest.FindSpan(t, recorder, "userclient.autojoin")
	require.Equal(t, "2",
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinCount))
	require.Equal(t, "0",
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinFailed))
	require.Equal(t, `["#alpha","#beta"]`,
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinChannels))
}

func TestUserClient_RecordReply_persists_to_issuer_reply_log(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	reply := domain.CommandError{
		Target: "#general",
		Err:    "whois: no such nick",
		At:     time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}

	f.user.RecordReply(ctx, reply)

	replies, err := f.store.InstanceRepliesBefore(ctx, domain.InstanceID(protocol.UserClientID), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{{ID: 1, Event: reply}}, replies)
}

// noopAPI satisfies [api.Client] with empty responses — enough for
// the user-client's join / poke / autojoin paths, none of which
// exercise the model dispatch loop.
type noopAPI struct{}

func (noopAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }
func (noopAPI) SendEvents(
	context.Context,
	domain.ModelID,
	domain.InstanceID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}
func (noopAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}
func (noopAPI) GenerateNick(context.Context, domain.ModelID, string, []domain.Nick) (api.NicknameResult, error) {
	return api.NicknameResult{Nick: "noopnick"}, nil
}
func (noopAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}
