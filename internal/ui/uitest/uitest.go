// Package uitest provides shared helpers for UI integration tests.
// It wraps teatest.TestModel with an output accumulator that
// survives WaitFor draining the output buffer, so FinalView can
// return all rendered content without racing against Quit.
package uitest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
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
	return io.TeeReader(a.Output(), &lockedWriter{mu: &a.mu, buf: &a.buf})
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

// RenderedView returns the currently visible screen state by
// replaying the cumulative teatest output through a minimal terminal
// emulator. Bubble Tea's standard renderer emits diff frames
// (skipping unchanged lines), so a tail-of-buffer slice is not a
// true snapshot; virtualScreen reconstructs one by applying cursor
// and erase sequences into a row buffer.
//
// Unlike CurrentView, RenderedView is non-destructive: it does not
// quit the program, so it can be called during polling. Use it in
// combination with WaitForView when the assertion target is the
// pixel state the user would see, rather than the model's View()
// at quit time, which can race against subsequent state churn.
func (a *App) RenderedView() string {
	a.t.Helper()

	if _, err := io.ReadAll(a.output()); err != nil {
		a.t.Fatalf("RenderedView: read output: %s", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	screen := newVirtualScreen()
	screen.feed(a.buf.Bytes())
	return screen.view()
}

// WaitForView polls the currently-rendered screen and returns the
// first view that satisfies predicate. Unlike WaitFor, which matches
// against the cumulative output stream and is satisfied by any
// transient frame that ever contained the substring, WaitForView
// reconstructs the terminal state from the diff frames bubbletea
// emits and presents the predicate with what the user would see
// right now.
//
// The returned view is the exact snapshot that satisfied the
// predicate, captured atomically with the predicate check; subsequent
// state churn cannot invalidate it. Use it as the assertion source
// instead of calling RenderedView again, which would re-sample the
// (possibly mutated) latest state.
func (a *App) WaitForView(predicate func(view string) bool) string {
	a.t.Helper()

	const (
		duration = 2 * time.Second
		interval = 10 * time.Millisecond
	)

	deadline := time.Now().Add(duration)

	for {
		view := a.RenderedView()

		if predicate(view) {
			return view
		}

		if time.Now().After(deadline) {
			a.t.Fatal(fmt.Errorf("WaitForView: predicate not met after %s. Current view:\n%s", duration, view))
			return view
		}

		time.Sleep(interval)
	}
}

// WaitForViewContains is a convenience wrapper around WaitForView
// that waits until every part is present in the currently-rendered
// view. Returns the snapshot at which the predicate was satisfied.
func (a *App) WaitForViewContains(parts ...string) string {
	a.t.Helper()

	return a.WaitForView(func(view string) bool {
		for _, part := range parts {
			if !strings.Contains(view, part) {
				return false
			}
		}

		return true
	})
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
	mu                 sync.Mutex
	ListModelsFn       func(context.Context) ([]api.ModelInfo, error)
	SendEventsFn       func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	GenerateNickFn     func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
	GeneratePersonasFn func(context.Context, domain.ModelID) ([]domain.Persona, error)
}

// ListModels delegates to ListModelsFn or returns nil.
func (f *FakeAPI) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ListModelsFn != nil {
		return f.ListModelsFn(ctx)
	}

	return nil, nil
}

// SendEvents delegates to SendEventsFn or returns a silence response.
func (f *FakeAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	_ domain.InstanceID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
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

// ContinueWithToolResults always returns a silence response.
func (f *FakeAPI) ContinueWithToolResults(
	_ context.Context,
	_ *api.Conversation,
	_ []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence},
	}, nil
}

// GenerateNick delegates to GenerateNickFn or returns "fakenick".
func (f *FakeAPI) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GenerateNickFn != nil {
		nick, err := f.GenerateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "fakenick"}, nil
}

// GeneratePersonas delegates to GeneratePersonasFn or returns nil.
func (f *FakeAPI) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GeneratePersonasFn != nil {
		return f.GeneratePersonasFn(ctx, smallModel)
	}

	return nil, nil
}

// SeedChannel creates a channel by issuing a real /join on the
// session and pins it as the user's last-focused channel in the
// store, mirroring the state a returning user lands in: joined the
// channel and the chat screen treats it as last-active on startup.
// The resulting JoinEvent and friends remain on the session's events
// channel so that a downstream ChatScreen drains and renders them
// when it takes over.
//
// `last_channel` is a UI-owned write in production (the chat screen
// persists it on `ChannelActiveMsg`), but tests that bypass the chat
// screen still need the entry there for the screen's autojoin
// restore to land on the seeded channel; writing through the store
// matches what the previous session's UI would have left behind. The
// last `SeedChannel` call wins.
//
// For integration tests that drive the ConnectionScreen and want to
// simulate "previous session" state, follow up with sess.Quit +
// DrainEvents to leave the channel on the autojoin list without
// lingering membership.
func SeedChannel(t testing.TB, sess *session.Session, name string) {
	t.Helper()

	require.NoError(t, sess.Join(t.Context(), name))
	require.NoError(t, sess.SetLastChannel(t.Context(), domain.ChannelName(name)))
}

// SeedAndFocusChannel creates a channel and emits a session-side
// focus event so the ChatScreen sees the focus signal during this
// run. `last_channel` is already written by `SeedChannel`'s store
// pin, so callers do not need to set it separately.
func SeedAndFocusChannel(t testing.TB, sess *session.Session, name string) {
	t.Helper()

	SeedChannel(t, sess, name)
	require.NoError(t, sess.FocusChannel(t.Context(), domain.ChannelName(name)))
}

// SeedMessage seeds a channel with a message from a synthetic
// model "seedbot". The session does not echo the user's own
// outgoing messages on its events channel (per RFC 2812
// §3.3.1), so a SendMessage from `s.user` would not flow into a
// downstream chat screen's render path; routing the seed
// through a model actor matches the realistic shape (channel
// activity from someone other than the user) and keeps the
// events channel stream populated.
func SeedMessage(t testing.TB, sess *session.Session, channel, body string) {
	t.Helper()

	const seederNick domain.Nick = "seedbot"
	const seederID domain.InstanceID = "inst-seedbot"

	bot, err := sess.ResolveNick(t.Context(), seederNick)
	if err != nil {
		bot = domain.NewModelInstance(seederID, seederNick, "test/model", "", nil)
		require.NoError(t, sess.SaveInstance(t.Context(), bot))
	}

	_, err = sess.SendMessageAs(t.Context(), bot, domain.ChannelName(channel), body)
	require.NoError(t, err)
}

// DrainEvents discards any buffered events on both session event
// buses (the non-protocol UI bus via [session.Session.Events] and
// the user-client subscription's protocol bus via
// [session.Session.User]). This prevents seed operations from
// leaking stale events into the UI when tests start.
func DrainEvents(sess *session.Session) {
	for {
		select {
		case <-sess.Events():
		case <-sess.User().Events():
		default:
			return
		}
	}
}
