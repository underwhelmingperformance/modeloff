package modelmanager

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
)

// TestModelClient_Caps_follows_the_subscription_mode_set pins where a
// model-client's capabilities come from: the session, on every
// question. A model subscribes with no modes and so holds nothing;
// once the session grants it `+o` through OPER, the same client
// reports the operator capability, and the command-visibility filter
// and the model tool registry both see it.
//
// The two client kinds have to agree on where the answer comes from.
// A hardcoded holder on either side lets the palette a client is
// offered drift from what the dispatcher's operator gate will
// actually run for it.
func TestModelClient_Caps_follows_the_subscription_mode_set(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	mgr := New(Config{APIClient: &apitest.Fake{}, Store: store, BaseContext: t.Context})
	t.Cleanup(func() { _ = mgr.DetachAll(t.Context()) })

	sess := session.New(t.Context(), store, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, store.SaveInstance(t.Context(), inst))

	require.NoError(t, sess.StartModelClients(t.Context()))
	mgr.clientsMu.Lock()
	client := mgr.clients["inst-botty"]
	mgr.clientsMu.Unlock()
	require.NotNil(t, client)

	require.False(t, client.Caps().Has(protocol.CapOperator))

	sess.SetOperAuthenticator(func(protocol.Client, string, string) bool { return true })

	resp, err := client.Send(t.Context(), protocol.Oper{User: "botty", Password: "hunter2"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	require.True(t, client.Caps().Has(protocol.CapOperator))
}

// TestSession_ClientCaps_answers_nothing_for_an_unregistered_identity
// covers the other half of the delegation: an identity the session
// holds no subscription for grants nothing, which is what a client
// that has not attached yet (or has already quit) reports.
func TestSession_ClientCaps_answers_nothing_for_an_unregistered_identity(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	mgr := New(Config{APIClient: &apitest.Fake{}, Store: store, BaseContext: t.Context})
	t.Cleanup(func() { _ = mgr.DetachAll(t.Context()) })

	sess := session.New(t.Context(), store, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	require.False(t, sess.ClientCaps("inst-nobody").Has(protocol.CapOperator))
}
