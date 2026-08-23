package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrg/xdg"
	_ "github.com/ncruces/go-sqlite3/driver" // SQLite driver.
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

const schema = `
CREATE TABLE IF NOT EXISTS channels (
    name TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY,
    channel TEXT NOT NULL,
    type    TEXT NOT NULL,
    data    TEXT NOT NULL,
    at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_channel_id
    ON events (channel, id);

CREATE TABLE IF NOT EXISTS instance_replies (
    id          INTEGER PRIMARY KEY,
    instance_id TEXT NOT NULL,
    type        TEXT NOT NULL,
    data        TEXT NOT NULL,
    at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_instance_replies_instance_id
    ON instance_replies (instance_id, id);

CREATE TABLE IF NOT EXISTS instances (
    instance_id TEXT PRIMARY KEY,
    nick        TEXT NOT NULL,
    data        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_instances_nick
    ON instances (nick);

CREATE TABLE IF NOT EXISTS memories (
    instance_id TEXT NOT NULL,
    key         TEXT NOT NULL,
    content     TEXT NOT NULL,
    PRIMARY KEY (instance_id, key)
);

CREATE TABLE IF NOT EXISTS personas (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    origin      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS autojoin (
    name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS last_read (
    channel  TEXT PRIMARY KEY REFERENCES channels(name) ON DELETE CASCADE,
    event_id INTEGER NOT NULL REFERENCES events(id)
);

INSERT OR IGNORE INTO state (key, value) VALUES ('schema_version', '1');
`

// SQLiteStore implements Store using a single SQLite database. It
// also owns the canonical `*domain.Instance` handle per InstanceID:
// the `instances` field caches every instance ever loaded or saved
// through this store, so callers see pointer-stable handles across
// calls. The registry is invalidated on `DeleteInstanceByID` and
// `Reset`.
type SQLiteStore struct {
	db *sql.DB

	instancesMu sync.RWMutex
	instances   map[domain.InstanceID]*domain.Instance

	// tracerProvider is the OTel `TracerProvider` the store uses for
	// its spans. Defaults to `otel.GetTracerProvider()`; tests inject
	// a per-test recorder via `WithTracerProvider` so span recordings
	// stay scoped to a single test.
	tracerProvider trace.TracerProvider
}

// SQLitePragmaDSN appends the connection-time PRAGMAs that the store
// requires (`busy_timeout`, `auto_vacuum`, `journal_mode`,
// `foreign_keys`) to the given filename or `file:` URI. The
// `ncruces/go-sqlite3` driver applies `_pragma=` parameters on every
// connection it opens, so any pool size sees the same configuration.
//
// Order matters, and not only for the reason the driver's own docs
// give (`busy_timeout` and the locking mode must come first):
// `auto_vacuum` also has to precede `journal_mode`. SQLite only
// accepts an `auto_vacuum` change on a database with no pages
// allocated yet, and switching to WAL allocates one, so setting
// `auto_vacuum` after `journal_mode(WAL)` silently does nothing.
//
// A brand-new database is created in incremental-vacuum mode by this
// pragma. `auto_vacuum` only changes an empty database, so an
// existing database predates the pragma and stays whatever mode it
// was created in; this is a silent no-op against one that already
// has a schema. [SQLiteStore.pruneEvents] runs
// `PRAGMA incremental_vacuum` after its retention pass, which only
// does anything for a database that is actually in incremental-vacuum
// mode.
func SQLitePragmaDSN(path string) string {
	dsn := path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return dsn + sep + "_pragma=busy_timeout(5000)&_pragma=auto_vacuum(incremental)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
}

// DefaultSQLitePath returns the on-disk location of the default
// SQLite database — $XDG_DATA_HOME/modeloff/modeloff.db. It is the
// same path [NewDefaultSQLiteStore] opens, exposed so callers
// (e.g. a `--wipe` startup flag) can act on the file without
// reopening it.
func DefaultSQLitePath() string {
	return filepath.Join(xdg.DataHome, "modeloff", "modeloff.db")
}

// Wipe removes the SQLite database file at base together with its
// WAL and SHM sidecars (`base-wal`, `base-shm`). Missing files are
// not an error: wiping a never-launched install is a no-op. After
// Wipe, opening a [SQLiteStore] at the same path creates a fresh
// schema.
func Wipe(base string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(base + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", base+suffix, err)
		}
	}

	return nil
}

// maxSQLiteConns bounds the connection pool [NewDefaultSQLiteStore]
// opens. The `ncruces/go-sqlite3` driver backs every connection with
// its own wazero WASM instance, so an unbounded pool spins up one
// such instance per concurrent caller; this is a single-user desktop
// application, so a small fixed pool comfortably covers the handful
// of goroutines (model dispatch turns, the chat-screen) that touch
// the store at once without growing unbounded under a burst.
const maxSQLiteConns = 4

// NewDefaultSQLiteStore creates a SQLiteStore using the XDG data
// directory ($XDG_DATA_HOME/modeloff/modeloff.db).
func NewDefaultSQLiteStore(ctx context.Context) (*SQLiteStore, error) {
	path := DefaultSQLitePath()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(maxSQLiteConns)
	db.SetMaxIdleConns(maxSQLiteConns)

	return NewSQLiteStore(ctx, db)
}

// NewSQLiteStore creates a store backed by the given database. The
// caller is responsible for opening the database with a DSN that
// configures the required connection-time PRAGMAs (`busy_timeout`,
// `journal_mode`, `foreign_keys`); `SQLitePragmaDSN` builds one.
// The schema is created on first open; subsequent opens are no-ops
// thanks to `CREATE TABLE IF NOT EXISTS`. Existing databases whose
// recorded schema version differs from [SchemaVersion] are
// reconciled through [applyMigrations].
func NewSQLiteStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	// Read the resulting journal mode for operator visibility — an
	// on-disk database normally reports `wal` here, but a `:memory:`
	// database reports `memory` because there is no file to journal,
	// and a filesystem that cannot host WAL falls back to `delete`.
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("read journal_mode: %w", err)
	}

	slog.Default().InfoContext(ctx, "sqlite journal mode",
		"component", "store.sqlite",
		"mode", journalMode,
	)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	if err := applyMigrations(ctx, db); err != nil {
		return nil, err
	}

	store := &SQLiteStore{
		db:             db,
		instances:      make(map[domain.InstanceID]*domain.Instance),
		tracerProvider: otel.GetTracerProvider(),
	}

	// Retention is disk hygiene, not a correctness requirement the
	// rest of the store depends on, so a failure here is logged
	// rather than turned into a failed open. See pruneEvents for why
	// it runs here, once, rather than on a recurring schedule.
	if err := store.pruneEvents(ctx); err != nil {
		slog.Default().ErrorContext(ctx, "prune events", "component", "store.sqlite", "error", err)
	}

	return store, nil
}

// WithTracerProvider overrides the OTel `TracerProvider` the store
// uses for its spans. Tests inject a per-test recorder so span
// recordings stay scoped to a single test rather than relying on the
// global provider's swap-and-restore. Production code does not need
// to call this — the default global provider is already correct.
func (s *SQLiteStore) WithTracerProvider(tp trace.TracerProvider) *SQLiteStore {
	s.tracerProvider = tp

	return s
}

// canonicaliseInstance returns the canonical `*domain.Instance` for
// the given id. On cache miss the freshly-loaded handle is inserted
// and returned. On cache hit the existing handle is returned
// untouched — the session is authoritative for the live nick,
// persona, and channels of every registered instance; the store row
// is a save-time snapshot and refreshing the cached handle from it
// would clobber unrelated in-flight mutations on the session side.
//
// Callers needing the on-disk row's display state must treat the
// returned handle's getters as authoritative and accept that the
// row may be staler.
func (s *SQLiteStore) canonicaliseInstance(fresh *domain.Instance) *domain.Instance {
	if fresh == nil {
		return nil
	}

	s.instancesMu.Lock()
	defer s.instancesMu.Unlock()

	if existing, ok := s.instances[fresh.ID()]; ok {
		return existing
	}

	s.instances[fresh.ID()] = fresh
	return fresh
}

// forgetInstance evicts an instance from the canonical registry.
// Subsequent loads that produce an Instance with the same id will
// return a fresh pointer — callers that held the old pointer see
// a stale handle, which is the correct semantic for a deleted
// instance.
func (s *SQLiteStore) forgetInstance(id domain.InstanceID) {
	s.instancesMu.Lock()
	delete(s.instances, id)
	s.instancesMu.Unlock()
}

// resolveInstance looks up the canonical handle for an id without
// touching the database. Returns nil if the id is not registered.
// Used by channel deserialisation to rewrite member-list stubs.
func (s *SQLiteStore) resolveInstance(id domain.InstanceID) *domain.Instance {
	s.instancesMu.RLock()
	defer s.instancesMu.RUnlock()

	return s.instances[id]
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// channelRow is the on-disk JSON shape of a row in the `channels`
// table. It is a persistence detail of the SQLite store and never
// leaves the package: callers receive the typed concrete
// `*StatusWindow` or `*ChannelWindow` constructed from
// the row by `rowToWindow`. Per-kind state that doesn't apply to a
// given row is left zero (a status row carries no members or
// topic; a DM row's member list is empty and `Name` is the
// counterpart's `InstanceID`).
//
// `Invitations` is written under its own key, so a row written when
// the set was keyed by nick reads as empty. That is the right
// outcome: those rows hold nicks, which the `InstanceID`-keyed field
// cannot accept, and an invitation is short-lived state that dies
// with its channel anyway (RFC 2811 §2), so there is nothing worth
// translating forward.
type channelRow struct {
	Name        domain.ChannelName
	Kind        domain.ChannelKind
	Topic       string
	TopicSetBy  domain.Nick
	TopicSetAt  time.Time
	Members     domain.MemberList
	Modes       domain.ChannelModes
	Invitations domain.Invitations
	Created     time.Time
}

// resolveChannelMembers checks the identities in a channel member
// list against the instance registry, loading any missing instances
// from SQLite in a single batch. Member rows that refer
// to an instance with no backing row are dropped from the list and
// logged — a leftover from a previous session where the instance
// was deleted but the channel's membership record still carried
// the id. Only channel-kind rows carry members; status and DM rows
// short-circuit at the empty-list check.
func (s *SQLiteStore) resolveChannelMembers(ctx context.Context, row *channelRow) error {
	if row.Members.Len() == 0 {
		return nil
	}

	// Gather ids that are not already in the registry. Every client
	// has an instances row, so the empty id
	// the user-client registers under is looked up like any other.
	var missing []domain.InstanceID
	seen := make(map[domain.InstanceID]struct{})

	for m := range row.Members.All() {
		id := m.InstanceID

		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}

		if s.resolveInstance(id) != nil {
			continue
		}

		missing = append(missing, id)
	}

	if len(missing) > 0 {
		if err := s.loadInstancesByID(ctx, missing); err != nil {
			return fmt.Errorf("load channel members: %w", err)
		}
	}

	// Any id that still resolves to nil references a deleted instance
	// and is dropped.
	dropped := make([]domain.InstanceID, 0)
	for member := range row.Members.All() {
		if s.resolveInstance(member.InstanceID) == nil {
			dropped = append(dropped, member.InstanceID)
		}
	}
	for _, id := range dropped {
		row.Members.RemoveID(id)
	}

	if len(dropped) > 0 {
		slog.Default().WarnContext(ctx,
			"channel members have no backing instance; dropped",
			"component", "store.sqlite",
			"channel", row.Name,
			"dropped_ids", dropped,
			"count", len(dropped),
		)
	}

	return nil
}

