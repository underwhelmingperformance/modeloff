package screens

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

// logRecord and logSink mirror recordHandler in chat_logging_test.go —
// keep in sync. Duplication is tolerated because the two helpers live
// in different packages (screens_test vs screens) and a shared helper
// would force an awkward cross-package export for a test-only type.
type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *logSink) Enabled(context.Context, slog.Level) bool { return true }

func (h *logSink) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})

	h.mu.Lock()
	h.records = append(h.records, logRecord{
		Level: r.Level,
		Msg:   r.Message,
		Attrs: attrs,
	})
	h.mu.Unlock()

	return nil
}

func (h *logSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logSink) WithGroup(string) slog.Handler      { return h }

func (h *logSink) find(msg string) (logRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Msg == msg {
			return r, true
		}
	}

	return logRecord{}, false
}

func (h *logSink) all() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]logRecord(nil), h.records...)
}

func installLogSink(t *testing.T) *logSink {
	t.Helper()

	h := &logSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return h
}

const liveModelsUnavailableNotice = "Model list unavailable: upstream 503."

// withAt returns a copy of n with its `At` field replaced by at,
// letting callers write one structural equality check without
// threading time.Now() through the test.
func withAt(n domain.SystemNotice, at time.Time) domain.SystemNotice {
	n.At = at
	return n
}

func TestChatScreen_handleLiveModelsLoadFailed(t *testing.T) {
	upstreamErr := errors.New("upstream 503")

	tests := map[string]struct {
		active          domain.ChannelName
		lastChannel     domain.ChannelName
		expectedChannel domain.ChannelName
		// liveStored pins whether the returned Cmd surfaces the
		// StoredEvent for live append. The handler returns it only
		// when the routing target equals `*s.active`; off-channel
		// notices flow exclusively through the scrollback append
		// so a focus-driven re-render finds them in `s.scrollback`
		// without doubling them on a focus-back.
		liveStored bool
	}{
		"active channel focused": {
			active:          "#general",
			expectedChannel: "#general",
			liveStored:      true,
		},
		"no active channel routes to status window": {
			// After α, connect-time live-model loads run in the
			// connection screen ahead of autojoin's focus event, so
			// the chat-screen failure handler only fires from the
			// `/config api-key` refresh callsites. When no real
			// channel is joined yet the routing target falls back
			// directly to `&modeloff` — the chat-screen-owned
			// default landing window — rather than consulting
			// LastChannel for an intermediate hop.
			active:          "",
			lastChannel:     "#previous",
			expectedChannel: domain.StatusChannelName,
			liveStored:      false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logs := installLogSink(t)

			sess := newTestSession(t)
			if tc.active != "" {
				require.NoError(t, sess.Join(t.Context(), string(tc.active)))
			}
			if tc.lastChannel != "" {
				require.NoError(t, sess.Join(t.Context(), string(tc.lastChannel)))
			}

			screen, err := NewChatScreen(t.Context(), sess, nil, nil, domain.KindStatus)
			require.NoError(t, err)
			*screen.active = tc.active
			*screen.liveModels = placeholderModels()
			*screen.liveModelsState = command.SuggestionStateReady

			_, cmd := screen.handleLiveModelsLoadFailed(liveModelsLoadFailedMsg{err: upstreamErr})

			require.Nil(t, *screen.liveModels, "liveModels should be emptied")
			require.Equal(t, command.SuggestionStateError, *screen.liveModelsState,
				"real upstream failure must flip completer state to Error so the popover is suppressed")
			require.NotNil(t, cmd)

			msgs := collectMsgs(cmd)

			want := domain.SystemNotice{
				Target: tc.expectedChannel,
				Text:   liveModelsUnavailableNotice,
			}

			if tc.liveStored {
				stored, ok := containsMsg[domain.StoredEvent](msgs)
				require.True(t, ok, "expected StoredEvent in batch, got %v", msgs)

				notice, ok := stored.Event.(domain.SystemNotice)
				require.True(t, ok, "expected SystemNotice, got %T", stored.Event)

				require.Equal(t, withAt(want, notice.At), notice)
				require.WithinDuration(t, time.Now(), notice.At, time.Second)
			} else {
				_, ok := containsMsg[domain.StoredEvent](msgs)
				require.False(t, ok,
					"off-channel notice must not return a live StoredEvent — scrollback owns the next render")
			}

			persisted, err := sess.EventsBefore(t.Context(), tc.expectedChannel, nil, 10)
			require.NoError(t, err)

			got := filterSystemNotices(persisted)
			// SQLite roundtrip drops time.Time's monotonic clock;
			// normalise the persisted timestamp to the literal we
			// compare against, so the structural assertion holds.
			normalised := make([]domain.SystemNotice, len(got))
			for i, n := range got {
				require.WithinDuration(t, time.Now(), n.At, time.Second)
				n.At = want.At
				normalised[i] = n
			}
			require.Equal(t, []domain.SystemNotice{want}, normalised)

			// `logAndShowOn`'s Cmd appends the persisted event to
			// `s.scrollback[ch]` so a subsequent focus-driven
			// `HistoryLoadedMsg(ch)` finds the line in scrollback
			// rather than wiping it. Pin the in-memory contract
			// for every sub-case (live and off-channel) — for
			// the off-channel branches this is the load-bearing
			// side-effect, since the Cmd returns nil and the
			// persisted-events check alone would not catch a
			// regression that dropped the scrollback append.
			//
			// Compare against the same `[]domain.SystemNotice`
			// projection the persisted-events check uses: the
			// scrollback must surface exactly the notices the
			// store recorded, in the same order, with no extras.
			storedEvents := screen.scrollbackOf(tc.expectedChannel)
			scrollbackNotices := make([]domain.SystemNotice, 0, len(storedEvents))
			for _, ev := range storedEvents {
				notice, ok := ev.Event.(domain.SystemNotice)
				require.True(t, ok,
					"unexpected scrollback event %T on %s; only the notice should be present",
					ev.Event, tc.expectedChannel)
				notice.At = want.At
				scrollbackNotices = append(scrollbackNotices, notice)
			}
			require.Equal(t, []domain.SystemNotice{want}, scrollbackNotices,
				"scrollback for %s must hold exactly the persisted notice", tc.expectedChannel)

			rec, found := logs.find("live models load failed")
			require.True(t, found, "expected slog record, got %v", logs.all())
			require.Equal(t, slog.LevelWarn, rec.Level)
			require.Equal(t, "ui", rec.Attrs["component"])
			require.Equal(t, string(tc.expectedChannel), rec.Attrs["channel"])
			require.Equal(t, "upstream 503", rec.Attrs["error"])
		})
	}
}

