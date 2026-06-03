// Package main is the entry point for the modeloff TUI application.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/userclient"
)

func main() {
	wipe := flag.Bool("wipe", false, "Remove the on-disk database (channels, instances, memories, personas, autojoin) before starting. The config file is left untouched.")

	flag.Parse()

	appCtx, cancelApp := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelApp()

	if *wipe {
		if err := store.Wipe(store.DefaultSQLitePath()); err != nil {
			fmt.Fprintf(os.Stderr, "error wiping database: %v\n", err)
			os.Exit(1)
		}
	}

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

	baseContext := func() context.Context { return appCtx }

	toolRegistry, err := chatcmd.BuildToolRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building tool registry: %v\n", err)
		os.Exit(1)
	}

	mgr := modelmanager.New(modelmanager.Config{
		Store:     dataStore,
		Memory:    memStore,
		APIClient: apiClient,
		APIFactory: func(apiKey, baseURL string) (api.Client, error) {
			return api.NewOpenRouterClient(apiKey, baseURL, nil), nil
		},
		InitialAPIKey: cfg.APIKey,
		SmallModel:    cfg.SmallModel,
		Tools:         toolRegistry,
		BaseContext:   baseContext,
	})

	sess := session.New(baseContext, dataStore, mgr)

	user := userclient.New(domain.Nick(cfg.UserNick), sess, dataStore)
	if err := user.Attach(appCtx); err != nil {
		fmt.Fprintf(os.Stderr, "error attaching user client: %v\n", err)
		os.Exit(1)
	}

	if err := mgr.Start(appCtx, sess); err != nil {
		slog.Warn("attach boot model clients", "error", err)
	}

	channelCount := 0

	if autojoin, err := dataStore.ListAutojoinChannels(appCtx); err == nil {
		channelCount = len(autojoin)
	}

	chatScreen, err := screens.NewChatScreen(baseContext, sess, mgr, user, cfgStore, dataStore, domain.KindStatus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building command grammar: %v\n", err)
		os.Exit(1)
	}

	chatScreen = chatScreen.WithObservability(obs)

	connScreen := screens.NewConnectionScreen(screens.ConnectionConfig{
		HasAPIKey:    cfg.APIKey != "",
		ChannelCount: channelCount,
		Nick:         cfg.UserNick,
		Session:      sess,
		Manager:      mgr,
		User:         user,
		BaseContext:  baseContext,
	}, chatScreen)

	p := tea.NewProgram(
		ui.NewRoot(connScreen),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(appCtx),
	)

	sess.StartPoking(appCtx, pokeScheduleFromConfig(cfgStore))

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

// pokeScheduleFromConfig adapts the persisted config into the
// session's [session.PokeSchedule]: poking is enabled once an API key
// is set and the interval is positive, and the live value is re-read
// each cycle so `/config poke-interval` takes effect without a
// restart.
func pokeScheduleFromConfig(cfgStore config.Store) session.PokeSchedule {
	return func(ctx context.Context) (time.Duration, bool) {
		cfg, err := cfgStore.Load(ctx)
		if err != nil || cfg.APIKey == "" || cfg.PokeInterval <= 0 {
			return 0, false
		}

		return cfg.PokeInterval, true
	}
}