// loadInstancesByID reads the given ids from the `instances` table
// in a single query and registers them in the canonical registry.
// Ids that don't resolve are silently ignored — the caller detects
// the miss via a second `resolveInstance` lookup.
func (s *SQLiteStore) loadInstancesByID(ctx context.Context, ids []domain.InstanceID) error {
	if len(ids) == 0 {
		return nil
	}

	// Pass the id list as a JSON array bound to a single parameter
	// and let SQLite expand it via `json_each`. This avoids building
	// an IN (?, ?, …) list at the string level while still binding
	// the id values through a prepared-statement parameter.
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("marshal ids: %w", err)
	}

	fresh, err := queryRows(ctx, s.db, `
		SELECT data FROM instances
		WHERE pending_deletion = 0
		  AND instance_id IN (SELECT value FROM json_each(?))
	`, []any{string(idsJSON)}, jsonColumn[domain.Instance])
	if err != nil {
		return fmt.Errorf("load instances: %w", err)
	}

	for i := range fresh {
		s.canonicaliseInstance(&fresh[i])
	}

	return nil
}

// rowToWindow projects a decoded on-disk row to its matching
// concrete [domain.Window]. Legacy DM rows are ignored because
// direct-message windows are client-owned UI state.
func (s *SQLiteStore) rowToWindow(row channelRow) (domain.Window, error) {
	switch row.Kind {
	case domain.KindStatus:
		return domain.NewStatusWindow(row.Created), nil

	case domain.KindChannel:
		cw := domain.NewChannelWindow(row.Name, row.Created)
		cw.Topic = row.Topic
		cw.TopicSetBy = row.TopicSetBy
		cw.TopicSetAt = row.TopicSetAt
		cw.Members = row.Members
		cw.Modes = row.Modes
		cw.Invitations = row.Invitations
		return cw, nil

	case domain.KindDM:
		return nil, domain.MissingDMCounterpartError{InstanceID: domain.InstanceID(row.Name)}

	default:
		return nil, domain.UnknownChannelKindError{Kind: row.Kind}
	}
}

// rowFromWindow projects a window to its on-disk row form. Per-kind
// state that doesn't apply to the source kind is left zero: a status
// row carries no members or topic; a DM row's `Name` is the
// counterpart's `InstanceID` and the member list is empty.
func rowFromWindow(w domain.Window) channelRow {
	row := channelRow{
		Name:    w.Name(),
		Kind:    w.Kind(),
		Created: w.Created(),
	}

	if cw, ok := w.(*domain.ChannelWindow); ok {
		row.Topic = cw.Topic
		row.TopicSetBy = cw.TopicSetBy
		row.TopicSetAt = cw.TopicSetAt
		row.Members = cw.Members
		row.Modes = cw.Modes
		row.Invitations = cw.Invitations
	}

	return row
}

// ListWindows implements Store. The returned slice carries one
// concrete `Window` per channel or status row in `channels`. Legacy
// DM rows are dropped and logged because direct-message windows are
// client-owned UI state.
func (s *SQLiteStore) ListWindows(ctx context.Context) ([]domain.Window, error) {
	var windows []domain.Window
	err := s.inSpan(ctx, "store.sqlite.list_windows", nil, func(ctx context.Context, _ trace.Span) error {
		rows, err := queryRows(ctx, s.db,
			`SELECT data FROM channels ORDER BY name`, nil,
			jsonColumn[channelRow])
		if err != nil {
			return err
		}

		windows = make([]domain.Window, 0, len(rows))

		for i := range rows {
			if err := s.resolveChannelMembers(ctx, &rows[i]); err != nil {
				return err
			}

			w, err := s.rowToWindow(rows[i])
			if err != nil {
				// `MissingDMCounterpartError` is the expected race
				// (instance row deleted before the DM row); log as
				// a warning. Anything else (an unknown kind, say)
				// is a data-integrity break and propagates so the
				// caller can decide how to react.
				var missing domain.MissingDMCounterpartError
				if !errors.As(err, &missing) {
					return err
				}

				slog.Default().WarnContext(ctx,
					"window from row; dropped",
					"component", "store.sqlite",
					"channel", rows[i].Name,
					"kind", rows[i].Kind,
					"error", err,
				)

				continue
			}

			windows = append(windows, w)
		}

		return nil
	})

	return windows, err
}

// GetWindow implements Store. It returns the concrete channel or
// status window for the given name. A DM row returns
// [domain.MissingDMCounterpartError], because direct-message windows
// are client state and [SQLiteStore.SaveWindow] refuses to persist
// one.
//
// The name is matched under NOCASE, which folds `A`-`Z` onto `a`-`z`
// and nothing else, the same fold [domain.KeyForChannel] applies, so
// a lookup here and a lookup in the session's live channel state
// agree. The row keeps the spelling it was saved with, and that is
// the name the returned window carries.
//
// `ORDER BY name LIMIT 1` settles a database written before the
// casemapping existed, which may hold both `#Dev` and `#dev` as
// separate rows: the same one of the pair answers every lookup, so
// the channel behaves as one channel from here on.
func (s *SQLiteStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	var w domain.Window
	err := s.inSpan(ctx, "store.sqlite.get_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(name))},
		func(ctx context.Context, _ trace.Span) error {
			row, err := queryRow(ctx, s.db,
				`SELECT data FROM channels WHERE name = ? COLLATE NOCASE ORDER BY name LIMIT 1`,
				[]any{name}, ErrNoSuchChannel,
				jsonColumn[channelRow])
			if err != nil {
				return fmt.Errorf("channel %q: %w", name, err)
			}

			if err := s.resolveChannelMembers(ctx, &row); err != nil {
				return err
			}

			w, err = s.rowToWindow(row)
			return err
		})

	return w, err
}

