// Package main is the entry point for the modeloff TUI application.
package main

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens"
)

func main() {
	appCtx, cancelApp := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelApp()

	obs, err := observability.NewRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initialising observability: %v\n", err)
		os.Exit(1)
	}

	cfg, cfgStore, err := loadConfig(appCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	dataStore, err := store.NewDefaultSQLiteStore(appCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating data store: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = dataStore.Close() }()

	memStore, err := memory.NewDefaultStore(dataStore, cfg, cfgStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating memory store: %v\n", err)
		os.Exit(1)
	}

	apiClient := api.NewOpenRouterClient(cfg.APIKey, cfg.BaseURL, nil)

	sess := session.New(
		func() context.Context { return appCtx },
		dataStore,
		memStore,
		apiClient,
		domain.Nick(cfg.UserNick),
		cfg.APIKey,
		cfg.SmallModel,
	)
	sess.SetAPIFactory(func(apiKey, baseURL string) (api.Client, error) {
		return api.NewOpenRouterClient(apiKey, baseURL, nil), nil
	})

	toolRegistry, err := chatcmd.BuildToolRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building tool registry: %v\n", err)
		os.Exit(1)
	}

	sess.SetToolRegistry(toolRegistry)

	channelCount := 0

	if autojoin, err := dataStore.ListAutojoinChannels(appCtx); err == nil {
		channelCount = len(autojoin)
	}

	chatScreen, err := screens.NewChatScreen(appCtx, sess, cfgStore, domain.KindStatus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building command grammar: %v\n", err)
		os.Exit(1)
	}

	chatScreen = chatScreen.WithObservability(obs)

	connScreen := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    cfg.APIKey != "",
		ChannelCount: channelCount,
		Nick:         cfg.UserNick,
		Next:         chatScreen,
		Session:      sess,
		Ctx:          appCtx,
	})

	p := tea.NewProgram(
		ui.NewRoot(connScreen),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(appCtx),
	)

	go runPokeLoop(appCtx, p, cfgStore)

	_, runErr := p.Run()

	cancelApp()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	if err := sess.Shutdown(drainCtx); err != nil {
		slog.Warn("session shutdown timed out", "error", err)
	}
	drainCancel()

	if shutdownErr := obs.Shutdown(context.Background()); shutdownErr != nil {
		fmt.Fprintf(os.Stderr, "error shutting down observability: %v\n", shutdownErr)
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", runErr)
		os.Exit(1)
	}
}

func loadConfig(ctx context.Context) (config.Config, *config.FileStore, error) {
	cfgStore, err := config.NewDefaultFileStore()
	if err != nil {
		return config.Config{}, nil, err
	}

	cfg, err := cfgStore.Load(ctx)
	if err != nil {
		return config.Config{}, nil, err
	}

	return cfg, cfgStore, nil
}

func runPokeLoop(ctx context.Context, p *tea.Program, cfgStore config.Store) {
	for {
		cfg, err := cfgStore.Load(ctx)
		if err != nil || cfg.APIKey == "" || cfg.PokeInterval <= 0 {
			if !sleepOrDone(ctx, time.Minute) {
				return
			}

			continue
		}

		if !sleepOrDone(ctx, perturbDuration(cfg.PokeInterval)) {
			return
		}

		p.Send(screens.PokeTickMsg{})
	}
}

func perturbDuration(interval time.Duration) time.Duration {
	delta := interval / 10
	if delta <= 0 {
		return interval
	}

	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(delta*2)+1))
	if err != nil {
		return interval
	}

	offset := time.Duration(n.Int64()) - delta

	return interval + offset
}

func sleepOrDone(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
