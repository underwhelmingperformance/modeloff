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
// requires (`busy_timeout`, `journal_mode`, `foreign_keys`) to the
// given filename or `file:` URI. The `ncruces/go-sqlite3` driver
// applies `_pragma=` parameters on every connection it opens, so any
// pool size sees the same configuration. Order matters per the driver
// docs: `busy_timeout` and the locking mode must come first.
func SQLitePragmaDSN(path string) string {
	dsn := path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return dsn + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
}

// NewDefaultSQLiteStore creates a SQLiteStore using the XDG data
// directory ($XDG_DATA_HOME/modeloff/modeloff.db).
func NewDefaultSQLiteStore(ctx context.Context) (*SQLiteStore, error) {
	dir := filepath.Join(xdg.DataHome, "modeloff")

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(filepath.Join(dir, "modeloff.db")))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

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

	return &SQLiteStore{
		db:             db,
		instances:      make(map[domain.InstanceID]*domain.Instance),
		tracerProvider: otel.GetTracerProvider(),
	}, nil
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
// `*StatusWindow` / `*ChannelWindow` / `*DMWindow` constructed from
// the row by `rowToWindow`. Per-kind state that doesn't apply to a
// given row is left zero (a status row carries no members or
// topic; a DM row's member list is empty and `Name` is the
// counterpart's `InstanceID`).
type channelRow struct {
	Name         domain.ChannelName
	Kind         domain.ChannelKind
	Topic        string
	TopicSetBy   domain.Nick
	TopicSetAt   time.Time
	Members      domain.MemberList
	Modes        domain.ChannelModes
	InvitedNicks domain.InvitedNicks
	Created      time.Time
}

// resolveChannelMembers rewrites the stub `*Instance` handles in
// the row's member list (set by MemberList.UnmarshalJSON) to
// canonical pointers from the registry, loading any missing
// instances from SQLite in a single batch. Member rows that refer
// to an instance with no backing row are dropped from the list and
// logged — a leftover from a previous session where the instance
// was deleted but the channel's membership record still carried
// the id. Only channel-kind rows carry members; status and DM rows
// short-circuit at the empty-list check.
func (s *SQLiteStore) resolveChannelMembers(ctx context.Context, row *channelRow) error {
	if row.Members.Len() == 0 {
		return nil
	}

	// Gather the ids carried by stubs that aren't already in the
	// registry. The human user's instance (empty id) is ignored
	// because the session constructs it on its own at Connect time
	// and seeds the registry before loading channels.
	var missing []domain.InstanceID
	seen := make(map[domain.InstanceID]struct{})

	for m := range row.Members.All() {
		id := m.Instance.ID()

		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}

		if id == "" {
			continue
		}

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

	// Rewrite stubs; any id that still resolves to nil references a
	// deleted instance and is dropped.
	dropped := make([]domain.InstanceID, 0)
	row.Members.ResolveInstances(func(id domain.InstanceID) *domain.Instance {
		canonical := s.resolveInstance(id)
		if canonical == nil {
			dropped = append(dropped, id)
		}

		return canonical
	})

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
		WHERE instance_id IN (SELECT value FROM json_each(?))
	`, []any{string(idsJSON)}, jsonColumn[domain.Instance])
	if err != nil {
		return fmt.Errorf("load instances: %w", err)
	}

	for i := range fresh {
		s.canonicaliseInstance(&fresh[i])
	}

	return nil
}

// resolveDMCounterpart resolves a DM window's stored
// `InstanceID` to the canonical `*Instance` handle through the
// store's registry, so pointer comparison against handles held
// by other paths stays valid. Returns nil for unknown ids;
// `rowToWindow` promotes nil into a typed error so the caller
// can drop the row and log.
func (s *SQLiteStore) resolveDMCounterpart(ctx context.Context, id domain.InstanceID) *domain.Instance {
	if cached := s.resolveInstance(id); cached != nil {
		return cached
	}

	inst, err := s.GetInstanceByID(ctx, id)
	if err != nil {
		return nil
	}

	return inst
}

// rowToWindow projects a decoded on-disk row to its matching
// concrete `Window`. DMs resolve their counterpart `*Instance`
// through the registry; an unresolved counterpart returns
// `domain.MissingDMCounterpartError` so the caller can drop the
// row and log.
func (s *SQLiteStore) rowToWindow(ctx context.Context, row channelRow) (domain.Window, error) {
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
		cw.InvitedNicks = row.InvitedNicks
		return cw, nil

	case domain.KindDM:
		counterpart := s.resolveDMCounterpart(ctx, domain.InstanceID(row.Name))
		if counterpart == nil {
			return nil, domain.MissingDMCounterpartError{InstanceID: domain.InstanceID(row.Name)}
		}

		dm := domain.NewDMWindow(counterpart, row.Created)
		return dm, nil

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
		row.InvitedNicks = cw.InvitedNicks
	}

	return row
}

// ListWindows implements Store. The returned slice carries one
// concrete `Window` per row in `channels`. Rows whose DM
// counterpart no longer resolves are dropped from the result and
// logged.
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

			w, err := s.rowToWindow(ctx, rows[i])
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

// GetWindow implements Store. Returns the typed concrete `Window`
// for the given name. DMs come back with their `Counterpart`
// resolved through the canonical registry; a missing counterpart
// surfaces as an error so the caller can decide whether to recover
// or surface to the user.
func (s *SQLiteStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	var w domain.Window
	err := s.inSpan(ctx, "store.sqlite.get_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(name))},
		func(ctx context.Context, _ trace.Span) error {
			row, err := queryRow(ctx, s.db,
				`SELECT data FROM channels WHERE name = ?`,
				[]any{name}, ErrNoSuchChannel,
				jsonColumn[channelRow])
			if err != nil {
				return fmt.Errorf("channel %q: %w", name, err)
			}

			if err := s.resolveChannelMembers(ctx, &row); err != nil {
				return err
			}

			w, err = s.rowToWindow(ctx, row)
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

// DeleteWindow implements Store.
func (s *SQLiteStore) DeleteWindow(ctx context.Context, name domain.ChannelName) error {
	return s.inSpan(ctx, "store.sqlite.delete_window",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(name))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM channels WHERE name = ?`, name)
		})
}

