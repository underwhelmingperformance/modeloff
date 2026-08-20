package screens

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// liveModelsLoadedMsg carries the OpenRouter catalogue a `ListModels`
// call returned, which `/add-model` completion offers.
type liveModelsLoadedMsg struct {
	models []chatcmd.ModelOption
}

// liveModelsLoadFailedMsg is dispatched when `ListModels` fails. It
// carries the underlying error; the handler empties the live-model
// cache to degrade tab-completion gracefully, treats
// `modelclient.ErrNoAPIKey` as a silent no-op, and surfaces other
// failures as a `SystemNotice`.
type liveModelsLoadFailedMsg struct {
	err error
}

// routeCatalogue answers a model-catalogue load. The catalogue is
// completion state, so both outcomes republish the completion set;
// only a failure the user can act on also renders a line.
func (s ChatScreen) routeCatalogue(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case liveModelsLoadedMsg:
		next, cmd := s.handleLiveModelsLoaded(msg)
		return next, cmd, true

	case liveModelsLoadFailedMsg:
		next, cmd := s.handleLiveModelsLoadFailed(msg)
		return next, cmd, true
	}

	return s, nil, false
}

func (s ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ChatScreen, tea.Cmd) {
	s, rebind := s.setLiveModels(msg.models, command.SuggestionStateReady)

	if s.realChannelCount() == 0 {
		return s, tea.Batch(rebind, msgCmd(components.SetPlaceholderMsg{Text: s.checklist.Render()}))
	}

	return s, rebind
}

// handleLiveModelsLoadFailed is the UI-policy home for live-model
// load failures. When `s.active` is empty — no real channel
// joined yet — the notice is routed to `&modeloff`, the
// chat-screen-owned default landing window.
func (s ChatScreen) handleLiveModelsLoadFailed(msg liveModelsLoadFailedMsg) (ChatScreen, tea.Cmd) {
	// ErrNoAPIKey here is a TOCTOU between loadLiveModels' HasAPIKey
	// short-circuit and Session.ListModels' check; treat as silent.
	if errors.Is(msg.err, modelclient.ErrNoAPIKey) {
		return s.setLiveModels(nil, command.SuggestionStateReady)
	}

	s, rebind := s.setLiveModels(nil, command.SuggestionStateError)

	channel := s.active
	if channel == "" {
		channel = domain.StatusChannelName
	}

	slog.Default().WarnContext(s.baseContext(), "live models load failed",
		"component", "ui",
		"channel", string(channel),
		"error", msg.err,
	)

	return s, tea.Batch(rebind, s.logAndShowOn(channel, domain.SystemNotice{
		Target: channel,
		Text:   fmt.Sprintf("Model list unavailable: %s.", msg.err),
		At:     time.Now(),
	}))
}

// setLiveModels replaces the cached OpenRouter model catalogue and
// the suggestion state derived from the load that produced it,
// returning the completer rebind that publishes both to the input
// bar's popover.
func (s ChatScreen) setLiveModels(models []chatcmd.ModelOption, state command.SuggestionState) (ChatScreen, tea.Cmd) {
	s.liveModels = models
	s.liveModelsState = state
	s.checklist.modelCount = len(models)

	return s, s.rebindCompleter()
}

func (s ChatScreen) loadLiveModels() tea.Cmd {
	if !s.mgr.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		models, err := s.mgr.ListModels(s.baseContext())
		if err != nil {
			return liveModelsLoadFailedMsg{err: err}
		}

		options := make([]chatcmd.ModelOption, 0, len(models))
		for _, model := range models {
			options = append(options, chatcmd.ModelOption{
				ID:          model.ID,
				Name:        model.Name,
				Description: model.Description,
			})
		}

		return liveModelsLoadedMsg{models: options}
	}
}

// ensurePersonas seeds the persona pool in the background so the
// next `--persona` tab-completion has something to offer. Best-
// effort: a failure is logged and discarded, since the persona pool
// is not required for any user-visible flow except completion.
func (s ChatScreen) ensurePersonas() tea.Cmd {
	if !s.mgr.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.mgr.EnsurePersonas(ctx); err != nil {
			slog.Default().WarnContext(ctx, "ensure personas",
				"component", "ui",
				"screen", "chat",
				"error", err,
			)
		}

		return nil
	}
}
