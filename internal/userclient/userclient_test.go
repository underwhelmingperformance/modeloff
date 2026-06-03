package userclient_test

import (
	"context"
	"testing"

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

	sess := session.New(t.Context, s, mgr)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s)
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