// SaveWindow implements Store. DM windows are rejected; DMs
// are not persisted.
func (s *SQLiteStore) SaveWindow(ctx context.Context, w domain.Window) error {
	if w.Kind() == domain.KindDM {
		return fmt.Errorf("store: refusing to persist a DM window for %q; DMs are in-memory UI state", w.Name())
	}

	return s.inSpan(ctx, "store.sqlite.save_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(w.Name()))},
		func(ctx context.Context, _ trace.Span) error {
			data, err := json.Marshal(rowFromWindow(w))
			if err != nil {
				return err
			}

			return execMutation(ctx, s.db,
				`INSERT INTO channels (name, data) VALUES (?, ?)
				 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
				w.Name(), string(data))
		})
}

// ChannelJoin is the durable state transition for one successful JOIN.
// Scrollback contains each replay-capable recipient's projected JOIN.
// ResetContext removes data from an earlier membership interval. An invited
// turn remains valid across the JOIN that consumes its invitation.
type ChannelJoin struct {
	Window        *domain.ChannelWindow
	Instance      *domain.Instance
	Event         domain.Join
	Scrollback    []ChannelScrollbackRecord
	ResetContext  bool
	PreserveTurns bool
}

type channelJoinData struct {
	window   []byte
	instance []byte
	event    []byte
}

// CommitChannelJoin writes both sides of a channel membership, its JOIN event,
// each recipient projection and any actor-context reset in one transaction. A
// failed transaction leaves the earlier membership interval unchanged.
func (s *SQLiteStore) CommitChannelJoin(
	ctx context.Context,
	join ChannelJoin,
) (CommittedChannelEvent, error) {
	var committed CommittedChannelEvent
	err := s.inSpan(ctx, "store.sqlite.commit_channel_join",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(join.Window.Name())),
			attribute.String(observability.AttrInstanceID, string(join.Instance.ID())),
		}, func(ctx context.Context, _ trace.Span) error {
			data, err := marshalChannelJoin(join)
			if err != nil {
				return err
			}
			scrollbackData, err := encodeChannelScrollback(join.Scrollback)
			if err != nil {
				return err
			}

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			if err := resetChannelJoinContext(ctx, tx, join); err != nil {
				return err
			}

			committed.EventID, err = writeChannelJoin(ctx, tx, join, data)
			if err != nil {
				return err
			}
			committed.ScrollbackIDs, err = appendChannelScrollbackTx(
				ctx, tx, join.Scrollback, scrollbackData,
			)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})
	if err != nil {
		return CommittedChannelEvent{}, err
	}

	s.instancesMu.Lock()
	if _, ok := s.instances[join.Instance.ID()]; !ok {
		s.instances[join.Instance.ID()] = join.Instance
	}
	s.instancesMu.Unlock()

	return committed, nil
}

func marshalChannelJoin(join ChannelJoin) (channelJoinData, error) {
	window, err := json.Marshal(rowFromWindow(join.Window))
	if err != nil {
		return channelJoinData{}, fmt.Errorf("marshal channel: %w", err)
	}

	instance, err := json.Marshal(join.Instance)
	if err != nil {
		return channelJoinData{}, fmt.Errorf("marshal instance: %w", err)
	}

	event, err := domain.MarshalPersistableEvent(join.Event)
	if err != nil {
		return channelJoinData{}, fmt.Errorf("marshal join event: %w", err)
	}

	return channelJoinData{window: window, instance: instance, event: event}, nil
}

func resetChannelJoinContext(ctx context.Context, tx *sql.Tx, join ChannelJoin) error {
	if !join.ResetContext {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_scrollback WHERE instance_id = ? AND channel = ?`,
		join.Instance.ID(), join.Window.Name()); err != nil {
		return fmt.Errorf("reset channel scrollback: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM instance_replies
		WHERE instance_id = ? AND window_kind = 1 AND window_key = ?
	`, join.Instance.ID(), join.Window.Name()); err != nil {
		return fmt.Errorf("reset channel replies: %w", err)
	}

	if join.PreserveTurns {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM model_turns
		WHERE instance_id = ? AND window_kind = 1 AND window_key = ?
	`, join.Instance.ID(), join.Window.Name()); err != nil {
		return fmt.Errorf("reset model turns: %w", err)
	}

	return nil
}

func writeChannelJoin(
	ctx context.Context,
	tx *sql.Tx,
	join ChannelJoin,
	data channelJoinData,
) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channels (name, data) VALUES (?, ?)
		 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
		join.Window.Name(), string(data.window)); err != nil {
		return 0, fmt.Errorf("save channel: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE SET
		     nick = excluded.nick,
		     data = excluded.data`,
		string(join.Instance.ID()), string(join.Instance.Nick()), string(data.instance)); err != nil {
		return 0, fmt.Errorf("save instance: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		join.Window.Name(), domain.EventType(join.Event), string(data.event),
		domain.EventTime(join.Event).Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("append join event: %w", err)
	}

	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read join event id: %w", err)
	}

	return eventID, nil
}

// DeleteWindow implements Store. The name is matched under NOCASE,
// the collation [SQLiteStore.GetWindow] reads with, so destroying a
// channel destroys every spelling of it.
//
// That matters on a database written before the casemapping existed,
// which may hold `#Dev` and `#dev` as separate rows. `GetWindow`
// answers such a pair with one of them, leaving the other a shadow
// nothing can reach; a BINARY delete would remove the row it was
// handed and promote the shadow, so the next client to create a
// channel under that name would find it furnished with a topic and
// modes from a channel nobody was in, against RFC 2811 §2's rule
// that a re-created channel starts fresh.
//
// The shadow's `events` rows outlive it, because the event log is
// keyed by channel name under BINARY and the session only ever asks
// for the spelling `ChannelWindow.Name()` gave it. Nothing reads
// those rows again; retention removes them later. Model-turn rows
// are actor-visible context, so this operation deletes every turn
// for the channel in the same transaction as the channel itself.
func (s *SQLiteStore) DeleteWindow(ctx context.Context, name domain.ChannelName) error {
	return s.inSpan(ctx, "store.sqlite.delete_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(name))},
		func(ctx context.Context, _ trace.Span) error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.ExecContext(ctx, `
				DELETE FROM model_turns
				WHERE window_kind = 1 AND window_key = ? COLLATE NOCASE
			`, name); err != nil {
				return fmt.Errorf("delete channel model turns: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM channels WHERE name = ? COLLATE NOCASE`, name); err != nil {
				return err
			}

			return tx.Commit()
		})
}

// AppendEvent implements Store.
func (s *SQLiteStore) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) (int64, error) {
	var id int64
	err := s.inSpan(ctx, "store.sqlite.append_event",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			data, err := domain.MarshalPersistableEvent(event)
			if err != nil {
				return fmt.Errorf("marshal event: %w", err)
			}

			id, err = execInsert(ctx, s.db,
				`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
				ch, domain.EventType(event), string(data), domain.EventTime(event).Format(time.RFC3339Nano))
			return err
		})

	return id, err
}

// ChannelAuditEvent is one canonical channel event and the channel
// whose audit log receives it.
type ChannelAuditEvent struct {
	Channel domain.ChannelName
	Event   domain.ChannelActivity
}

type encodedChannelAuditEvent struct {
	channel   domain.ChannelName
	eventType string
	data      []byte
	at        time.Time
}

func encodeChannelAuditEvents(events []ChannelAuditEvent) ([]encodedChannelAuditEvent, error) {
	encoded := make([]encodedChannelAuditEvent, len(events))
	for i, event := range events {
		data, err := domain.MarshalPersistableEvent(event.Event)
		if err != nil {
			return nil, fmt.Errorf("marshal audit event for %q: %w", event.Channel, err)
		}

		encoded[i] = encodedChannelAuditEvent{
			channel:   event.Channel,
			eventType: domain.EventType(event.Event),
			data:      data,
			at:        domain.EventTime(event.Event),
		}
	}

	return encoded, nil
}

func appendChannelAuditEvents(
	ctx context.Context,
	tx *sql.Tx,
	events []encodedChannelAuditEvent,
) error {
	for _, event := range events {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
			event.channel, event.eventType, string(event.data),
			event.at.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("append audit event for %q: %w", event.channel, err)
		}
	}

	return nil
}

func canonicaliseChannelAuditEvents(
	ctx context.Context,
	tx *sql.Tx,
	events []ChannelAuditEvent,
) ([]ChannelAuditEvent, error) {
	canonical := make([]ChannelAuditEvent, len(events))
	for i, event := range events {
		channel := event.Channel
		err := tx.QueryRowContext(ctx,
			`SELECT name FROM channels WHERE name = ? COLLATE NOCASE ORDER BY name LIMIT 1`,
			channel,
		).Scan(&channel)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("resolve audit channel %q: %w", event.Channel, err)
		}

		activity := event.Event
		if part, ok := activity.(domain.Part); ok {
			part.Target = channel
			activity = part
		}
		canonical[i] = ChannelAuditEvent{Channel: channel, Event: activity}
	}

	return canonical, nil
}

// ActorRename is the durable state transition for one NICK command.
type ActorRename struct {
	Instance   *domain.Instance
	Windows    []*domain.ChannelWindow
	Events     []ChannelAuditEvent
	Scrollback []ChannelScrollbackRecord
}

// CommitActorRename writes the renamed instance, every supplied channel
// member snapshot, all canonical NICK events and every recipient's projected
// scrollback in one transaction.
func (s *SQLiteStore) CommitActorRename(
	ctx context.Context,
	rename ActorRename,
) (CommittedActorRename, error) {
	var committed CommittedActorRename
	err := s.inSpan(ctx, "store.sqlite.commit_actor_rename",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(rename.Instance.ID())),
			attribute.String(observability.AttrNick, string(rename.Instance.Nick())),
		}, func(ctx context.Context, _ trace.Span) error {
			instanceData, err := json.Marshal(rename.Instance)
			if err != nil {
				return fmt.Errorf("marshal instance: %w", err)
			}

			type encodedWindow struct {
				name domain.ChannelName
				data []byte
			}
			windows := make([]encodedWindow, len(rename.Windows))
			for i, window := range rename.Windows {
				data, err := json.Marshal(rowFromWindow(window))
				if err != nil {
					return fmt.Errorf("marshal channel %q: %w", window.Name(), err)
				}
				windows[i] = encodedWindow{name: window.Name(), data: data}
			}

			events, err := encodeChannelAuditEvents(rename.Events)
			if err != nil {
				return err
			}
			scrollback, err := encodeChannelScrollback(rename.Scrollback)
			if err != nil {
				return err
			}

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.ExecContext(ctx,
				`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)
				 ON CONFLICT (instance_id) DO UPDATE SET
				     nick = excluded.nick,
				     data = excluded.data`,
				string(rename.Instance.ID()), string(rename.Instance.Nick()),
				string(instanceData)); err != nil {
				return fmt.Errorf("save instance: %w", err)
			}

			for _, window := range windows {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO channels (name, data) VALUES (?, ?)
					 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
					window.name, string(window.data)); err != nil {
					return fmt.Errorf("save channel %q: %w", window.name, err)
				}
			}

			if err := appendChannelAuditEvents(ctx, tx, events); err != nil {
				return err
			}
			committed.ScrollbackIDs, err = appendChannelScrollbackTx(
				ctx, tx, rename.Scrollback, scrollback,
			)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})
	if err != nil {
		return CommittedActorRename{}, err
	}

	s.instancesMu.Lock()
	if _, ok := s.instances[rename.Instance.ID()]; !ok {
		s.instances[rename.Instance.ID()] = rename.Instance
	}
	s.instancesMu.Unlock()

	return committed, nil
}

// CommittedActorRename contains the internal row identifiers for the projected
// scrollback rows written by [SQLiteStore.CommitActorRename].
type CommittedActorRename struct {
	ScrollbackIDs []int64
}

// ChannelScrollbackRecord is one recipient-specific channel event.
// Event has already passed through the same visibility projection as
// the live delivery.
type ChannelScrollbackRecord struct {
	InstanceID domain.InstanceID
	Channel    domain.ChannelName
	Event      domain.PersistableEvent
}

// ChannelDeparture is the durable state transition for one PART or KICK.
type ChannelDeparture struct {
	Window     *domain.ChannelWindow
	Instance   *domain.Instance
	Event      domain.ChannelDepartureEvent
	Scrollback []ChannelScrollbackRecord
}

// CommittedChannelEvent identifies the audit event and each projected
// scrollback row written by one channel transition.
type CommittedChannelEvent struct {
	EventID       int64
	ScrollbackIDs []int64
}

// ChannelEvent is one canonical channel event and the recipient projections
// derived from it.
type ChannelEvent struct {
	Channel    domain.ChannelName
	Event      domain.ChannelActivity
	Scrollback []ChannelScrollbackRecord
}

// CommitChannelEvent writes a canonical channel event and each projected
// scrollback row in one transaction.
func (s *SQLiteStore) CommitChannelEvent(
	ctx context.Context,
	event ChannelEvent,
) (CommittedChannelEvent, error) {
	var committed CommittedChannelEvent
	err := s.inSpan(ctx, "store.sqlite.commit_channel_event",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(event.Channel)),
		}, func(ctx context.Context, _ trace.Span) error {
			eventData, err := domain.MarshalPersistableEvent(event.Event)
			if err != nil {
				return fmt.Errorf("marshal channel event: %w", err)
			}
			scrollbackData, err := encodeChannelScrollback(event.Scrollback)
			if err != nil {
				return err
			}

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			committed.EventID, err = appendChannelEvent(
				ctx, tx, event.Channel, event.Event, eventData,
			)
			if err != nil {
				return err
			}
			committed.ScrollbackIDs, err = appendChannelScrollbackTx(
				ctx, tx, event.Scrollback, scrollbackData,
			)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})

	return committed, err
}

// ChannelUpdate is one channel-state mutation and the event that announces it.
type ChannelUpdate struct {
	Window     *domain.ChannelWindow
	Event      domain.ChannelActivity
	Scrollback []ChannelScrollbackRecord
}

// CommitChannelUpdate writes channel state, its audit event and each projected
// scrollback row in one transaction.
func (s *SQLiteStore) CommitChannelUpdate(
	ctx context.Context,
	update ChannelUpdate,
) (CommittedChannelEvent, error) {
	var committed CommittedChannelEvent
	err := s.inSpan(ctx, "store.sqlite.commit_channel_update",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(update.Window.Name())),
		}, func(ctx context.Context, _ trace.Span) error {
			windowData, err := json.Marshal(rowFromWindow(update.Window))
			if err != nil {
				return fmt.Errorf("marshal channel: %w", err)
			}
			eventData, err := domain.MarshalPersistableEvent(update.Event)
			if err != nil {
				return fmt.Errorf("marshal channel event: %w", err)
			}
			scrollbackData, err := encodeChannelScrollback(update.Scrollback)
			if err != nil {
				return err
			}

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.ExecContext(ctx,
				`INSERT INTO channels (name, data) VALUES (?, ?)
				 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
				update.Window.Name(), string(windowData)); err != nil {
				return fmt.Errorf("save channel: %w", err)
			}

			committed.EventID, err = appendChannelEvent(
				ctx, tx, update.Window.Name(), update.Event, eventData,
			)
			if err != nil {
				return err
			}
			committed.ScrollbackIDs, err = appendChannelScrollbackTx(
				ctx, tx, update.Scrollback, scrollbackData,
			)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})

	return committed, err
}

type channelDepartureData struct {
	instance   []byte
	window     []byte
	event      []byte
	scrollback [][]byte
}

// CommitChannelDeparture writes a departure event, both membership
// representations, actor-scoped context deletion and the remaining
// recipients' projected scrollback in one transaction.
func (s *SQLiteStore) CommitChannelDeparture(
	ctx context.Context,
	departure ChannelDeparture,
) (CommittedChannelEvent, error) {
	var committed CommittedChannelEvent
	err := s.inSpan(ctx, "store.sqlite.commit_channel_departure",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(departure.Window.Name())),
			attribute.String(observability.AttrInstanceID, string(departure.Instance.ID())),
		}, func(ctx context.Context, _ trace.Span) error {
			data, err := encodeChannelDeparture(departure)
			if err != nil {
				return err
			}

			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			committed, err = commitChannelDepartureTx(ctx, tx, departure, data)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})

	return committed, err
}

