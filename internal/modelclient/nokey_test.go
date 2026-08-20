package modelclient

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
)

// TestDispatch_without_an_api_client_makes_no_upstream_call pins what
// a model-client does with no key behind it. `Manager.SetAPIKey` drops
// the client when the key is cleared, and `main` builds none when the
// configured key is empty, so this is the state the app boots into
// before `/config` has been used.
//
// The turn has to end where it stands: no call, no error to classify,
// and so nothing for the re-dispatch path to schedule. The one thing
// it does say is the operator diagnostic, which is what tells the user
// their models are not answering because the app has no key rather
// than because the models chose silence.
func TestDispatch_without_an_api_client_makes_no_upstream_call(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{}

		inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
		mc := New(inst, sess, func() api.Client { return nil }, nil, nil, nil, nil, context.Background, nil)
		mc.retry = retryPolicy{Delay: retryTestDelay}

		require.NoError(t, mc.Attach(t.Context()))
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")
		synctest.Wait()

		require.Equal(t, 0, upstream.callCount())

		require.Equal(t, []domain.ProtocolEvent{
			domain.ModelDispatchStarted{Instance: inst, At: sess.Now()},
			domain.ModelUnavailableError{Channel: "#dev", Nick: "botty", At: sess.Now()},
			domain.ModelDispatchDone{Instance: inst, At: sess.Now()},
		}, sess.emittedEvents())
	})
}
