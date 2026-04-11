package ui_test

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/laney/modeloff/internal/observability"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestApp_observability_drawer_and_fullscreen_with_teatest(t *testing.T) {
	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, obs.Shutdown(context.Background()))
	}()

	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore).WithObservability(obs)))
	tm.Send(tea.WindowSizeMsg{Width: 140, Height: 30})
	tm.WaitFor("#general")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlL})
	tm.WaitFor("Logs", "Metrics")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlF})
	tm.WaitFor("Metrics", "Tab", "Esc", "obs logs")

	view := ansi.Strip(tm.CurrentView())
	require.NotContains(t, view, "testuser >")
}

func TestApp_status_bar_shows_metrics_summary_with_teatest(t *testing.T) {
	obs, err := observability.NewRuntime()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, obs.Shutdown(context.Background()))
	}()

	recordUsageSpan()

	sess, _, cfgStore := newIntegrationSession(t, &integrationAPI{})
	uitest.SeedChannel(t, sess, "#general")

	tm := uitest.New(t, uipkg.NewRoot(screens.NewChatScreen(t.Context(), sess, cfgStore).WithObservability(obs)))
	tm.Send(tea.WindowSizeMsg{Width: 200, Height: 30})
	tm.WaitFor("#general")
	tm.WaitFor("req 1", "in 12", "out 8", "0.2500")

	view := ansi.Strip(tm.CurrentView())
	require.Contains(t, view, "req 1")
	require.Contains(t, view, "in 12")
	require.Contains(t, view, "out 8")
	require.Contains(t, view, "0.2500")
}

func recordUsageSpan() {
	ctx, span := otel.Tracer("test").Start(context.Background(), "api.openrouter.send_events")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.send_events"),
		attribute.String(observability.AttrModelID, "anthropic/claude-3-haiku"),
		attribute.String(observability.AttrResult, observability.ResultReply),
		attribute.Int64(observability.AttrPromptTokens, 12),
		attribute.Int64(observability.AttrCompletionTokens, 8),
		attribute.Float64(observability.AttrCostCredits, 0.25),
	)
	span.End()

	_ = ctx
}