func encodeChannelDeparture(departure ChannelDeparture) (channelDepartureData, error) {
	instance, err := json.Marshal(departure.Instance)
	if err != nil {
		return channelDepartureData{}, fmt.Errorf("marshal instance: %w", err)
	}

	event, err := domain.MarshalPersistableEvent(departure.Event)
	if err != nil {
		return channelDepartureData{}, fmt.Errorf("marshal departure event: %w", err)
	}

	var window []byte
	if departure.Window.Members.Len() > 0 {
		window, err = json.Marshal(rowFromWindow(departure.Window))
		if err != nil {
			return channelDepartureData{}, fmt.Errorf("marshal channel: %w", err)
		}
	}

	scrollback := make([][]byte, len(departure.Scrollback))
	for i, record := range departure.Scrollback {
		scrollback[i], err = domain.MarshalPersistableEvent(record.Event)
		if err != nil {
			return channelDepartureData{}, fmt.Errorf("marshal projected event: %w", err)
		}
	}

	return channelDepartureData{
		instance: instance, window: window, event: event, scrollback: scrollback,
	}, nil
}

func commitChannelDepartureTx(
	ctx context.Context,
	tx *sql.Tx,
	departure ChannelDeparture,
	data channelDepartureData,
) (CommittedChannelEvent, error) {
	if err := saveChannelDepartureState(ctx, tx, departure, data); err != nil {
		return CommittedChannelEvent{}, err
	}

	eventID, err := appendDepartureEvent(ctx, tx, departure, data.event)
	if err != nil {
		return CommittedChannelEvent{}, err
	}
	if err := deleteDepartedActorContext(ctx, tx, departure); err != nil {
		return CommittedChannelEvent{}, err
	}

	scrollbackIDs, err := appendDepartureScrollback(ctx, tx, departure, data.scrollback)
	if err != nil {
		return CommittedChannelEvent{}, err
	}

	return CommittedChannelEvent{EventID: eventID, ScrollbackIDs: scrollbackIDs}, nil
}

func saveChannelDepartureState(
	ctx context.Context,
	tx *sql.Tx,
	departure ChannelDeparture,
	data channelDepartureData,
) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE SET
		     nick = excluded.nick,
		     data = excluded.data`,
		string(departure.Instance.ID()), string(departure.Instance.Nick()),
		string(data.instance)); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	if len(data.window) > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels (name, data) VALUES (?, ?)
			 ON CONFLICT (name) DO UPDATE SET data = excluded.data`,
			departure.Window.Name(), string(data.window)); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM model_turns
		WHERE window_kind = 1 AND window_key = ? COLLATE NOCASE
	`, departure.Window.Name()); err != nil {
		return fmt.Errorf("delete channel model turns: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channels WHERE name = ? COLLATE NOCASE`,
		departure.Window.Name()); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}

	return nil
}

func appendDepartureEvent(
	ctx context.Context,
	tx *sql.Tx,
	departure ChannelDeparture,
	data []byte,
) (int64, error) {
	return appendChannelEvent(ctx, tx, departure.Window.Name(), departure.Event, data)
}

func appendChannelEvent(
	ctx context.Context,
	tx *sql.Tx,
	channel domain.ChannelName,
	event domain.ChannelActivity,
	data []byte,
) (int64, error) {
	result, err := tx.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		channel, domain.EventType(event), string(data),
		domain.EventTime(event).Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("append channel event: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read channel event id: %w", err)
	}

	return id, nil
}

func deleteDepartedActorContext(
	ctx context.Context,
	tx *sql.Tx,
	departure ChannelDeparture,
) error {
	windowKind, windowKey := windowColumns(
		protocol.ChannelWindowTarget(departure.Window.Name()),
	)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM channel_scrollback
		WHERE instance_id = ? AND channel = ?
	`, departure.Instance.ID(), departure.Window.Name()); err != nil {
		return fmt.Errorf("delete channel scrollback: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM instance_replies
		WHERE instance_id = ? AND window_kind = ? AND window_key = ?
	`, departure.Instance.ID(), windowKind, windowKey); err != nil {
		return fmt.Errorf("delete channel replies: %w", err)
	}

	return nil
}

func appendDepartureScrollback(
	ctx context.Context,
	tx *sql.Tx,
	departure ChannelDeparture,
	data [][]byte,
) ([]int64, error) {
	return appendChannelScrollbackTx(ctx, tx, departure.Scrollback, data)
}

func encodeChannelScrollback(records []ChannelScrollbackRecord) ([][]byte, error) {
	data := make([][]byte, len(records))
	for i, record := range records {
		encoded, err := domain.MarshalPersistableEvent(record.Event)
		if err != nil {
			return nil, fmt.Errorf("marshal projected event: %w", err)
		}
		data[i] = encoded
	}

	return data, nil
}

func appendChannelScrollbackTx(
	ctx context.Context,
	tx *sql.Tx,
	records []ChannelScrollbackRecord,
	data [][]byte,
) ([]int64, error) {
	ids := make([]int64, 0, len(records))
	for i, record := range records {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO channel_scrollback
				(instance_id, channel, type, data, at)
			VALUES (?, ?, ?, ?, ?)
		`, record.InstanceID, record.Channel, domain.EventType(record.Event),
			string(data[i]), domain.EventTime(record.Event).Format(time.RFC3339Nano))
		if err != nil {
			return nil, fmt.Errorf("append projected event: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read projected event id: %w", err)
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// AppendChannelScrollback records projected channel deliveries in
// input order and returns their internal row identifiers in the same
// order.
func (s *SQLiteStore) AppendChannelScrollback(
	ctx context.Context,
	records []ChannelScrollbackRecord,
) ([]int64, error) {
	if len(records) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(records))
	err := s.inSpan(ctx, "store.sqlite.append_channel_scrollback", nil,
		func(ctx context.Context, _ trace.Span) error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			for _, record := range records {
				data, err := domain.MarshalPersistableEvent(record.Event)
				if err != nil {
					return fmt.Errorf("marshal event: %w", err)
				}

				result, err := tx.ExecContext(ctx, `
					INSERT INTO channel_scrollback
						(instance_id, channel, type, data, at)
					VALUES (?, ?, ?, ?, ?)
				`, record.InstanceID, record.Channel, domain.EventType(record.Event),
					string(data), domain.EventTime(record.Event).Format(time.RFC3339Nano))
				if err != nil {
					return err
				}

				id, err := result.LastInsertId()
				if err != nil {
					return fmt.Errorf("read inserted row id: %w", err)
				}
				ids = append(ids, id)
			}

			return tx.Commit()
		})

	return ids, err
}

// ChannelScrollback returns the most recent projected events that
// the actor received in its current membership interval.
func (s *SQLiteStore) ChannelScrollback(
	ctx context.Context,
	actor domain.InstanceID,
	ch domain.ChannelName,
	n int,
) ([]domain.StoredEvent, error) {
	return s.ChannelScrollbackBefore(ctx, actor, ch, nil, n)
}

// ChannelScrollbackBefore returns the most recent projected events
// strictly before `before`, or the latest events when it is nil.
func (s *SQLiteStore) ChannelScrollbackBefore(
	ctx context.Context,
	actor domain.InstanceID,
	ch domain.ChannelName,
	before *int64,
	n int,
) ([]domain.StoredEvent, error) {
	var events []domain.StoredEvent
	err := s.inSpan(ctx, "store.sqlite.channel_scrollback",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(ch)),
			attribute.String(observability.AttrInstanceID, string(actor)),
		},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, data FROM (
				SELECT id, data FROM channel_scrollback
				WHERE instance_id = ? AND channel = ?
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC`, []any{actor, ch, n}
			if before != nil {
				query, args = `SELECT id, data FROM (
					SELECT id, data FROM channel_scrollback
					WHERE instance_id = ? AND channel = ? AND id < ?
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`, []any{actor, ch, *before, n}
			}

			got, err := queryEventRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// DeleteChannelScrollback removes every projected event for one
// actor and channel. PART, KICK and a later JOIN use this operation
// to make membership intervals disjoint.
func (s *SQLiteStore) DeleteChannelScrollback(
	ctx context.Context,
	actor domain.InstanceID,
	ch domain.ChannelName,
) error {
	return s.inSpan(ctx, "store.sqlite.delete_channel_scrollback",
		[]attribute.KeyValue{
			attribute.String(observability.AttrChannel, string(ch)),
			attribute.String(observability.AttrInstanceID, string(actor)),
		},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`DELETE FROM channel_scrollback WHERE instance_id = ? AND channel = ?`,
				actor, ch)
		})
}