// AppendEvent implements Store.
func (s *SQLiteStore) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) (int64, error) {
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

			got, err := queryRows(ctx, s.db, query, args, storedEventRow)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// DMEventsBefore implements Store. Returns the DM thread
// between `self` and `peer`: bidirectional message rows plus
// peer's actor-scoped events from any channel, deduped by
// `(instance_id, type, at)`. Either id may be the empty string
// (the user). The `coalesce` over the JSON `instance_id` path
// reads an absent field (the `omitzero` shape for empty ids)
// the same as a present empty string. Rows come back
// chronological via inner-desc / outer-asc.
func (s *SQLiteStore) DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error) {
	var events []domain.StoredEvent
	err := s.inSpan(ctx, "store.sqlite.dm_events_before",
		[]attribute.KeyValue{
			attribute.String(observability.AttrInstanceID, string(self)),
			attribute.String("modeloff.dm.peer_id", string(peer)),
		},
		func(ctx context.Context, _ trace.Span) error {
			// Bidirectional message rows: peer→self and self→peer.
			const messageRows = `SELECT id, data FROM events WHERE
				(channel = ? AND coalesce(json_extract(data, '$.data.instance_id'), '') = ?)
				OR
				(channel = ? AND coalesce(json_extract(data, '$.data.instance_id'), '') = ?)
			`

			// Peer's actor-scoped events anywhere, deduped by
			// (instance_id, type, at). Per-channel persistence
			// (one row per channel the actor was in at event
			// time) collapses to one representative row via
			// MIN(id).
			const actorEventRows = `SELECT MIN(id) AS id, data FROM events
				WHERE coalesce(json_extract(data, '$.data.instance_id'), '') = ?
					AND json_extract(data, '$.type') IN ('quit', 'nick_change')
				GROUP BY json_extract(data, '$.data.instance_id'),
					json_extract(data, '$.type'),
					json_extract(data, '$.data.at')
			`

			union := `(` + messageRows + ` UNION ` + actorEventRows + `)`

			query, args := `SELECT id, data FROM (
					SELECT id, data FROM `+union+` AS thread
					ORDER BY id DESC LIMIT ?
				) ORDER BY id ASC`,
				[]any{string(peer), string(self), string(self), string(peer), string(peer), n}
			if before != nil {
				query, args = `SELECT id, data FROM (
						SELECT id, data FROM `+union+` AS thread WHERE id < ?
						ORDER BY id DESC LIMIT ?
					) ORDER BY id ASC`,
					[]any{string(peer), string(self), string(self), string(peer), string(peer), *before, n}
			}

			got, err := queryRows(ctx, s.db, query, args, storedEventRow)
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

			got, err := queryRows(ctx, s.db, query, args, storedEventRow)
			if err != nil {
				return err
			}

			events = got
			return nil
		})

	return events, err
}