func TestChatScreen_handleLiveModelsLoadFailed_silent_on_no_api_key(t *testing.T) {
	tests := map[string]error{
		"bare sentinel":    session.ErrNoAPIKey,
		"wrapped sentinel": fmt.Errorf("ListModels: %w", session.ErrNoAPIKey),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			logs := installLogSink(t)

			screen, newErr := NewChatScreen(t.Context(), newTestSession(t), nil, nil, domain.KindStatus)
			require.NoError(t, newErr)
			*screen.liveModels = placeholderModels()
			*screen.liveModelsState = command.SuggestionStateReady

			_, cmd := screen.handleLiveModelsLoadFailed(liveModelsLoadFailedMsg{err: err})

			require.Nil(t, *screen.liveModels)
			require.Equal(t, command.SuggestionStateReady, *screen.liveModelsState,
				"ErrNoAPIKey must leave the completer in Ready: the popover suppression is reserved for genuine upstream failures")
			require.Nil(t, cmd, "ErrNoAPIKey is a validation race and must not surface")

			_, found := logs.find("live models load failed")
			require.False(t, found, "ErrNoAPIKey must not be logged; got %v", logs.all())
		})
	}
}

func placeholderModels() []chatcmd.ModelOption {
	return []chatcmd.ModelOption{{ID: "test/model"}}
}

func TestChatScreen_APIKeySetResult_clears_live_models_and_resets_state(t *testing.T) {
	tests := map[string]chatcmd.APIKeySetResult{
		"set":   {Reset: false},
		"reset": {Reset: true},
	}

	for name, msg := range tests {
		t.Run(name, func(t *testing.T) {
			sess := newTestSession(t)
			require.NoError(t, sess.Join(t.Context(), "#general"))

			screen, err := NewChatScreen(t.Context(), sess, nil, nil, domain.KindStatus)
			require.NoError(t, err)
			*screen.active = "#general"
			*screen.liveModels = placeholderModels()
			*screen.liveModelsState = command.SuggestionStateError

			_, _ = screen.Update(msg)

			require.Nil(t, *screen.liveModels,
				"APIKeySetResult must clear the stale cache so the next loadLiveModels tick repopulates it")
			require.Equal(t, command.SuggestionStateReady, *screen.liveModelsState,
				"APIKeySetResult must reset completer state so the popover reappears once the reload lands")
		})
	}
}

func filterSystemNotices(events []domain.StoredEvent) []domain.SystemNotice {
	var notices []domain.SystemNotice
	for _, evt := range events {
		if n, ok := evt.Event.(domain.SystemNotice); ok {
			notices = append(notices, n)
		}
	}

	return notices
}

var _ tea.Msg = liveModelsLoadFailedMsg{}
