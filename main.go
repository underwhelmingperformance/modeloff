// Package main is the entry point for the modeloff TUI application.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
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

		// The vector index lives beside the SQLite database, under
		// the same "modeloff" data directory, but is a chromem-go
		// directory tree, not a single file store.Wipe knows how to
		// remove. It holds memory content in cleartext metadata, so
		// --wipe's promise to remove memories is not met while this
		// directory survives.
		if err := os.RemoveAll(memory.DefaultIndexDir()); err != nil {
			fmt.Fprintf(os.Stderr, "error wiping memory index: %v\n", err)
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

	memStore, err := memory.NewDefaultStore(appCtx, dataStore, cfg, cfgStore)
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

	defaultChannelModes, err := defaultChannelModesFromConfig(cfg, cfgStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading default channel modes: %v\n", err)
		os.Exit(1)
	}

	sess := session.New(baseContext, dataStore, mgr, defaultChannelModes)

	user := userclient.New(domain.Nick(cfg.UserNick), sess, dataStore, userclient.NewStoreReplyLog(dataStore))
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

	// A `/config poke-interval` edit or a freshly-set API key changes
	// what the schedule reports; WakePoke interrupts the scheduler's
	// current sleep so the change takes effect on this cycle instead
	// of only once that sleep runs out.
	cfgStore.OnChange(func(prev, curr config.Config) {
		if prev.PokeInterval == curr.PokeInterval && prev.APIKey == curr.APIKey {
			return
		}

		sess.WakePoke()
	})

	_, runErr := p.Run()

	cancelApp()

	// Cancelling the app context wakes every dispatch goroutine;
	// DetachAll is what waits for them — the models still attached
	// and the ones already draining from a QUIT or KILL alike — so no
	// model turn is still running when the session closes its
	// subscriptions underneath it.
	mgr.DetachAll()

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

// loadConfig loads the persisted configuration. A config.json that
// exists but fails to parse would otherwise leave the application
// refusing to start, with no way to recover short of deleting the
// file by hand. loadConfig instead moves it aside to a timestamped
// backup and retries, so the application starts with defaults and
// the user keeps the original file to inspect or restore from.
func loadConfig(ctx context.Context) (config.Config, *config.FileStore, error) {
	cfgStore, err := config.NewDefaultFileStore()
	if err != nil {
		return config.Config{}, nil, err
	}

	cfg, err := cfgStore.Load(ctx)
	if err == nil {
		return cfg, cfgStore, nil
	}

	// Only a genuine decode failure (a corrupt config.json) is worth
	// recovering from by moving the file aside. A read failure for
	// some other reason, a permissions problem for instance, would
	// still fail identically after a rename, and the rename itself
	// would discard a config file that was never actually corrupt.
	if !errors.Is(err, config.ErrCorrupt) {
		return config.Config{}, nil, err
	}

	backup, recoverErr := cfgStore.RecoverCorrupt()
	if recoverErr != nil || backup == "" {
		return config.Config{}, nil, err
	}

	fmt.Fprintf(os.Stderr,
		"warning: config file at %s could not be read (%v); it has been backed up to %s and defaults will be used\n",
		cfgStore.Path(), err, backup,
	)
	slog.Default().WarnContext(ctx, "config file was unreadable, recovered with defaults",
		"component", "main",
		"path", cfgStore.Path(),
		"backup_path", backup,
		"error", err,
	)

	cfg, err = cfgStore.Load(ctx)
	if err != nil {
		return config.Config{}, nil, err
	}

	return cfg, cfgStore, nil
}

// channelModesHolder keeps the parsed default channel modes the
// session reads when a JOIN creates a channel.
//
// The session performs that read on its command loop, where every
// other client's command waits behind it, so the read must not touch
// the config file. Parsing happens in `store` instead, called once at
// startup and again from the config-change listener, both off the
// loop.
type channelModesHolder struct {
	parsed   atomic.Pointer[domain.ChannelModes]
	fallback domain.ChannelModes
}

// store parses `spec` and keeps the result for later reads. An
// unparseable spec logs and keeps the built-in default: `/config`
// validates what it writes, but config.json is hand-editable, and
// refusing to create the channel would leave the user unable to join
// anything until they had repaired the file by hand.
func (h *channelModesHolder) store(spec string) {
	parsed, err := domain.ParseChannelModes(spec)
	if err != nil {
		slog.Warn("parse configured default channel modes, using built-in default",
			"component", "main",
			"configured", spec,
			"default_modes", config.DefaultChannelModesSpec,
			"error", err,
		)

		parsed = h.fallback
	}

	h.parsed.Store(&parsed)
}

// read returns the modes last stored. It satisfies
// [session.DefaultChannelModes] and costs one atomic load, so the
// session's command loop never waits on it.
func (h *channelModesHolder) read(context.Context) domain.ChannelModes {
	return *h.parsed.Load()
}

// defaultChannelModesFromConfig builds the session's
// [session.DefaultChannelModes] from `cfg`, and subscribes to
// `cfgStore` so that a `/config default-modes` edit is reparsed as it
// is saved. The next channel created then starts on the new value,
// without a restart and without the session reading the config file
// itself.
//
// The built-in default is parsed once up front, since it is the
// fallback every later parse failure lands on. A build whose own
// compiled-in default does not parse is a programming error worth
// refusing to start on, and is the only error returned here.
func defaultChannelModesFromConfig(cfg config.Config, cfgStore config.Store) (session.DefaultChannelModes, error) {
	fallback, err := domain.ParseChannelModes(config.DefaultChannelModesSpec)
	if err != nil {
		return nil, fmt.Errorf("parse built-in default channel modes %q: %w", config.DefaultChannelModesSpec, err)
	}

	holder := &channelModesHolder{fallback: fallback}
	holder.store(cfg.DefaultChannelModes)

	cfgStore.OnChange(func(prev, curr config.Config) {
		if prev.DefaultChannelModes == curr.DefaultChannelModes {
			return
		}

		holder.store(curr.DefaultChannelModes)
	})

	return holder.read, nil
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