// EventsBefore implements Store. The query takes the last `n`
// events strictly before `before` (or the most recent when
// `before` is nil) by selecting them descending in an inner
// query and re-ordering ascending in the outer one — the driver
// then yields rows already in chronological order.
func (s *SQLiteStore) EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error) {
	var events []domain.StoredEvent
	err := s.inSpan(ctx, "store.sqlite.events_before",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, data FROM (
					SELECT id, data FROM events WHERE channel = ?
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`, []any{ch, n}
			if before != nil {
				query, args = `SELECT id, data FROM (
						SELECT id, data FROM events WHERE channel = ? AND id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`, []any{ch, *before, n}
			}

			got, err := queryEventRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// AppendInstanceReply records a point-to-point reply — a numeric the
// instance received in answer to its own command (WHOIS, LIST) — in
// the instance's private reply log. This is the instance's own
// memory: it replays only into that instance's prompt, never into
// the shared channel log where other instances would read it.
func (s *SQLiteStore) AppendInstanceReply(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
	event domain.IssuerReply,
) (int64, error) {
	var rowID int64
	err := s.inSpan(ctx, "store.sqlite.append_instance_reply",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			data, err := domain.MarshalPersistableEvent(event)
			if err != nil {
				return fmt.Errorf("marshal event: %w", err)
			}

			kind, key := windowColumns(window)
			rowID, err = execInsert(ctx, s.db,
				`INSERT INTO instance_replies
				 (instance_id, window_kind, window_key, type, data, at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				id, kind, key, domain.EventType(event), string(data), domain.EventTime(event).Format(time.RFC3339Nano))
			return err
		})

	return rowID, err
}

// InstanceRepliesBefore returns up to `n` of the instance's own
// replies strictly before `before` (or the most recent when `before`
// is nil), in chronological order.
func (s *SQLiteStore) InstanceRepliesBefore(
	ctx context.Context,
	id domain.InstanceID,
	before *int64,
	n int,
) ([]InstanceReplyRecord, error) {
	var events []InstanceReplyRecord
	err := s.inSpan(ctx, "store.sqlite.instance_replies_before",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, window_kind, window_key, data FROM (
					SELECT id, window_kind, window_key, data FROM instance_replies WHERE instance_id = ?
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`, []any{id, n}
			if before != nil {
				query, args = `SELECT id, window_kind, window_key, data FROM (
						SELECT id, window_kind, window_key, data FROM instance_replies WHERE instance_id = ? AND id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`, []any{id, *before, n}
			}

			got, err := queryInstanceReplyRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// InstanceRepliesForWindowBefore returns one instance's replies for
// exactly `window`, up to `n` rows strictly before `before`. A nil
// window selects session-wide replies only.
func (s *SQLiteStore) InstanceRepliesForWindowBefore(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
	before *int64,
	n int,
) ([]InstanceReplyRecord, error) {
	kind, key := windowColumns(window)
	var events []InstanceReplyRecord
	err := s.inSpan(ctx, "store.sqlite.instance_replies_for_window_before",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, window_kind, window_key, data FROM (
					SELECT id, window_kind, window_key, data FROM instance_replies
					WHERE instance_id = ? AND window_kind = ? AND window_key = ?
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`, []any{id, kind, key, n}
			if before != nil {
				query, args = `SELECT id, window_kind, window_key, data FROM (
						SELECT id, window_kind, window_key, data FROM instance_replies
						WHERE instance_id = ? AND window_kind = ? AND window_key = ? AND id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`, []any{id, kind, key, *before, n}
			}

			got, err := queryInstanceReplyRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// DeleteInstanceRepliesForWindow removes private reply lines that
// belonged to a window whose current client view has closed.
func (s *SQLiteStore) DeleteInstanceRepliesForWindow(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
) error {
	kind, key := windowColumns(window)
	return s.inSpan(ctx, "store.sqlite.delete_instance_replies_for_window",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(id)),
			attribute.String(observability.AttrChannel, key),
		}, func(ctx context.Context, _ trace.Span) error {
			_, err := s.db.ExecContext(ctx,
				`DELETE FROM instance_replies
				 WHERE instance_id = ?
				   AND window_kind = ?
				   AND window_key = ?`,
				id, kind, key)
			return err
		})
}

// BeginModelTurn creates the durable envelope for one provider turn
// and runs the actor's retention pass. Admission is the one point in
// a turn where retention runs: the turn's own entries do not exist
// yet, so the pass cannot delete the turn it has just admitted, and a
// turn that appends a dozen entries pays for the pass once.
func (s *SQLiteStore) BeginModelTurn(
	ctx context.Context,
	turn ModelTurn,
	input ModelTurnEntry,
) (ModelTurnID, error) {
	if turn.Window == nil {
		return 0, fmt.Errorf("begin model turn: window is required")
	}
	if input.Kind != ModelTurnInput || !json.Valid(input.Data) {
		return 0, fmt.Errorf("begin model turn: valid input entry is required")
	}

	var turnID ModelTurnID
	err := s.inSpan(ctx, "store.sqlite.begin_model_turn",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(turn.InstanceID))},
		func(ctx context.Context, _ trace.Span) error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			kind, key := windowColumns(turn.Window)
			result, err := tx.ExecContext(ctx, `
				INSERT INTO model_turns
					(instance_id, window_kind, window_key, model_id, started_at)
				VALUES (?, ?, ?, ?, ?)
			`, turn.InstanceID, kind, key, turn.ModelID, turn.StartedAt.Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return fmt.Errorf("read model turn id: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO model_turn_entries (turn_id, seq, kind, data, at)
				VALUES (?, ?, ?, ?, ?)
			`, id, input.Seq, input.Kind, string(input.Data),
				input.At.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			if err := trimModelTurns(
				ctx, tx, turn.InstanceID,
				modelTurnRetentionHeadroom, modelTurnRetentionBytes,
			); err != nil {
				return fmt.Errorf("trim model turns: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			turnID = ModelTurnID(id)
			return nil
		})

	return turnID, err
}

// AppendModelTurnEntry appends one JSON record to a model turn.
// Retention runs at [BeginModelTurn], so an append writes one row and
// nothing else.
func (s *SQLiteStore) AppendModelTurnEntry(
	ctx context.Context,
	turnID ModelTurnID,
	entry ModelTurnEntry,
) error {
	if !json.Valid(entry.Data) {
		return fmt.Errorf("append model turn entry: invalid JSON")
	}

	return s.inSpan(ctx, "store.sqlite.append_model_turn_entry", nil,
		func(ctx context.Context, _ trace.Span) error {
			result, err := s.db.ExecContext(ctx, `
				INSERT INTO model_turn_entries (turn_id, seq, kind, data, at)
				SELECT id, ?, ?, ?, ? FROM model_turns WHERE id = ?
			`, entry.Seq, entry.Kind, string(entry.Data),
				entry.At.Format(time.RFC3339Nano), turnID)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows == 0 {
				return ErrModelTurnClosed
			}

			return nil
		})
}

type modelTurnExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func trimModelTurns(
	ctx context.Context,
	executor modelTurnExecutor,
	actor domain.InstanceID,
	maxTurns int,
	maxBytes int64,
) error {
	_, err := executor.ExecContext(ctx, `
		WITH turn_sizes AS (
			SELECT turn.id,
				COALESCE(SUM(length(CAST(entry.data AS BLOB))), 0) AS bytes
			FROM model_turns AS turn
			LEFT JOIN model_turn_entries AS entry ON entry.turn_id = turn.id
			WHERE turn.instance_id = ?
			GROUP BY turn.id
		), ranked AS (
			SELECT id,
				ROW_NUMBER() OVER (ORDER BY id DESC) AS position,
				SUM(bytes) OVER (ORDER BY id DESC) AS retained_bytes
			FROM turn_sizes
		)
		DELETE FROM model_turns WHERE id IN (
			SELECT id FROM ranked
			WHERE position > ? OR (position > 1 AND retained_bytes > ?)
		)
	`, actor, maxTurns, maxBytes)

	return err
}

// ModelTurnsForInstanceBefore returns the newest `n` turns an actor
// recorded before `before`, oldest first. A nil cursor starts at the
// newest turn.
func (s *SQLiteStore) ModelTurnsForInstanceBefore(
	ctx context.Context,
	id domain.InstanceID,
	before *ModelTurnID,
	n int,
) ([]ModelTurnRecord, error) {
	var turns []ModelTurnRecord
	err := s.inSpan(ctx, "store.sqlite.model_turns_for_instance_before",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, window_kind, window_key, model_id, started_at FROM (
					SELECT id, window_kind, window_key, model_id, started_at
					FROM model_turns
					WHERE instance_id = ?
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`, []any{id, n}
			if before != nil {
				query, args = `SELECT id, window_kind, window_key, model_id, started_at FROM (
						SELECT id, window_kind, window_key, model_id, started_at
						FROM model_turns
						WHERE instance_id = ? AND id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`, []any{id, *before, n}
			}

			got, err := queryModelTurnRows(ctx, s.db, id, query, args)
			if err != nil {
				return err
			}

			turns = got
			return nil
		})

	return turns, err
}

func queryModelTurnRows(
	ctx context.Context,
	db *sql.DB,
	id domain.InstanceID,
	query string,
	args []any,
) ([]ModelTurnRecord, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var turns []ModelTurnRecord
	for rows.Next() {
		var turn ModelTurnRecord
		var windowKind int
		var windowKey, startedAt string
		if err := rows.Scan(
			&turn.ID, &windowKind, &windowKey, &turn.ModelID, &startedAt,
		); err != nil {
			return nil, err
		}

		window, err := windowFromColumns(windowKind, windowKey)
		if err != nil {
			return nil, fmt.Errorf("read model turn window: %w", err)
		}
		parsedStartedAt, err := time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse model turn start time: %w", err)
		}

		turn.InstanceID = id
		turn.Window = window
		turn.StartedAt = parsedStartedAt
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return turns, nil
}

// ModelTurnEntries returns a turn's records in the order its writer
// produced them.
func (s *SQLiteStore) ModelTurnEntries(ctx context.Context, turnID ModelTurnID) ([]ModelTurnEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, seq, data, at
		FROM model_turn_entries
		WHERE turn_id = ?
		ORDER BY seq
	`, turnID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []ModelTurnEntry
	for rows.Next() {
		var entry ModelTurnEntry
		var data, at string
		if err := rows.Scan(&entry.Kind, &entry.Seq, &data, &at); err != nil {
			return nil, err
		}

		parsedAt, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("parse model turn entry time: %w", err)
		}
		entry.Data = json.RawMessage(data)
		entry.At = parsedAt
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return entries, nil
}

// DeleteModelTurnsForWindow removes every recorded turn whose model
// could see a window that has now closed.
func (s *SQLiteStore) DeleteModelTurnsForWindow(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
) error {
	kind, key := windowColumns(window)
	return s.inSpan(ctx, "store.sqlite.delete_model_turns_for_window",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(id)),
			attribute.String(observability.AttrChannel, key),
		}, func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `
				DELETE FROM model_turns
				WHERE instance_id = ? AND window_kind = ? AND window_key = ?
			`, id, kind, key)
		})
}

func windowColumns(window protocol.WindowTarget) (int, string) {
	switch protocol.WindowTargetKind(window) {
	case domain.KindChannel:
		return 1, string(protocol.WindowKey(window))
	case domain.KindDM:
		return 2, string(protocol.WindowKey(window))
	}

	// No target is the status window, `&modeloff`, where an issuer's
	// own replies are filed. It has no server-side conversation, so
	// the row holds no window.
	return 0, ""
}

func windowFromColumns(kind int, key string) (protocol.WindowTarget, error) {
	switch kind {
	case 0:
		return nil, nil
	case 1:
		return protocol.ChannelWindowTarget(domain.ChannelName(key)), nil
	case 2:
		return protocol.DirectWindowTarget(domain.InstanceID(key)), nil
	default:
		return nil, fmt.Errorf("unknown instance reply window kind %d", kind)
	}
}

func queryInstanceReplyRows(
	ctx context.Context,
	db *sql.DB,
	query string,
	args []any,
) ([]InstanceReplyRecord, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var replies []InstanceReplyRecord
	for rows.Next() {
		var (
			id   int64
			kind int
			key  string
			data string
		)
		if err := rows.Scan(&id, &kind, &key, &data); err != nil {
			return nil, err
		}

		event, err := domain.UnmarshalPersistableEvent([]byte(data))
		if errors.Is(err, domain.ErrUnknownEventType) {
			slog.Default().WarnContext(ctx, "skipping unrecognised instance-reply row",
				"id", id, "error", err)
			continue
		}
		if err != nil {
			return nil, err
		}
		reply, ok := event.(domain.IssuerReply)
		if !ok {
			return nil, fmt.Errorf("instance reply row %d contains %T", id, event)
		}
		window, err := windowFromColumns(kind, key)
		if err != nil {
			return nil, fmt.Errorf("instance reply row %d: %w", id, err)
		}

		replies = append(replies, InstanceReplyRecord{ID: id, Window: window, Event: reply})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return replies, nil
}

// DMEventsBefore implements Store. Returns the bidirectional message
// thread between `self` and `peer`. Either id may be the empty string
// (the user). `source_instance_id` is a generated column carrying
// the event source's instance id as a real, indexable value. An
// absent field reads the same as a present empty string; `type` is
// the event's own stored column, not a JSON extraction. Rows come
// back chronological via inner-desc / outer-asc.
func (s *SQLiteStore) DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error) {
	var events []domain.StoredEvent
	err := s.inSpan(ctx, "store.sqlite.dm_events_before",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(self)),
			attribute.String("modeloff.dm.peer_id", string(peer)),
		},
		func(ctx context.Context, _ trace.Span) error {
			// Bidirectional message rows: peer→self and self→peer.
			// idx_events_source_thread (source_instance_id, type, channel, id)
			// covers both branches of the OR.
			const messageRows = `SELECT id, data FROM events WHERE
				(channel = ? AND source_instance_id = ?)
				OR
				(channel = ? AND source_instance_id = ?)
			`

			query, args := `SELECT id, data FROM (
					SELECT id, data FROM (`+messageRows+`) AS thread
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`,
				[]any{string(peer), string(self), string(self), string(peer), n}
			if before != nil {
				query, args = `SELECT id, data FROM (
						SELECT id, data FROM (`+messageRows+`) AS thread WHERE id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`,
					[]any{string(peer), string(self), string(self), string(peer), *before, n}
			}

			got, err := queryEventRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// EventsFrom implements Store.
func (s *SQLiteStore) EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error) {
	var events []domain.StoredEvent
	err := s.inSpan(ctx, "store.sqlite.events_from",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT id, data FROM events WHERE channel = ?
				 ORDER BY id ASC LIMIT ?`, []any{ch, n}
			if from != nil {
				query, args = `SELECT id, data FROM events WHERE channel = ? AND id >= ?
					 ORDER BY id ASC LIMIT ?`, []any{ch, *from, n}
			}

			got, err := queryEventRows(ctx, s.db, query, args)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// CountEventsFrom implements Store. Returns the number of events in
// channel ch at or after the given event id (inclusive), or the
// total row count for the channel when from is nil — the same
// bounds as EventsFrom, computed with a `count(*)` so no matched
// row's JSON payload is decoded just to take len() of the result.
func (s *SQLiteStore) CountEventsFrom(ctx context.Context, ch domain.ChannelName, from *int64) (int, error) {
	var count int
	err := s.inSpan(ctx, "store.sqlite.count_events_from",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			query, args := `SELECT count(*) FROM events WHERE channel = ?`, []any{ch}
			if from != nil {
				query, args = `SELECT count(*) FROM events WHERE channel = ? AND id >= ?`, []any{ch, *from}
			}

			got, err := queryRow(ctx, s.db, query, args, nil, scalarColumn[int]())
			if err != nil {
				return err
			}

			count = got
			return nil
		})

	return count, err
}

// CountDMEventsFrom implements Store. Returns the number of messages
// in the DM thread between `self` and `peer` at or after the given
// event id (inclusive), or the whole thread when `from` is nil.
//
// It counts what the window shows: the messages of both directions,
// which are logged under different keys. A line to `peer` sits under
// the peer's id and the peer's answer under `self`'s, so counting one
// key alone answers for one direction, which for the user's DM with a
// model is the direction the user sent and never the one it is
// waiting to read. The `source_instance_id` generated column carries the
// sender, so `idx_events_source_thread` covers both branches of the OR.
func (s *SQLiteStore) CountDMEventsFrom(ctx context.Context, self, peer domain.InstanceID, from *int64) (int, error) {
	var count int
	err := s.inSpan(ctx, "store.sqlite.count_dm_events_from",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(self)),
			attribute.String("modeloff.dm.peer_id", string(peer)),
		},
		func(ctx context.Context, _ trace.Span) error {
			const thread = `SELECT count(*) FROM events WHERE
				(
					(channel = ? AND source_instance_id = ?)
					OR
					(channel = ? AND source_instance_id = ?)
				)`

			query, args := thread, []any{string(peer), string(self), string(self), string(peer)}
			if from != nil {
				query, args = thread+` AND id >= ?`, append(args, *from)
			}

			got, err := queryRow(ctx, s.db, query, args, nil, scalarColumn[int]())
			if err != nil {
				return err
			}

			count = got
			return nil
		})

	return count, err
}

// ListInstances implements Store. Returns canonical `*Instance`
// pointers from the registry; callers that called `GetInstanceByID`
// previously observe the same pointers.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]*domain.Instance, error) {
	var instances []*domain.Instance
	err := s.inSpan(ctx, "store.sqlite.list_instances", nil, func(ctx context.Context, _ trace.Span) error {
		fresh, err := queryRows(ctx, s.db,
			`SELECT data FROM instances WHERE pending_deletion = 0 ORDER BY nick`, nil,
			jsonColumn[domain.Instance])
		if err != nil {
			return err
		}

		instances = make([]*domain.Instance, 0, len(fresh))
		for i := range fresh {
			instances = append(instances, s.canonicaliseInstance(&fresh[i]))
		}

		return nil
	})

	return instances, err
}

// GetInstanceByID implements Store. Returns the canonical
// `*Instance` pointer — two calls for the same id return the same
// handle.
func (s *SQLiteStore) GetInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error) {
	var inst *domain.Instance
	err := s.inSpan(ctx, "store.sqlite.get_instance_by_id",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			fresh, err := queryRow(ctx, s.db,
				`SELECT data FROM instances WHERE instance_id = ? AND pending_deletion = 0`,
				[]any{string(id)}, nil,
				jsonColumn[domain.Instance])
			if err != nil {
				return fmt.Errorf("instance %q: %w", id, err)
			}

			inst = s.canonicaliseInstance(&fresh)
			return nil
		})

	return inst, err
}

