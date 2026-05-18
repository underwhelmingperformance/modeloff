package screens_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/uitest"
)

// capturedRecord holds the fields we assert on from a slog.Record.
type capturedRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// recordHandler is a thread-safe slog.Handler that accumulates records.
// Mirror of logSink in chat_live_models_test.go — keep in sync.
type recordHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})

	h.mu.Lock()
	h.records = append(h.records, capturedRecord{
		Level:   r.Level,
		Message: r.Message,
		Attrs:   attrs,
	})
	h.mu.Unlock()

	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordHandler) find(msg string) (capturedRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}

	return capturedRecord{}, false
}

func (h *recordHandler) all() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]capturedRecord(nil), h.records...)
}

func captureLogs(t *testing.T) *recordHandler {
	t.Helper()

	h := &recordHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return h
}

func TestChatScreen_command_execution_is_logged(t *testing.T) {
	h := captureLogs(t)

	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/topic cool topic")
	// Wait for the topic change event, not just the typed text.
	tm.WaitFor("set by testuser")

	rec, found := h.find("command executed")
	require.True(t, found, "expected 'command executed' log entry, got: %v", h.all())
	require.Equal(t, slog.LevelInfo, rec.Level)
	require.Equal(t, "topic", rec.Attrs["command"])
	require.Equal(t, "/topic cool topic", rec.Attrs["raw"])
	require.Equal(t, "#general", rec.Attrs["channel"])
	require.Equal(t, "ui", rec.Attrs["component"])
}

func TestChatScreen_command_parse_failure_is_logged(t *testing.T) {
	h := captureLogs(t)

	tm, _ := newChatAppInChannel(t, "#general")

	tm.Submit("/nonexistent blah")
	// Wait for the error display which includes "unknown command".
	tm.WaitFor("unknown command")

	rec, found := h.find("command parse failed")
	require.True(t, found, "expected 'command parse failed' log entry, got: %v", h.all())
	require.Equal(t, slog.LevelWarn, rec.Level)
	require.Equal(t, "/nonexistent blah", rec.Attrs["raw"])
	require.Equal(t, "ui", rec.Attrs["component"])
}

func TestChatScreen_keybind_toggle_nick_list_is_logged(t *testing.T) {
	logs := captureLogs(t)

	h := newTestSession(t)
	uitest.SeedChannel(t, h.sess, "#general")

	tm := newChatApp(t, h)
	tm.WaitFor("#general")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlN})

	tm.Submit("hello after toggle")
	tm.WaitFor("hello after toggle")

	rec, found := logs.find("keybind triggered")
	require.True(t, found, "expected 'keybind triggered' log entry, got: %v", logs.all())
	require.Equal(t, slog.LevelInfo, rec.Level)
	require.Equal(t, "toggle_nick_list", rec.Attrs["action"])
	require.Equal(t, "ui", rec.Attrs["component"])
}
