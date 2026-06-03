package testclient_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/testclient"
	"github.com/laney/modeloff/internal/userclient"
)

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
		APIClient:   noopAPI{},
		BaseContext: t.Context,
	})
	t.Cleanup(mgr.DetachAll)

	sess := session.New(t.Context, s, mgr)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s)
	require.NoError(t, user.Attach(t.Context()))

	return &fixture{sess: sess, store: s, user: user}
}

func TestTestClient_New_applies_defaults(t *testing.T) {
	f := newFixture(t)

	bot := testclient.New("seedbot", f.sess)

	require.Equal(t, protocol.ClientID("test-seedbot"), bot.Identity())
	require.Equal(t, domain.Nick("seedbot"), bot.Instance().Nick())
	require.Equal(t, domain.ModelID("test/model"), bot.Instance().ModelID)
	require.Nil(t, bot.Events())
}

func TestTestClient_New_applies_options(t *testing.T) {
	f := newFixture(t)

	bot := testclient.New("seedbot", f.sess,
		testclient.WithInstanceID("inst-custom"),
		testclient.WithModelID("vendor/model"),
		testclient.WithPersona("a curious bot"),
		testclient.WithChannels("#general", "#random"),
	)

	require.Equal(t, protocol.ClientID("inst-custom"), bot.Identity())
	require.Equal(t, domain.ModelID("vendor/model"), bot.Instance().ModelID)

	channels := bot.Instance().Channels()
	require.NotNil(t, channels)
	_, hasGeneral := channels.Get("#general")
	_, hasRandom := channels.Get("#random")
	require.True(t, hasGeneral)
	require.True(t, hasRandom)
}

func TestTestClient_Attach_persists_and_subscribes(t *testing.T) {
	f := newFixture(t)

	bot := testclient.New("seedbot", f.sess,
		testclient.WithInstanceID("inst-seedbot"),
	)

	require.NoError(t, bot.Attach(t.Context()))
	t.Cleanup(bot.Detach)

	stored, err := f.store.GetInstanceByID(t.Context(), "inst-seedbot")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("seedbot"), stored.Nick())
	require.NotNil(t, bot.Events())
}

func TestTestClient_Attach_is_idempotent(t *testing.T) {
	f := newFixture(t)

	bot := testclient.New("seedbot", f.sess)

	require.NoError(t, bot.Attach(t.Context()))
	require.NoError(t, bot.Attach(t.Context()))
	t.Cleanup(bot.Detach)
}

func TestTestClient_Send_routes_through_dispatcher(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.user.Join(t.Context(), "#general"))

	bot := testclient.New("seedbot", f.sess,
		testclient.WithChannels("#general"),
	)
	require.NoError(t, bot.Attach(t.Context()))
	t.Cleanup(bot.Detach)

	resp, err := bot.Send(t.Context(), protocol.PrivMsg{Target: "#general", Body: "hi"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
}

func TestTestClient_Detach_is_idempotent(t *testing.T) {
	f := newFixture(t)

	bot := testclient.New("seedbot", f.sess)
	require.NoError(t, bot.Attach(t.Context()))

	bot.Detach()
	bot.Detach()
	require.Nil(t, bot.Events())
}

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
