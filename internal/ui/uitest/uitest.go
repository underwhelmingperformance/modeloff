// Package uitest provides shared helpers for UI integration tests.
// It wraps teatest.TestModel with an output accumulator that
// survives WaitFor draining the output buffer, so FinalView can
// return all rendered content without racing against Quit.
package uitest

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
)

// App wraps teatest.TestModel with a cumulative output buffer.
// teatest.WaitFor drains the output stream, so later reads only see
// what was rendered after the last WaitFor. App tees every read into
// a buffer that accumulates across the entire test, letting
// FinalView return all rendered content.
type App struct {
	*teatest.TestModel
	t testing.TB

	mu  sync.Mutex
	buf bytes.Buffer
}

// New creates an App from a tea.Model with the given options.
// If no options are provided, a default 80x24 terminal is used.
func New(t testing.TB, m tea.Model, opts ...teatest.TestOption) *App {
	t.Helper()

	if len(opts) == 0 {
		opts = []teatest.TestOption{teatest.WithInitialTermSize(80, 24)}
	}

	tm := teatest.NewTestModel(t, m, opts...)
	t.Cleanup(func() { _ = tm.Quit() })

	return &App{TestModel: tm, t: t}
}

// output returns a reader that tees every byte read from the
// teatest output stream into the cumulative buffer.
func (a *App) output() io.Reader {
	return io.TeeReader(a.TestModel.Output(), &lockedWriter{mu: &a.mu, buf: &a.buf})
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.Write(p)
}

// Submit types text and presses Enter.
func (a *App) Submit(text string) {
	a.Type(text)
	a.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

// WaitFor blocks until every part appears in the output stream.
func (a *App) WaitFor(parts ...string) {
	a.t.Helper()

	teatest.WaitFor(a.t, a.output(), func(out []byte) bool {
		for _, part := range parts {
			if !bytes.Contains(out, []byte(part)) {
				return false
			}
		}

		return true
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

// WaitForCondition blocks until condition returns true against the
// output accumulated since this call began. Unlike WaitFor, the
// condition receives a fresh buffer (not the cumulative one), so it
// can be used for absence checks like "responding indicator gone".
// Output still flows into the cumulative buffer for FinalView.
func (a *App) WaitForCondition(condition func([]byte) bool) {
	a.t.Helper()

	teatest.WaitFor(a.t, a.output(), condition,
		teatest.WithDuration(2*time.Second),
		teatest.WithCheckInterval(10*time.Millisecond))
}

// FinalView drains remaining output, quits the program, and returns
// all rendered content accumulated across the test. Because the
// content was captured *before* Quit, there is no race with QuitMsg
// processing.
//
// The returned string contains all frames ever rendered. Use
// require.Contains for positive assertions. For absence checks on
// the current screen state, use CurrentView instead.
func (a *App) FinalView() string {
	a.t.Helper()

	// Drain output rendered since the last WaitFor.
	_, err := io.ReadAll(a.output())
	require.NoError(a.t, err)

	require.NoError(a.t, a.Quit())
	a.WaitFinished(a.t, teatest.WithFinalTimeout(2*time.Second))

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.buf.String()
}

// CurrentView quits the program and returns the view rendered by the
// final model state. Unlike FinalView, this returns only the current
// screen — not the cumulative output. Use this for NotContains
// assertions where earlier frames would cause false positives.
func (a *App) CurrentView() string {
	a.t.Helper()

	require.NoError(a.t, a.Quit())
	a.WaitFinished(a.t, teatest.WithFinalTimeout(2*time.Second))

	fm := a.FinalModel(a.t)

	type viewer interface {
		View() string
	}

	m, ok := fm.(viewer)
	require.True(a.t, ok, "final model does not implement View() string")

	return m.View()
}

// FakeAPI is a configurable test double for api.Client. Each method
// delegates to the corresponding function field when set, falling back
// to a sensible default otherwise. The mutex protects concurrent access
// from model goroutines during teatest runs.
type FakeAPI struct {
	mu             sync.Mutex
	ListModelsFn   func(context.Context) ([]api.ModelInfo, error)
	SendEventsFn   func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	GenerateNickFn func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
}

func (f *FakeAPI) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ListModelsFn != nil {
		return f.ListModelsFn(ctx)
	}

	return nil, nil
}

func (f *FakeAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) (api.CompletionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.SendEventsFn != nil {
		response, err := f.SendEventsFn(ctx, modelID, system, history, events)
		return api.CompletionResult{Response: response}, err
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

func (f *FakeAPI) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (api.NicknameResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GenerateNickFn != nil {
		nick, err := f.GenerateNickFn(ctx, nickModel, modelID)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "fakenick"}, nil
}

// SeedChannel creates a channel via the session.
func SeedChannel(t testing.TB, sess *session.Session, name string) {
	t.Helper()

	_, err := sess.Join(t.Context(), name)
	require.NoError(t, err)
}

// SeedMessage sends a message to a channel via the session.
func SeedMessage(t testing.TB, sess *session.Session, channel, body string) {
	t.Helper()

	_, err := sess.SendMessage(t.Context(), domain.ChannelName(channel), body)
	require.NoError(t, err)
}
