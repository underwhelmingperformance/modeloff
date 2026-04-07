// Package main is the entry point for the modeloff TUI application.
package main

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	"os"
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
	"github.com/laney/modeloff/internal/ui/screens"
)

func main() {
	ctx := context.Background()

	cfg, cfgStore, err := loadConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	dataStore, err := store.NewDefaultSQLiteStore()
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

	obs, err := observability.NewRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initialising observability: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if shutdownErr := obs.Shutdown(context.Background()); shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "error shutting down observability: %v\n", shutdownErr)
		}
	}()

	_, searchEnabled := memStore.(memory.Searcher)
	apiClient := api.NewOpenRouterClient(cfg.APIKey, cfg.BaseURL, nil, searchEnabled)

	sess := session.New(
		dataStore,
		memStore,
		apiClient,
		cfgStore,
		domain.Nick(cfg.UserNick),
	)
	sess.SetAPIFactory(func(c config.Config) (api.Client, error) {
		_, search := memStore.(memory.Searcher)
		return api.NewOpenRouterClient(c.APIKey, c.BaseURL, nil, search), nil
	})

	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()

	channelCount := 0

	channels, err := dataStore.ListChannels(appCtx)
	if err == nil {
		channelCount = len(channels)
	}

	chatScreen := screens.NewChatScreen(appCtx, sess).WithObservability(obs)

	connScreen := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    cfg.APIKey != "",
		ChannelCount: channelCount,
		Nick:         cfg.UserNick,
		Next:         chatScreen,
	})

	p := tea.NewProgram(
		ui.NewRoot(connScreen),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	go runPokeLoop(appCtx, p, cfgStore)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
