package screens

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// routeLifecycle answers the screen's own connection state: the quit
// the user asked for, the backend's answer to it, and whether the
// persisted config still carries an API key.
func (s ChatScreen) routeLifecycle(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case ui.QuitRequestedMsg:
		next, cmd := s.handleQuitRequested(msg)
		return next, cmd, true

	case ui.QuitCompleteMsg:
		return s, tea.Quit, true

	case apiKeyMissingMsg:
		s.apiKeyMissing = msg.missing
		return s, s.listenForAPIKeyChanges(), true
	}

	return s, nil, false
}

// handleQuitRequested locks the UI, shows a "Disconnecting…"
// indication, and runs the backend quit asynchronously. The result
// arrives as a QuitCompleteMsg, which the screen turns into
// tea.Quit.
func (s ChatScreen) handleQuitRequested(msg ui.QuitRequestedMsg) (ChatScreen, tea.Cmd) {
	if s.quitting {
		// A second quit request while the first is in flight is an
		// escape hatch: the user pressed Ctrl+C again because the
		// disconnect looks stuck. Bypass Session.Quit and exit now.
		return s, tea.Quit
	}

	s.quitting = true

	message := msg.Message

	// The "Disconnecting…" feedback comes from the status item that
	// StatusItems appends when s.quitting is true; the status bar is
	// always rendered when the terminal is wide enough, so no
	// placeholder fallback is needed.
	cmds := []tea.Cmd{
		msgCmd(components.InputLockedMsg{Locked: true}),
		func() tea.Msg {
			resp, err := s.user.Send(s.baseContext(), protocol.Quit{Reason: message})
			if err == nil {
				err = resp.Err
			}
			return ui.QuitCompleteMsg{Err: err}
		},
	}

	return s, tea.Batch(cmds...)
}

// apiKeyMissingMsg reports the API key's presence in the persisted
// config. One is sent for every config write that changes the key,
// whichever command made it.
type apiKeyMissingMsg struct{ missing bool }

// listenForAPIKeyChanges reads the next update off apiKeyChanges,
// the channel the cfgStore.OnChange listener writes to. Mirrors
// listenForProtocolEvents in that this should be re-invoked after
// each delivery so the channel is continuously drained, but the two
// end differently: listenForProtocolEvents' channel belongs to a
// session subscription the session closes on teardown, while
// apiKeyChanges is owned by this screen with nothing else to close
// it. This instead ends when baseContext is done — the application
// shutting down — and unsubscribes from cfgStore.OnChange in the same
// moment, so a config write after shutdown finds no listener left to
// call.
func (s ChatScreen) listenForAPIKeyChanges() tea.Cmd {
	ch := s.apiKeyChanges
	if ch == nil {
		return nil
	}

	ctx := s.baseContext()

	return func() tea.Msg {
		select {
		case missing, ok := <-ch:
			if !ok {
				return nil
			}

			return apiKeyMissingMsg{missing: missing}

		case <-ctx.Done():
			if s.unsubscribeAPIKeyChanges != nil {
				s.unsubscribeAPIKeyChanges()
			}

			return nil
		}
	}
}

// watchAPIKey subscribes to the config store and reports every change
// to the API key's presence on the returned channel.
//
// A config write's OnChange callback can run on any goroutine that
// called Save or Update (see FileStore.Save), concurrently with
// another such call, so the send must not assume it is the only
// writer. sendLatest keeps the channel holding whatever value was sent
// most recently: a plain non-blocking send would do the opposite when
// the buffer is already full — leaving the first unread value in place
// and dropping the new one — so two rapid opposite writes (key set,
// then cleared) would leave listenForAPIKeyChanges reading "key
// configured" until some later, unrelated write happened to correct
// it.
func watchAPIKey(store config.Store, changes chan bool) config.UnsubscribeFunc {
	if store == nil {
		return nil
	}

	return store.OnChange(func(_ context.Context, prev, curr config.Config) {
		if prev.APIKey == curr.APIKey {
			return
		}

		sendLatest(changes, curr.APIKey == "")
	})
}

// sendLatest sends v on ch, replacing whatever value ch already held
// if it was full, so a reader draining ch one value at a time always
// eventually reads the most recently sent value rather than getting
// stuck behind an older one nobody has read yet. Safe for concurrent
// callers: each retries until its own send lands, and ch's buffer
// never holds more than one value at a time either way.
func sendLatest[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
		}

		select {
		case <-ch:
		default:
		}
	}
}

// disconnectingStatusItem is the always-visible feedback the chat and
// connection screens emit while a quit is in flight, so the user
// sees something happening even if Session.Quit takes a moment.
var disconnectingStatusItem = ui.StatusItem{
	ID:       "disconnecting",
	Side:     ui.StatusSideRight,
	Priority: 100,
	Full:     "Disconnecting…",
	Compact:  "off…",
}

// noAPIKeyStatusItem prompts the user to run /config while no API
// key is configured. Spec point 1.1 has the app open and prompt for
// /config until this is done; the welcome checklist carries the same
// prompt but only while no channel is open, so this status item is
// what keeps the prompt visible once the user has joined something.
var noAPIKeyStatusItem = ui.StatusItem{
	ID:       "no-api-key",
	Side:     ui.StatusSideRight,
	Priority: 90,
	Full:     "No API key configured — use /config to set one",
	Compact:  "no key",
}