// ResolveNick returns the canonical `*Instance` whose current
// display nick matches the argument. Identity is the stable anchor
// in this system; nicks are mutable display state. The command
// parser is the single intentional caller: it resolves user input
// into a handle once at the boundary, and every downstream call
// takes the handle.
//
// The nick is matched under NOCASE, which folds `A`-`Z` onto `a`-`z`
// and nothing else, the same fold [domain.EqualNick] applies, so the
// store and the session agree on when two nicks name one client. The
// row keeps the spelling it was saved with.
//
// If multiple instances share the same display nick the store
// returns the first in nick order. The `idx_instances_nick` indexes
// are non-unique because display nicks are expected to drift, and
// callers are responsible for preventing collisions upstream: the
// session claims a nick on its command loop and refuses one already
// held.
func (s *SQLiteStore) ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	var inst *domain.Instance
	err := s.inSpan(ctx, "store.sqlite.resolve_nick",
		[]attribute.KeyValue{attribute.String(observability.AttrNick, string(nick))},
		func(ctx context.Context, _ trace.Span) error {
			fresh, err := queryRow(ctx, s.db,
				`SELECT data FROM instances
				 WHERE nick = ? COLLATE NOCASE AND pending_deletion = 0
				 ORDER BY nick LIMIT 1`,
				[]any{nick},
				fmt.Errorf("resolve nick %q: %w", nick, ErrNoSuchNick),
				jsonColumn[domain.Instance])
			if err != nil {
				if errors.Is(err, ErrNoSuchNick) {
					return err
				}
				return fmt.Errorf("resolve nick %q: %w", nick, err)
			}

			inst = s.canonicaliseInstance(&fresh)
			return nil
		})

	return inst, err
}

