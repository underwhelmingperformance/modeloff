// Package uitest provides shared helpers for UI integration tests.
// It wraps teatest.TestModel with an output accumulator that
// survives WaitFor draining the output buffer, so FinalView can
// return all rendered content without racing against Quit.
package uitest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"
	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/testclient"
	"github.com/laney/modeloff/internal/userclient"
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

	width  int
	height int
}

// Option configures an App and its underlying teatest model.
type Option func(*appOptions)

type appOptions struct {
	width  int
	height int
}

// WithInitialTermSize sets the terminal size for the program and the
// emulator that reconstructs its rendered output.
func WithInitialTermSize(width, height int) Option {
	return func(opts *appOptions) {
		opts.width = width
		opts.height = height
	}
}

// New creates an App from a tea.Model with the given options.
// If no options are provided, a default 80x24 terminal is used.
func New(t testing.TB, m tea.Model, options ...Option) *App {
	t.Helper()

	opts := appOptions{width: 80, height: 24}
	for _, option := range options {
		option(&opts)
	}

	tm := teatest.NewTestModel(t, m,
		teatest.WithInitialTermSize(opts.width, opts.height),
		teatest.WithProgramOptions(tea.WithColorProfile(colorprofile.ANSI)),
	)
	t.Cleanup(func() { _ = tm.Quit() })

	return &App{
		TestModel: tm,
		t:         t,
		width:     opts.width,
		height:    opts.height,
	}
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
	a.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// WaitFor blocks until every part appears in the visible terminal view.
func (a *App) WaitFor(parts ...string) {
	a.t.Helper()

	a.WaitForViewContains(parts...)
}

// WaitForCondition blocks until condition returns true against the
// output accumulated since this call began. Unlike WaitFor, the
// condition receives a fresh buffer (not the cumulative one), so it
// can be used for absence checks like "spinner gone".
// Output still flows into the cumulative buffer for FinalView.
func (a *App) WaitForCondition(condition func([]byte) bool) {
	a.t.Helper()

	teatest.WaitFor(a.t, a.output(), condition,
		teatest.WithDuration(2*time.Second),
		teatest.WithCheckInterval(10*time.Millisecond))
}

// RenderedView returns the currently visible screen state by replaying
// the cumulative teatest output through a terminal emulator. Bubble Tea
// emits diff frames, so a tail-of-buffer slice is not a true snapshot.
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
	output := bytes.Clone(a.buf.Bytes())
	a.mu.Unlock()

	view, err := renderTerminal(output, a.width, a.height)
	require.NoError(a.t, err)

	return view
}

func renderTerminal(output []byte, width, height int) (string, error) {
	emulator := vt.NewEmulator(width, height)
	input, ok := emulator.InputPipe().(io.Closer)
	if !ok {
		return "", errors.New("terminal input pipe cannot be closed")
	}

	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, emulator)
		drained <- err
	}()
	drain := func() error {
		if err := input.Close(); err != nil {
			return fmt.Errorf("close terminal input: %w", err)
		}

		if err := <-drained; err != nil {
			return fmt.Errorf("drain terminal replies: %w", err)
		}

		return nil
	}

	if _, err := emulator.WriteString(ansi.SetModeLineFeedNewLine); err != nil {
		return "", errors.Join(fmt.Errorf("set line-feed mode: %w", err), drain())
	}
	if _, err := emulator.Write(output); err != nil {
		return "", errors.Join(fmt.Errorf("replay output: %w", err), drain())
	}

	view := strings.TrimRight(emulator.String(), "\n")
	if err := drain(); err != nil {
		return "", err
	}

	return view, nil
}

// WaitForView polls the currently-rendered screen and returns the first
// view that satisfies predicate. It reconstructs the terminal state
// from Bubble Tea's diff frames, so the predicate receives what the
// user would see at that point.
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
		View() tea.View
	}

	m, ok := fm.(viewer)
	require.True(a.t, ok, "final model does not implement View() tea.View")

	return m.View().Content
}

