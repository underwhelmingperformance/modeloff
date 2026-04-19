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
func withAt(n domain.ChannelSystemNotice, at time.Time) domain.ChannelSystemNotice {
	n.At = at
	return n
}

func TestChatScreen_handleLiveModelsLoadFailed(t *testing.T) {
	upstreamErr := errors.New("upstream 503")

	tests := map[string]struct {
		active          domain.ChannelName
		expectedChannel domain.ChannelName
	}{
		"active channel focused": {
			active:          "#general",
			expectedChannel: "#general",
		},
		"no active channel routes to status": {
			active:          "",
			expectedChannel: domain.StatusChannelName,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logs := installLogSink(t)

			sess := newTestSession(t)
			if tc.active != "" {
				require.NoError(t, sess.Join(t.Context(), string(tc.active)))
			}

			screen, err := NewChatScreen(t.Context(), sess, nil, domain.KindStatus)
			require.NoError(t, err)
			*screen.active = tc.active
			*screen.liveModels = placeholderModels()

			_, cmd := screen.handleLiveModelsLoadFailed(liveModelsLoadFailedMsg{err: upstreamErr})

			require.Nil(t, *screen.liveModels, "liveModels should be emptied")
			require.NotNil(t, cmd)

			msgs := collectMsgs(cmd)

			stored, ok := containsMsg[domain.StoredEvent](msgs)
			require.True(t, ok, "expected StoredEvent in batch, got %v", msgs)

			notice, ok := stored.Event.(domain.ChannelSystemNotice)
			require.True(t, ok, "expected ChannelSystemNotice, got %T", stored.Event)

			want := domain.ChannelSystemNotice{
				Channel: tc.expectedChannel,
				Text:    liveModelsUnavailableNotice,
			}
			require.Equal(t, withAt(want, notice.At), notice)
			require.WithinDuration(t, time.Now(), notice.At, time.Second)

			persisted, err := sess.EventsBefore(t.Context(), tc.expectedChannel, nil, 10)
			require.NoError(t, err)

			// SQLite roundtrip drops time.Time's monotonic clock;
			// normalise to compare structurally.
			got := filterSystemNotices(persisted)
			for i := range got {
				if got[i].At.Equal(notice.At) {
					got[i].At = notice.At
				}
			}
			require.Equal(t, []domain.ChannelSystemNotice{withAt(want, notice.At)}, got)

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

			screen, newErr := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
			require.NoError(t, newErr)
			*screen.liveModels = placeholderModels()

			_, cmd := screen.handleLiveModelsLoadFailed(liveModelsLoadFailedMsg{err: err})

			require.Nil(t, *screen.liveModels)
			require.Nil(t, cmd, "ErrNoAPIKey is a validation race and must not surface")

			_, found := logs.find("live models load failed")
			require.False(t, found, "ErrNoAPIKey must not be logged; got %v", logs.all())
		})
	}
}

func placeholderModels() []chatcmd.ModelOption {
	return []chatcmd.ModelOption{{ID: "test/model"}}
}

func filterSystemNotices(events []domain.StoredEvent) []domain.ChannelSystemNotice {
	var notices []domain.ChannelSystemNotice
	for _, evt := range events {
		if n, ok := evt.Event.(domain.ChannelSystemNotice); ok {
			notices = append(notices, n)
		}
	}

	return notices
}

var _ tea.Msg = liveModelsLoadFailedMsg{}