// SaveInstance implements Store. The caller hands over the
// canonical handle; the store reads its current fields under the
// handle's read lock (via MarshalJSON) and writes them to the
// `instances` row. Registering the handle in the canonical map
// ensures a subsequent `GetInstanceByID` returns the same pointer.
func (s *SQLiteStore) SaveInstance(ctx context.Context, inst *domain.Instance) error {
	// Snapshot the nick once so the span attribute, the INSERT column
	// value, and the marshaled data blob all agree. The data blob is
	// already atomic with the marshal-time snapshot under the handle's
	// read lock; pairing the column value and span attribute with a
	// single nick read closes the divergence window.
	nick := inst.Nick()

	return s.inSpan(ctx, "store.sqlite.save_instance",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(inst.ID())),
			attribute.String(observability.AttrNick, string(nick)),
		},
		func(ctx context.Context, _ trace.Span) error {
			data, err := json.Marshal(inst)
			if err != nil {
				return err
			}

			if err := execMutation(ctx, s.db,
				`INSERT INTO instances (instance_id, nick, data) VALUES (?, ?, ?)
				 ON CONFLICT (instance_id) DO UPDATE SET
				     nick = excluded.nick,
				     data = excluded.data`,
				string(inst.ID()), string(nick), string(data)); err != nil {
				return err
			}

			// Register the saved handle as canonical if there isn't
			// already a registered handle for this id.
			s.instancesMu.Lock()
			if _, ok := s.instances[inst.ID()]; !ok {
				s.instances[inst.ID()] = inst
			}
			s.instancesMu.Unlock()

			return nil
		})
}

// InstanceDeletion is the durable state transition for one client
// connection ending. Events contains the canonical per-channel
// departure audit rows, and Scrollback contains each surviving
// recipient's projected departure rows.
type InstanceDeletion struct {
	InstanceID domain.InstanceID
	Events     []ChannelAuditEvent
	Scrollback []ChannelScrollbackRecord
}

// CommittedInstanceDeletion contains the canonical projected
// scrollback and its row identifiers from
// [SQLiteStore.CommitInstanceDeletion].
type CommittedInstanceDeletion struct {
	ScrollbackIDs []int64
	Scrollback    []ChannelScrollbackRecord
}

// DeleteInstanceByID implements Store.
func (s *SQLiteStore) DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error {
	_, err := s.CommitInstanceDeletion(ctx, InstanceDeletion{InstanceID: id})

	return err
}

// CommitInstanceDeletion evicts the instance row and its `memories`
// and `instance_replies` rows, removes its channel memberships, records
// the indexed-memory deletion still due, and appends every supplied
// canonical departure event and recipient projection in one
// transaction. The empty user id is reused across sessions, so its
// private replies remain available after the connection ends.
func (s *SQLiteStore) CommitInstanceDeletion(
	ctx context.Context,
	deletion InstanceDeletion,
) (CommittedInstanceDeletion, error) {
	id := deletion.InstanceID
	var committed CommittedInstanceDeletion

	err := s.inSpan(ctx, "store.sqlite.delete_instance_by_id",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin tx: %w", err)
			}

			defer func() { _ = tx.Rollback() }()

			committed, err = commitInstanceDeletionTx(ctx, tx, deletion)
			if err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			return nil
		})
	if err != nil {
		return CommittedInstanceDeletion{}, err
	}

	s.forgetInstance(id)

	return committed, nil
}

func commitInstanceDeletionTx(
	ctx context.Context,
	tx *sql.Tx,
	deletion InstanceDeletion,
) (CommittedInstanceDeletion, error) {
	id := deletion.InstanceID
	if err := recordPendingMemoryDeletion(ctx, tx, id); err != nil {
		return CommittedInstanceDeletion{}, err
	}
	if err := deleteInstanceOwnedContext(ctx, tx, id); err != nil {
		return CommittedInstanceDeletion{}, err
	}
	if err := deleteDirectPeerContext(ctx, tx, id); err != nil {
		return CommittedInstanceDeletion{}, err
	}
	if err := deleteInstanceChannelMemberships(ctx, tx, id); err != nil {
		return CommittedInstanceDeletion{}, err
	}
	if err := appendCanonicalChannelAuditEvents(ctx, tx, deletion.Events); err != nil {
		return CommittedInstanceDeletion{}, err
	}

	canonicalScrollback, err := canonicaliseChannelScrollback(ctx, tx, deletion.Scrollback)
	if err != nil {
		return CommittedInstanceDeletion{}, err
	}
	scrollback, err := encodeChannelScrollback(canonicalScrollback)
	if err != nil {
		return CommittedInstanceDeletion{}, err
	}
	scrollbackIDs, err := appendChannelScrollbackTx(ctx, tx, canonicalScrollback, scrollback)
	if err != nil {
		return CommittedInstanceDeletion{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM instances WHERE instance_id = ?`, string(id)); err != nil {
		return CommittedInstanceDeletion{}, fmt.Errorf("delete instance: %w", err)
	}

	return CommittedInstanceDeletion{
		ScrollbackIDs: scrollbackIDs,
		Scrollback:    canonicalScrollback,
	}, nil
}

func canonicaliseChannelScrollback(
	ctx context.Context,
	tx *sql.Tx,
	records []ChannelScrollbackRecord,
) ([]ChannelScrollbackRecord, error) {
	canonical := make([]ChannelScrollbackRecord, len(records))
	for i, record := range records {
		channel := record.Channel
		err := tx.QueryRowContext(ctx,
			`SELECT name FROM channels WHERE name = ? COLLATE NOCASE ORDER BY name LIMIT 1`,
			channel,
		).Scan(&channel)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("resolve projected channel %q: %w", record.Channel, ErrNoSuchChannel)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve projected channel %q: %w", record.Channel, err)
		}

		record.Channel = channel
		if part, ok := record.Event.(domain.Part); ok {
			part.Target = channel
			record.Event = part
		}
		canonical[i] = record
	}

	return canonical, nil
}

func recordPendingMemoryDeletion(
	ctx context.Context,
	tx *sql.Tx,
	id domain.InstanceID,
) error {
	// The empty instance id belongs to the user, which never has a
	// model memory collection.
	if id == "" {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO pending_memory_deletions (instance_id) VALUES (?)`,
		string(id),
	); err != nil {
		return fmt.Errorf("record pending memory deletion: %w", err)
	}

	return nil
}

func deleteInstanceOwnedContext(
	ctx context.Context,
	tx *sql.Tx,
	id domain.InstanceID,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memories WHERE instance_id = ?`, string(id)); err != nil {
		return fmt.Errorf("delete memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_scrollback WHERE instance_id = ?`, string(id)); err != nil {
		return fmt.Errorf("delete channel scrollback: %w", err)
	}

	if id == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM instance_replies WHERE instance_id = ?`, string(id)); err != nil {
		return fmt.Errorf("delete instance replies: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM model_turns WHERE instance_id = ?`, string(id)); err != nil {
		return fmt.Errorf("delete model turns: %w", err)
	}

	return nil
}

func deleteDirectPeerContext(
	ctx context.Context,
	tx *sql.Tx,
	id domain.InstanceID,
) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM instance_replies
		WHERE window_kind = 2 AND window_key = ? AND instance_id != ?
	`, string(id), string(id)); err != nil {
		return fmt.Errorf("delete direct-window instance replies: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM model_turns
		WHERE window_kind = 2 AND window_key = ? AND instance_id != ?
	`, string(id), string(id)); err != nil {
		return fmt.Errorf("delete direct-window model turns: %w", err)
	}

	return nil
}

func appendCanonicalChannelAuditEvents(
	ctx context.Context,
	tx *sql.Tx,
	events []ChannelAuditEvent,
) error {
	canonical, err := canonicaliseChannelAuditEvents(ctx, tx, events)
	if err != nil {
		return err
	}
	encoded, err := encodeChannelAuditEvents(canonical)
	if err != nil {
		return err
	}

	return appendChannelAuditEvents(ctx, tx, encoded)
}

// deleteInstanceChannelMemberships removes an instance from every
// persisted channel in the same transaction that deletes the instance.
// It treats casing-equivalent rows as one channel when deciding whether
// any members remain.
func deleteInstanceChannelMemberships(ctx context.Context, tx *sql.Tx, id domain.InstanceID) error {
	rows, err := queryRows(ctx, tx,
		`SELECT data FROM channels WHERE json_extract(data, '$.Kind') = ?`,
		[]any{domain.KindChannel}, jsonColumn[channelRow])
	if err != nil {
		return fmt.Errorf("list channel memberships: %w", err)
	}

	for _, group := range channelMembershipGroups(rows, id) {
		if err := applyChannelMembershipGroup(ctx, tx, group); err != nil {
			return err
		}
	}

	return nil
}

type channelMembershipUpdate struct {
	row     *channelRow
	changed bool
}

func channelMembershipGroups(
	rows []channelRow,
	id domain.InstanceID,
) map[domain.ChannelKey][]channelMembershipUpdate {
	groups := make(map[domain.ChannelKey][]channelMembershipUpdate)
	for index := range rows {
		row := &rows[index]
		changed := false
		for member := range row.Members.All() {
			if member.InstanceID != id {
				continue
			}

			row.Members.Remove(member)
			changed = true
			break
		}

		key := domain.KeyForChannel(row.Name)
		groups[key] = append(groups[key], channelMembershipUpdate{row: row, changed: changed})
	}

	return groups
}

func applyChannelMembershipGroup(
	ctx context.Context,
	tx *sql.Tx,
	group []channelMembershipUpdate,
) error {
	groupChanged := false
	membersRemain := false
	for _, update := range group {
		groupChanged = groupChanged || update.changed
		if update.row.Members.Len() > 0 {
			membersRemain = true
		}
	}

	if !groupChanged {
		return nil
	}

	if !membersRemain {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM model_turns
				WHERE window_kind = 1 AND window_key = ? COLLATE NOCASE
		`, group[0].row.Name); err != nil {
			return fmt.Errorf("delete channel model turns for %q: %w", group[0].row.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM channels WHERE name = ? COLLATE NOCASE`, group[0].row.Name,
		); err != nil {
			return fmt.Errorf("delete empty channel %q: %w", group[0].row.Name, err)
		}
		return nil
	}

	for _, update := range group {
		if update.row.Members.Len() == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM channels WHERE name = ? COLLATE BINARY`, update.row.Name,
			); err != nil {
				return fmt.Errorf("delete empty channel row %q: %w", update.row.Name, err)
			}
			continue
		}
		if !update.changed {
			continue
		}

		data, err := json.Marshal(update.row)
		if err != nil {
			return fmt.Errorf("marshal channel %q: %w", update.row.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE channels SET data = ? WHERE name = ? COLLATE BINARY`, string(data), update.row.Name,
		); err != nil {
			return fmt.Errorf("update channel %q: %w", update.row.Name, err)
		}
	}

	return nil
}

