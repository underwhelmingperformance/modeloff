package screens

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
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

func TestChatScreen_handleLiveModelsLoadFailed(t *testing.T) {
	upstreamErr := errors.New("upstream 503")

	tests := map[string]struct {
		active          domain.ChannelName
		lastChannel     domain.ChannelName
		expectedChannel domain.ChannelName
	}{
		"active channel focused": {
			active:          "#general",
			expectedChannel: "#general",
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
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logs := installLogSink(t)

			sess, mgr, user := newTestSession(t)
			if tc.active != "" {
				require.NoError(t, user.Join(t.Context(), tc.active))
			}
			if tc.lastChannel != "" {
				require.NoError(t, user.Join(t.Context(), tc.lastChannel))
			}

			screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
			require.NoError(t, err)
			*screen.active = tc.active
			*screen.liveModels = placeholderModels()
			*screen.liveModelsState = command.SuggestionStateReady

			_, cmd := screen.handleLiveModelsLoadFailed(liveModelsLoadFailedMsg{err: upstreamErr})

			require.Nil(t, *screen.liveModels, "liveModels should be emptied")
			require.Equal(t, command.SuggestionStateError, *screen.liveModelsState,
				"real upstream failure must flip completer state to Error so the popover is suppressed")
			require.NotNil(t, cmd)

			want := domain.SystemNotice{
				Target: tc.expectedChannel,
				Text:   liveModelsUnavailableNotice,
			}

			// The notice nudges the message list to re-read the
			// target window's scrollback; it never returns a
			// StoredEvent for direct render.
			msgs := collectMsgs(cmd)
			require.Equal(t, []tea.Msg{
				components.ScrollbackUpdatedMsg{Channel: tc.expectedChannel},
			}, msgs)

			// The notice is UI feedback, not channel activity: it
			// lands only in the in-memory scrollback and never
			// reaches the shared channel event log.
			persisted, err := sess.EventsBefore(t.Context(), tc.expectedChannel, nil, 10)
			require.NoError(t, err)
			require.Empty(t, filterSystemNotices(persisted),
				"a UI notice must not pollute the channel event log")

			scrollback := screen.scrollbackOf(tc.expectedChannel)
			scrollbackNotices := make([]domain.SystemNotice, 0, len(scrollback))
			for _, ev := range scrollback {
				notice, ok := ev.(domain.SystemNotice)
				require.True(t, ok,
					"unexpected scrollback event %T on %s; only the notice should be present",
					ev, tc.expectedChannel)
				notice.At = want.At
				scrollbackNotices = append(scrollbackNotices, notice)
			}
			require.Equal(t, []domain.SystemNotice{want}, scrollbackNotices,
				"scrollback for %s must hold exactly the rendered notice", tc.expectedChannel)

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
		"bare sentinel":    modelclient.ErrNoAPIKey,
		"wrapped sentinel": fmt.Errorf("ListModels: %w", modelclient.ErrNoAPIKey),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			logs := installLogSink(t)

			screen := newScreenFixture(t)
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
			sess, mgr, user := newTestSession(t)
			require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))

			screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
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
