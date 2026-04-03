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
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
)

func main() {
	cfg, cfgStore, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	dataStore, err := store.NewDefaultFileStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating data store: %v\n", err)
		os.Exit(1)
	}

	memStore, err := memory.NewDefaultFileStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating memory store: %v\n", err)
		os.Exit(1)
	}

	apiClient := api.NewOpenRouterClient(cfg.APIKey, "", nil)

	sess := session.New(
		dataStore,
		memStore,
		apiClient,
		cfgStore,
		domain.Nick(cfg.UserNick),
	)

	channelCount := 0

	channels, err := dataStore.ListChannels(context.Background())
	if err == nil {
		channelCount = len(channels)
	}

	chatScreen := screens.NewChatScreen(sess)

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

	pokeCtx, cancelPokes := context.WithCancel(context.Background())
	defer cancelPokes()

	go runPokeLoop(pokeCtx, p, cfgStore)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig() (config.Config, *config.FileStore, error) {
	cfgStore, err := config.NewDefaultFileStore()
	if err != nil {
		return config.Config{}, nil, err
	}

	cfg, err := cfgStore.Load()
	if err != nil {
		return config.Config{}, nil, err
	}

	return cfg, cfgStore, nil
}

func runPokeLoop(ctx context.Context, p *tea.Program, cfgStore config.Store) {
	for {
		cfg, err := cfgStore.Load()
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