// MarkInstancePendingDeletion removes an instance from every lookup
// while retaining its row for a later deletion retry.
func (s *SQLiteStore) MarkInstancePendingDeletion(ctx context.Context, id domain.InstanceID) error {
	err := s.inSpan(ctx, "store.sqlite.mark_instance_pending_deletion",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`UPDATE instances SET pending_deletion = 1 WHERE instance_id = ?`,
				string(id))
		})
	if err != nil {
		return err
	}

	s.forgetInstance(id)

	return nil
}

// ListPendingInstanceDeletions returns the instance ids whose rows
// must be deleted before model clients are restored at startup.
func (s *SQLiteStore) ListPendingInstanceDeletions(ctx context.Context) ([]domain.InstanceID, error) {
	return queryRows(ctx, s.db,
		`SELECT instance_id FROM instances WHERE pending_deletion = 1 ORDER BY instance_id`,
		nil,
		scalarColumn[domain.InstanceID]())
}

// ListPendingMemoryDeletions returns the instance ids whose indexed
// memory collections still need deletion.
func (s *SQLiteStore) ListPendingMemoryDeletions(ctx context.Context) ([]domain.InstanceID, error) {
	return queryRows(ctx, s.db,
		`SELECT instance_id FROM pending_memory_deletions ORDER BY instance_id`,
		nil,
		scalarColumn[domain.InstanceID]())
}

// DeletePendingMemoryDeletion clears an indexed-memory deletion
// after the external collection has been removed.
func (s *SQLiteStore) DeletePendingMemoryDeletion(ctx context.Context, id domain.InstanceID) error {
	return execMutation(ctx, s.db,
		`DELETE FROM pending_memory_deletions WHERE instance_id = ?`,
		string(id))
}

// GetLastWindow implements the chat screen's UI state store. A
// missing row means no saved landing. A present empty value is the
// user's self-DM and returns a typed DM window key.
func (s *SQLiteStore) GetLastWindow(ctx context.Context) (domain.Window, error) {
	var window domain.Window
	err := s.inSpan(ctx, "store.sqlite.get_last_window", nil, func(ctx context.Context, _ trace.Span) error {
		value, present, err := getOptionalState[domain.ChannelName](ctx, s.db, "last_window")
		if err != nil || !present {
			return err
		}

		window = domain.WindowKey(value)
		return nil
	})

	return window, err
}

// SetLastWindow implements the chat screen's UI state store.
func (s *SQLiteStore) SetLastWindow(ctx context.Context, window domain.Window) error {
	return s.inSpan(ctx, "store.sqlite.set_last_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(window.Name()))},
		func(ctx context.Context, _ trace.Span) error {
			return setState(ctx, s.db, "last_window", string(window.Name()))
		})
}

// ClearLastWindow implements the chat screen's UI state store.
func (s *SQLiteStore) ClearLastWindow(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.clear_last_window", nil,
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM state WHERE key = ?`, "last_window")
		})
}

// GetLastRead implements Store.
func (s *SQLiteStore) GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error) {
	var eventID int64
	err := s.inSpan(ctx, "store.sqlite.get_last_read",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			id, err := queryRow(ctx, s.db,
				`SELECT event_id FROM last_read WHERE channel = ?`,
				[]any{ch}, nil, scalarColumn[int64]())
			if errors.Is(err, sql.ErrNoRows) {
				eventID = 0
				return nil
			}
			if err != nil {
				return err
			}

			eventID = id
			return nil
		})

	return eventID, err
}

// SetLastRead implements Store.
func (s *SQLiteStore) SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error {
	return s.inSpan(ctx, "store.sqlite.set_last_read",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(ch))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO last_read (channel, event_id) VALUES (?, ?)
				 ON CONFLICT (channel) DO UPDATE SET event_id = excluded.event_id`,
				ch, eventID)
		})
}

// GetDMLastRead implements Store. Returns 0 when no cursor has been
// recorded for the DM thread with peer, the same "nothing read yet"
// convention GetLastRead uses for a channel.
func (s *SQLiteStore) GetDMLastRead(ctx context.Context, peer domain.InstanceID) (int64, error) {
	var eventID int64
	err := s.inSpan(ctx, "store.sqlite.get_dm_last_read",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(peer))},
		func(ctx context.Context, _ trace.Span) error {
			id, err := queryRow(ctx, s.db,
				`SELECT event_id FROM dm_last_read WHERE instance_id = ?`,
				[]any{peer}, nil, scalarColumn[int64]())
			if errors.Is(err, sql.ErrNoRows) {
				eventID = 0
				return nil
			}
			if err != nil {
				return err
			}

			eventID = id
			return nil
		})

	return eventID, err
}

// SetDMLastRead implements Store, recording the user's read cursor
// for the DM thread with peer. This is last_read's counterpart for a
// DM: last_read.channel references channels(name), and a DM window
// is never a row in that table, so the cursor for a DM thread is
// recorded here instead, keyed by the counterpart's instance id.
func (s *SQLiteStore) SetDMLastRead(ctx context.Context, peer domain.InstanceID, eventID int64) error {
	return s.inSpan(ctx, "store.sqlite.set_dm_last_read",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(peer))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO dm_last_read (instance_id, event_id) VALUES (?, ?)
				 ON CONFLICT (instance_id) DO UPDATE SET event_id = excluded.event_id`,
				peer, eventID)
		})
}

// GetSessionActive implements Store.
func (s *SQLiteStore) GetSessionActive(ctx context.Context) (string, error) {
	var value string
	err := s.inSpan(ctx, "store.sqlite.get_session_active", nil, func(ctx context.Context, _ trace.Span) error {
		var inErr error
		value, inErr = getState[string](ctx, s.db, "session_active")
		return inErr
	})

	return value, err
}

// SetSessionActive implements Store.
func (s *SQLiteStore) SetSessionActive(ctx context.Context, value string) error {
	return s.inSpan(ctx, "store.sqlite.set_session_active", nil, func(ctx context.Context, _ trace.Span) error {
		return setState(ctx, s.db, "session_active", value)
	})
}

// ClearSessionActive implements Store.
func (s *SQLiteStore) ClearSessionActive(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.clear_session_active", nil, func(ctx context.Context, _ trace.Span) error {
		return execMutation(ctx, s.db, `DELETE FROM state WHERE key = ?`, "session_active")
	})
}

// ListAutojoinChannels implements Store.
func (s *SQLiteStore) ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error) {
	var channels []domain.ChannelName
	err := s.inSpan(ctx, "store.sqlite.list_autojoin_channels", nil, func(ctx context.Context, _ trace.Span) error {
		got, err := queryRows(ctx, s.db,
			`SELECT name FROM autojoin ORDER BY name`, nil,
			scalarColumn[domain.ChannelName]())
		if err != nil {
			return err
		}

		channels = got
		return nil
	})

	return channels, err
}

// SetAutojoinChannels implements Store.
func (s *SQLiteStore) SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error {
	return s.inSpan(ctx, "store.sqlite.set_autojoin_channels", nil, func(ctx context.Context, _ trace.Span) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, `DELETE FROM autojoin`); err != nil {
			return fmt.Errorf("clear autojoin: %w", err)
		}

		for _, ch := range channels {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO autojoin (name) VALUES (?)`, ch); err != nil {
				return fmt.Errorf("insert autojoin %q: %w", ch, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		return nil
	})
}

// Reset implements Store. Clears every table this store owns and
// invalidates the canonical instance registry so a subsequent load
// of a since-recreated id never hands back a stale pre-Reset handle.
func (s *SQLiteStore) Reset(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.reset", nil, func(ctx context.Context, _ trace.Span) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		defer func() { _ = tx.Rollback() }()

		// Order: children before parents (last_read, dm_last_read →
		// channels, events; dm_windows, instance_replies, memories →
		// instances).
		for _, stmt := range []string{
			`DELETE FROM last_read`,
			`DELETE FROM dm_last_read`,
			`DELETE FROM channels`,
			`DELETE FROM events`,
			`DELETE FROM dm_windows`,
			`DELETE FROM instance_replies`,
			`DELETE FROM model_turn_entries`,
			`DELETE FROM model_turns`,
			`DELETE FROM pending_memory_deletions`,
			`DELETE FROM instances`,
			`DELETE FROM memories`,
			`DELETE FROM personas`,
			`DELETE FROM state`,
			`DELETE FROM autojoin`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("reset: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		s.instancesMu.Lock()
		s.instances = make(map[domain.InstanceID]*domain.Instance)
		s.instancesMu.Unlock()

		return nil
	})
}

// inSpan brackets fn with a span and result-recording on the store's
// tracer provider. See `observability.SpanRunner` for the wrapper's
// shape; `sql.ErrNoRows` (anywhere in the returned error's chain) is
// classified as `ErrorKindNotFound` so dashboards can separate
// missing-row outcomes from infrastructure failures, and every other
// error falls back to `ErrorKindStore`.
func (s *SQLiteStore) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         s.tracerProvider.Tracer("github.com/laney/modeloff/internal/store"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifyStoreError,
	}.Run(ctx, op, attrs, fn)
}

func classifyStoreError(err error) string {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNoSuchNick) || errors.Is(err, ErrNoSuchChannel) {
		return observability.ErrorKindNotFound
	}

	return ""
}

func getState[T ~string](ctx context.Context, db *sql.DB, key string) (T, error) {
	value, err := queryRow(ctx, db,
		`SELECT value FROM state WHERE key = ?`,
		[]any{key}, nil, scalarColumn[string]())
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	return T(value), nil
}

func getOptionalState[T ~string](ctx context.Context, db *sql.DB, key string) (T, bool, error) {
	value, err := queryRow(ctx, db,
		`SELECT value FROM state WHERE key = ?`,
		[]any{key}, nil, scalarColumn[string]())
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	return T(value), true, nil
}

func setState(ctx context.Context, db *sql.DB, key, value string) error {
	return execMutation(ctx, db,
		`INSERT INTO state (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
}

// scalarColumn returns a decoder that scans a single value into a
// caller-supplied destination type. Use for the bare-column queries
// that don't fit `jsonColumn` — autojoin names, last-read event
// ids, single-row scalar lookups.
func scalarColumn[T any]() func(rowScanner) (T, error) {
	return func(r rowScanner) (T, error) {
		var v T
		if err := r.Scan(&v); err != nil {
			return v, err
		}

		return v, nil
	}
}