// FakeAPI is a configurable test double for api.Client. Each method
// delegates to the corresponding function field when set, falling back
// to a sensible default otherwise. The mutex protects concurrent access
// from model goroutines during teatest runs.
//
// SendEventsFn returns the raw [api.CompletionResult] so tests can
// drive either silence (the default — no tool calls) or specific
// tool-call patterns (the model "wants to say something" via the
// `msg`/`me`/`pass` tools).
type FakeAPI struct {
	mu                 sync.Mutex
	ListModelsFn       func(context.Context) ([]api.ModelInfo, error)
	SendEventsFn       func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	GenerateNickFn     func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
	GeneratePersonasFn func(context.Context, domain.ModelID) ([]domain.PersonaTemplate, error)
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

// RenderEventRequest returns the standard OpenRouter request shape
// used by the test client.
func (f *FakeAPI) RenderEventRequest(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	tools ...api.ToolDefinition,
) (api.RenderedEventRequest, error) {
	return api.RenderEventRequest(
		modelID, selfInstanceID, systemPrompt, history, events, tools...,
	)
}

// RenderToolResultRequest returns the standard OpenRouter request
// shape used by the test client.
func (f *FakeAPI) RenderToolResultRequest(
	conv *api.Conversation,
	results []api.ToolResult,
	tools ...api.ToolDefinition,
) (api.RenderedEventRequest, error) {
	if conv == nil {
		return api.RenderedEventRequest{}, nil
	}

	return api.RenderToolResultRequest(conv, results, tools...)
}

// SendEvents delegates to SendEventsFn or returns silence (no tool
// calls, which the dispatch loop terminates on).
func (f *FakeAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	_ domain.InstanceID,
	system api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.SendEventsFn != nil {
		return f.SendEventsFn(ctx, modelID, system.Text(), history.Messages(), events)
	}

	return api.CompletionResult{}, nil
}

// ContinueWithToolResults always returns silence — tests that want
// to drive multi-turn tool loops should set SendEventsFn to return
// the desired sequence directly.
func (f *FakeAPI) ContinueWithToolResults(
	_ context.Context,
	_ *api.Conversation,
	_ []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
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
func (f *FakeAPI) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.PersonaTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GeneratePersonasFn != nil {
		return f.GeneratePersonasFn(ctx, smallModel)
	}

	return nil, nil
}

// AddModel attaches a model instance to a channel through the
// wire path — the same dispatcher path the user-issued
// `/add-model` and the OPER tool call take. The user-client
// holds `+o` from bootstrap so the operator gate passes; tests
// that want to exercise the rejection path should `Send` the
// command from a non-OPER model-client directly.
func AddModel(t testing.TB, user *userclient.UserClient, channel domain.ChannelName, model domain.ModelID, persona string) {
	t.Helper()

	resp, err := user.Send(t.Context(), protocol.AddModel{
		Channel: channel,
		Model:   model,
		Persona: persona,
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
}

// Quit issues a wire-shaped `QUIT` through the user-client.
// Used by tests that simulate a clean previous-session shutdown
// to populate the autojoin list and clear the session-active
// marker, the same effect the chat-screen's `/quit` handler has.
func Quit(t testing.TB, user *userclient.UserClient, message string) {
	t.Helper()

	require.NoError(t, user.Quit(t.Context(), message))
}

// SeedChannel creates a channel by issuing a real JOIN through
// the user-client and pins it as the user's last-focused channel
// in the store, mirroring the state a returning user lands in:
// joined the channel and the chat screen treats it as last-active
// on startup. The resulting JoinEvent and friends remain on the
// user-client subscription's events stream so that a downstream
// ChatScreen drains and renders them when it takes over.
//
// Last-channel persistence is the chat-screen's responsibility
// (via the narrow `screens.UIStateStore` interface). Tests that
// need a specific channel restored at chat-screen Init time should
// construct the chat-screen with a store handle that satisfies
// `UIStateStore`; the no-uiState callers fall back to the chat-
// screen's "no-preference, first NAMES reply wins" rule, which is
// what the existing test pattern relies on.
//
// For integration tests that drive the ConnectionScreen and want
// to simulate "previous session" state, follow up with [Quit] +
// [DrainEvents] to leave the channel on the autojoin list without
// lingering membership.
func SeedChannel(t testing.TB, user *userclient.UserClient, name string) {
	t.Helper()

	require.NoError(t, user.Join(t.Context(), domain.ChannelName(name)))
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

	bot := seedbotFor(t, sess)
	bot.Instance().JoinChannel(domain.ChannelName(channel), time.Now())

	resp, err := bot.Send(t.Context(), protocol.PrivMsg{
		Target: protocol.ChannelTarget(channel),
		Body:   body,
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
}

// seedbots holds the seed client of each session a test has seeded
// through, so repeated [SeedMessage] calls speak as one client. A
// subscription belongs to the client that registered it, so a second
// client under "inst-seedbot" would be refused; and one identity
// keeps the nick a test renders the same from one seeded line to the
// next.
var seedbots sync.Map
var sessionStores sync.Map

func seedbotFor(t testing.TB, sess *session.Session) *testclient.TestClient {
	t.Helper()

	if existing, ok := seedbots.Load(sess); ok {
		bot, isBot := existing.(*testclient.TestClient)
		require.True(t, isBot)

		return bot
	}

	store, ok := sessionStores.Load(sess)
	require.True(t, ok, "test session has a registered store")

	bot := testclient.NewStored("seedbot", sess, store.(SessionStore), testclient.WithInstanceID("inst-seedbot"))
	require.NoError(t, bot.Attach(t.Context()))

	seedbots.Store(sess, bot)

	t.Cleanup(func() {
		seedbots.Delete(sess)
		bot.Detach()
	})

	return bot
}

// NewTestSession constructs the `*session.Session`,
// `*modelmanager.Manager`, and `*userclient.UserClient` trio
// chat-screen tests reach for. The manager owns the api / memory /
// tools handles; the session is wired to the manager as its
// factory; the user-client attaches to the session under the
// fixed "testuser" nick with `+o`. Cleanup hooks register so
// dispatch goroutines drain before the next test.
func NewTestSession(
	t testing.TB,
	store SessionStore,
	apiClient api.Client,
	memStore memory.Store,
	tools *modelclient.ToolRegistry,
	apiKey string,
	smallModel domain.ModelID,
	baseContext func() context.Context,
) (*session.Session, *modelmanager.Manager, *userclient.UserClient) {
	mgr := modelmanager.New(modelmanager.Config{
		Store:         store,
		APIClient:     apiClient,
		Memory:        memStore,
		Tools:         tools,
		BaseContext:   baseContext,
		InitialAPIKey: apiKey,
		SmallModel:    smallModel,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
	mgr.SetAPIFactory(func(apiKey, baseURL string) (api.Client, error) {
		_ = baseURL
		_ = apiKey
		return apiClient, nil
	})

	userCredential := protocol.NewUserCredential()
	sess := session.New(baseContext(), store, mgr, nil,
		session.WithUserCredential(userCredential))
	sessionStores.Store(sess, store)
	t.Cleanup(func() { sessionStores.Delete(sess) })
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, store,
		userclient.NewStoreReplyLog(store), userCredential)
	require.NoError(t, user.Attach(baseContext()))

	return sess, mgr, user
}

// SessionStore is the union of the session, manager and
// user-client persistence surfaces. The concrete
// `*storetest.MemoryStore` and `*store.SQLiteStore` satisfy it
// implicitly.
type SessionStore interface {
	session.Store
	modelmanager.Store
	userclient.Store
}

// NewModelManager returns a fresh [modelmanager.Manager] backed by
// the supplied dependencies and registered for cleanup. Pass the
// returned value as the `factory` argument to [session.New]; the
// concrete `*modelmanager.Manager` satisfies
// [session.ModelClientFactory].
func NewModelManager(
	t testing.TB,
	store modelmanager.Store,
	apiClient api.Client,
	memStore memory.Store,
	tools *modelclient.ToolRegistry,
	apiKey string,
	smallModel domain.ModelID,
	baseContext func() context.Context,
) *modelmanager.Manager {
	mgr := modelmanager.New(modelmanager.Config{
		Store:         store,
		APIClient:     apiClient,
		Memory:        memStore,
		Tools:         tools,
		BaseContext:   baseContext,
		InitialAPIKey: apiKey,
		SmallModel:    smallModel,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
	return mgr
}

// DrainEvents discards any buffered events on the user-client
// subscription's protocol bus. This prevents seed operations from
// leaking stale events into the UI when tests start.
func DrainEvents(user *userclient.UserClient) {
	for {
		select {
		case <-user.Events():
		default:
			return
		}
	}
}