// ListInstances implements Store. Returns canonical `*Instance`
// pointers from the registry; callers that called `GetInstanceByID`
// previously observe the same pointers.
func (s *SQLiteStore) ListInstances(ctx context.Context) ([]*domain.Instance, error) {
	var instances []*domain.Instance
	err := s.inSpan(ctx, "store.sqlite.list_instances", nil, func(ctx context.Context, _ trace.Span) error {
		fresh, err := queryRows(ctx, s.db,
			`SELECT data FROM instances ORDER BY nick`, nil,
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
				`SELECT data FROM instances WHERE instance_id = ?`,
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
// If multiple instances share the same display nick the store
// returns one arbitrary matching row — the `idx_instances_nick`
// index is non-unique because display nicks are expected to drift.
// Callers are responsible for preventing collisions upstream (the
// session refuses renames that would collide; see task #53).
func (s *SQLiteStore) ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	var inst *domain.Instance
	err := s.inSpan(ctx, "store.sqlite.resolve_nick",
		[]attribute.KeyValue{attribute.String(observability.AttrNick, string(nick))},
		func(ctx context.Context, _ trace.Span) error {
			fresh, err := queryRow(ctx, s.db,
				`SELECT data FROM instances WHERE nick = ?`,
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

// DeleteInstanceByID implements Store. Evicts the row from SQLite
// and the handle from the canonical registry.
func (s *SQLiteStore) DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error {
	return s.inSpan(ctx, "store.sqlite.delete_instance_by_id",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			if err := execMutation(ctx, s.db, `DELETE FROM instances WHERE instance_id = ?`, string(id)); err != nil {
				return err
			}

			s.forgetInstance(id)
			return nil
		})
}

// GetLastChannel implements Store.
func (s *SQLiteStore) GetLastChannel(ctx context.Context) (domain.ChannelName, error) {
	var value domain.ChannelName
	err := s.inSpan(ctx, "store.sqlite.get_last_channel", nil, func(ctx context.Context, _ trace.Span) error {
		var inErr error
		value, inErr = getState[domain.ChannelName](ctx, s.db, "last_channel")
		return inErr
	})

	return value, err
}

// SetLastChannel implements Store.
func (s *SQLiteStore) SetLastChannel(ctx context.Context, name domain.ChannelName) error {
	return s.inSpan(ctx, "store.sqlite.set_last_channel",
		[]attribute.KeyValue{attribute.String(observability.AttrChannel, string(name))},
		func(ctx context.Context, _ trace.Span) error {
			return setState(ctx, s.db, "last_channel", string(name))
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

// ReadMemories implements Store.
func (s *SQLiteStore) ReadMemories(ctx context.Context, id domain.InstanceID) ([]MemoryEntry, error) {
	var entries []MemoryEntry
	err := s.inSpan(ctx, "store.sqlite.read_memories",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRows(ctx, s.db,
				`SELECT key, content FROM memories WHERE instance_id = ? ORDER BY key`,
				[]any{string(id)},
				func(r rowScanner) (MemoryEntry, error) {
					var e MemoryEntry
					return e, r.Scan(&e.Key, &e.Content)
				})
			if err != nil {
				return err
			}

			entries = got
			return nil
		})

	return entries, err
}

// WriteMemory implements Store.
func (s *SQLiteStore) WriteMemory(ctx context.Context, id domain.InstanceID, key, content string) error {
	return s.inSpan(ctx, "store.sqlite.write_memory",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO memories (instance_id, key, content) VALUES (?, ?, ?)
				 ON CONFLICT (instance_id, key) DO UPDATE SET content = excluded.content`,
				string(id), key, content)
		})
}

// DeleteMemory implements Store.
func (s *SQLiteStore) DeleteMemory(ctx context.Context, id domain.InstanceID, key string) error {
	return s.inSpan(ctx, "store.sqlite.delete_memory",
		[]attribute.KeyValue{attribute.String(observability.AttrInstanceID, string(id))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM memories WHERE instance_id = ? AND key = ?`, string(id), key)
		})
}

// ListPersonas implements Store.
func (s *SQLiteStore) ListPersonas(ctx context.Context) ([]domain.Persona, error) {
	var personas []domain.Persona
	err := s.inSpan(ctx, "store.sqlite.list_personas", nil, func(ctx context.Context, _ trace.Span) error {
		got, err := queryRows(ctx, s.db,
			`SELECT id, description, origin FROM personas ORDER BY id`, nil,
			personaRow)
		if err != nil {
			return err
		}

		personas = got
		return nil
	})

	return personas, err
}

// GetPersona implements Store.
func (s *SQLiteStore) GetPersona(ctx context.Context, id string) (domain.Persona, error) {
	var p domain.Persona
	err := s.inSpan(ctx, "store.sqlite.get_persona",
		[]attribute.KeyValue{attribute.String("persona.id", id)},
		func(ctx context.Context, _ trace.Span) error {
			got, err := queryRow(ctx, s.db,
				`SELECT id, description, origin FROM personas WHERE id = ?`,
				[]any{id}, nil, personaRow)
			if err != nil {
				return fmt.Errorf("persona %q: %w", id, err)
			}

			p = got
			return nil
		})

	return p, err
}

// SavePersona implements Store.
func (s *SQLiteStore) SavePersona(ctx context.Context, p domain.Persona) error {
	return s.inSpan(ctx, "store.sqlite.save_persona",
		[]attribute.KeyValue{attribute.String("persona.id", p.ID)},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db,
				`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
				 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
				p.ID, p.Description, p.Origin)
		})
}

// DeletePersonasByOrigin implements Store.
func (s *SQLiteStore) DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error {
	return s.inSpan(ctx, "store.sqlite.delete_personas_by_origin",
		[]attribute.KeyValue{attribute.String("persona.origin", string(origin))},
		func(ctx context.Context, _ trace.Span) error {
			return execMutation(ctx, s.db, `DELETE FROM personas WHERE origin = ?`, origin)
		})
}

// personaRow decodes the (id, description, origin) shape used by
// every personas-table query.
func personaRow(r rowScanner) (domain.Persona, error) {
	var p domain.Persona
	return p, r.Scan(&p.ID, &p.Description, &p.Origin)
}

// ReplaceGeneratedPersonas implements Store. It atomically deletes all
// generated personas and inserts the given replacements in a single
// transaction.
func (s *SQLiteStore) ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error {
	return s.inSpan(ctx, "store.sqlite.replace_generated_personas", nil, func(ctx context.Context, _ trace.Span) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, `DELETE FROM personas WHERE origin = ?`, domain.PersonaGenerated); err != nil {
			return fmt.Errorf("delete generated: %w", err)
		}

		for _, p := range personas {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO personas (id, description, origin) VALUES (?, ?, ?)
				 ON CONFLICT (id) DO UPDATE SET description = excluded.description, origin = excluded.origin`,
				p.ID, p.Description, p.Origin); err != nil {
				return fmt.Errorf("insert persona %q: %w", p.ID, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		return nil
	})
}

// ResetMemories implements Store.
func (s *SQLiteStore) ResetMemories(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.reset_memories", nil, func(ctx context.Context, _ trace.Span) error {
		return execMutation(ctx, s.db, `DELETE FROM memories`)
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

// Reset implements Store.
func (s *SQLiteStore) Reset(ctx context.Context) error {
	return s.inSpan(ctx, "store.sqlite.reset", nil, func(ctx context.Context, _ trace.Span) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}

		defer func() { _ = tx.Rollback() }()

		// Order: children before parents (last_read → channels, events).
		for _, stmt := range []string{
			`DELETE FROM last_read`,
			`DELETE FROM channels`,
			`DELETE FROM events`,
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

func setState(ctx context.Context, db *sql.DB, key, value string) error {
	return execMutation(ctx, db,
		`INSERT INTO state (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
}

// storedEventRow decodes the (id, data) shape used by every event-log
// query. Returns a `domain.StoredEvent` with the unmarshalled
// payload.
func storedEventRow(r rowScanner) (domain.StoredEvent, error) {
	var (
		id   int64
		data string
	)

	if err := r.Scan(&id, &data); err != nil {
		return domain.StoredEvent{}, err
	}

	event, err := domain.UnmarshalPersistableEvent([]byte(data))
	if err != nil {
		return domain.StoredEvent{}, err
	}

	return domain.StoredEvent{ID: id, Event: event}, nil
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
