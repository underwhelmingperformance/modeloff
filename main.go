// Package main is the entry point for the modeloff TUI application.
package main

import (
	"context"
	"fmt"
	"os"

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

	roomCount := 0

	rooms, err := dataStore.ListRooms(context.Background())
	if err == nil {
		roomCount = len(rooms)
	}

	placeholder := screens.NewPlaceholderScreen(cfg.UserNick)

	connScreen := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey: cfg.APIKey != "",
		RoomCount: roomCount,
		Nick:      cfg.UserNick,
		Next:      placeholder,
	})

	_ = sess // Session will be used by future screens.

	p := tea.NewProgram(
		ui.NewRoot(connScreen),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

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
