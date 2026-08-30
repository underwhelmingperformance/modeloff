package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// SchemaVersion is the current on-disk schema version. Bumped by
// exactly one whenever a schema change is needed; the corresponding
// [migration] entry brings a v(N-1) database forward to vN.
//
// `schema` (sqlite.go) only ever describes the v1 shape and seeds
// `state.schema_version` to '1' via `INSERT OR IGNORE`; it never grows
// the v2+ shape directly. [NewSQLiteStore] runs it for a database that
// records no version, and applyMigrations then brings every database,
// fresh or pre-existing, to this version: that is the single path from
// v1 onward. If `schema` carried a v2+ column or index, a fresh
// database would receive it before the migration meant to introduce
// it ran, and that migration would fail on an object already there.
const SchemaVersion = 22

type schemaTooNewError struct {
	Found     int
	Supported int
}

func (e *schemaTooNewError) Error() string {
	return fmt.Sprintf(
		"store schema is v%d but this build expects v%d; downgrades aren't supported",
		e.Found, e.Supported,
	)
}

type missingMigrationError struct {
	From int
	To   int
}

func (e *missingMigrationError) Error() string {
	return fmt.Sprintf(
		"store schema is v%d with no migration to reach v%d; delete the store file to start fresh",
		e.From, e.To,
	)
}

// migration is one forward-only step that brings the database
// from v(Version-1) to vVersion. Apply runs inside the
// transaction [applyMigrations] opens, so a mid-chain failure
// rolls every applied step back.
type migration struct {
	Version int
	Apply   func(ctx context.Context, tx *sql.Tx) error
}

type migrationStatement struct {
	name string
	sql  string
}

// migrations is the ordered registry of forward-only steps. v1 is
// the first cut and nothing predates it. Future schema changes
// append entries with strictly increasing `Version`.
var migrations = []migration{
	{
		Version: 2,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// dm_instance_id gives DMEventsBefore's thread lookup a
			// real column, so idx_events_dm_thread can serve it as an
			// index seek.
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE events ADD COLUMN dm_instance_id TEXT GENERATED ALWAYS AS
					(coalesce(json_extract(data, '$.data.instance_id'), '')) VIRTUAL
			`); err != nil {
				return fmt.Errorf("add events.dm_instance_id: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_events_dm_thread
					ON events (dm_instance_id, type, channel, id)
			`); err != nil {
				return fmt.Errorf("create idx_events_dm_thread: %w", err)
			}

			// dm_windows holds the user-client's set of open DM
			// windows, keyed by the counterpart model instance's id.
			// Client-owned data: the user-client reads and writes
			// this table directly; the session dispatcher never
			// touches it. Deleting the counterpart instance cascades
			// to drop its DM window entry too.
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS dm_windows (
					instance_id TEXT PRIMARY KEY REFERENCES instances(instance_id) ON DELETE CASCADE
				)
			`); err != nil {
				return fmt.Errorf("create dm_windows: %w", err)
			}

			return nil
		},
	},
	{
		Version: 3,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// The server casemaps nicks and channel names before
			// comparing them (RFC 2812 §2.2), so `ResolveNick` and
			// `GetWindow` match under NOCASE. Both columns already
			// carry a BINARY index, which a NOCASE comparison cannot
			// use; these index the folded form so the lookups stay
			// index seeks.
			//
			// Neither index is UNIQUE. Uniqueness is the session's to
			// enforce, on its command loop, where a nick is claimed and
			// a channel is created, which is the convention
			// `idx_instances_nick` already follows. A UNIQUE index
			// would also refuse to be created against a database
			// written before the casemapping existed, which may
			// already hold `#Dev` alongside `#dev`, and failing the
			// migration would leave that user unable to start at all.
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_instances_nick_nocase
					ON instances (nick COLLATE NOCASE)
			`); err != nil {
				return fmt.Errorf("create idx_instances_nick_nocase: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_channels_name_nocase
					ON channels (name COLLATE NOCASE)
			`); err != nil {
				return fmt.Errorf("create idx_channels_name_nocase: %w", err)
			}

			return nil
		},
	},
	{
		Version: 4,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// last_read.channel references channels(name), and a DM
			// window is never a row in that table, so a DM's cursor
			// cannot be recorded there. dm_last_read is a second cursor
			// table keyed by the counterpart's instance id instead,
			// mirroring dm_windows: deleting the counterpart drops its
			// cursor along with it, the same way deleting a channel
			// already drops that channel's row in last_read.
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS dm_last_read (
					instance_id TEXT PRIMARY KEY REFERENCES instances(instance_id) ON DELETE CASCADE,
					event_id    INTEGER NOT NULL REFERENCES events(id)
				)
			`); err != nil {
				return fmt.Errorf("create dm_last_read: %w", err)
			}

			return nil
		},
	},
	{
		Version: 5,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// memories carried no per-entry write time, so nothing
			// could prefer a recently-written memory over a stale
			// one when a caller caps how many flow into a prompt.
			// A row written before this column existed gets the
			// default empty string, which parseMemoryAt reads back
			// as the zero time: the oldest possible entry, rather
			// than a fabricated write time it never actually had.
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE memories ADD COLUMN at TEXT NOT NULL DEFAULT ''
			`); err != nil {
				return fmt.Errorf("add memories.at: %w", err)
			}

			return nil
		},
	},
	{
		Version: 6,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE instances ADD COLUMN pending_deletion INTEGER NOT NULL DEFAULT 0
			`); err != nil {
				return fmt.Errorf("add instances.pending_deletion: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE pending_memory_deletions (
					instance_id TEXT PRIMARY KEY
				)
			`); err != nil {
				return fmt.Errorf("create pending_memory_deletions: %w", err)
			}

			return nil
		},
	},
	{
		Version: 7,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// Channel events are stored before recipient-specific wire
			// projection. This table records the projected event each
			// member received during its current membership interval. An old
			// row does not record the recipient or the channel modes that
			// controlled its delivery, so this migration cannot copy it
			// verbatim. v9 performs a conservative content-only backfill.
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS channel_scrollback (
					id          INTEGER PRIMARY KEY,
					instance_id TEXT NOT NULL,
					channel     TEXT NOT NULL REFERENCES channels(name) ON DELETE CASCADE,
					type        TEXT NOT NULL,
					data        TEXT NOT NULL,
					at          TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create channel_scrollback: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_channel_scrollback_actor_window
					ON channel_scrollback (instance_id, channel, id)
			`); err != nil {
				return fmt.Errorf("index channel_scrollback: %w", err)
			}

			return nil
		},
	},
	{
		Version: 8,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_events_dm_thread`); err != nil {
				return fmt.Errorf("drop obsolete idx_events_dm_thread: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE events ADD COLUMN source_instance_id TEXT GENERATED ALWAYS AS
					(coalesce(
						json_extract(data, '$.data.source.instance_id'),
						json_extract(data, '$.data.instance_id'),
						''
					)) VIRTUAL
			`); err != nil {
				return fmt.Errorf("add events.source_instance_id: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_events_source_thread
					ON events (source_instance_id, type, channel, id)
			`); err != nil {
				return fmt.Errorf("create idx_events_source_thread: %w", err)
			}

			return nil
		},
	},
	{
		Version: 9,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE instance_replies ADD COLUMN window_kind INTEGER NOT NULL DEFAULT 0
			`); err != nil {
				return fmt.Errorf("add instance_replies.window_kind: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE instance_replies ADD COLUMN window_key TEXT NOT NULL DEFAULT ''
			`); err != nil {
				return fmt.Errorf("add instance_replies.window_key: %w", err)
			}

			// Old LIST rows do not record the window in which LIST ran.
			// Other non-global replies with an empty channel are also
			// ambiguous because the user DM uses the empty instance id.
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM instance_replies
				 WHERE type = 'list_reply'
				    OR (type != 'personas_list'
				        AND coalesce(json_extract(data, '$.data.channel'), '') = '')
			`); err != nil {
				return fmt.Errorf("remove replies with ambiguous origin: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE instance_replies
				   SET window_key = coalesce(json_extract(data, '$.data.channel'), ''),
				       window_kind = CASE
				           WHEN json_extract(data, '$.data.channel') GLOB '[#&]*' THEN 1
				           WHEN coalesce(json_extract(data, '$.data.channel'), '') != '' THEN 2
				           ELSE 0
				       END
				 WHERE type != 'personas_list'
			`); err != nil {
				return fmt.Errorf("scope existing instance replies: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_instance_replies_actor_window
					ON instance_replies (instance_id, window_kind, window_key, id)
			`); err != nil {
				return fmt.Errorf("index instance reply windows: %w", err)
			}

			return nil
		},
	},
	{
		Version: 10,
		Apply:   backfillSafeChannelScrollback,
	},
	{
		Version: 11,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS model_turns (
					id          INTEGER PRIMARY KEY,
					instance_id TEXT NOT NULL,
					window_kind INTEGER NOT NULL,
					window_key  TEXT NOT NULL,
					model_id    TEXT NOT NULL,
					started_at  TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create model_turns: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_model_turns_actor_window
					ON model_turns (instance_id, window_kind, window_key, id)
			`); err != nil {
				return fmt.Errorf("index model turns: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE IF NOT EXISTS model_turn_entries (
					id      INTEGER PRIMARY KEY,
					turn_id INTEGER NOT NULL REFERENCES model_turns(id) ON DELETE CASCADE,
					seq     INTEGER NOT NULL,
					kind    TEXT NOT NULL,
					data    TEXT NOT NULL,
					at      TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create model_turn_entries: %w", err)
			}

			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_model_turn_entries_turn
					ON model_turn_entries (turn_id, seq)
			`); err != nil {
				return fmt.Errorf("index model turn entries: %w", err)
			}

			return nil
		},
	},
	{
		Version: 12,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE context_summary_sources (
					id          INTEGER PRIMARY KEY,
					instance_id TEXT NOT NULL,
					window_kind INTEGER NOT NULL,
					window_key  TEXT NOT NULL,
					data        TEXT NOT NULL,
					at          TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create context summary sources: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX idx_context_summary_sources_actor_window
					ON context_summary_sources (instance_id, window_kind, window_key, id)
			`); err != nil {
				return fmt.Errorf("index context summary sources: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE context_summaries (
					id              INTEGER PRIMARY KEY,
					instance_id     TEXT NOT NULL,
					window_kind     INTEGER NOT NULL,
					window_key      TEXT NOT NULL,
					summary         TEXT NOT NULL,
					first_source_id INTEGER NOT NULL REFERENCES context_summary_sources(id),
					last_source_id  INTEGER NOT NULL REFERENCES context_summary_sources(id),
					created_at      TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create context summaries: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX idx_context_summaries_actor_window
					ON context_summaries (instance_id, window_kind, window_key, id)
			`); err != nil {
				return fmt.Errorf("index context summaries: %w", err)
			}

			return nil
		},
	},
	{
		Version: 13,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				ALTER TABLE memories ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0
			`); err != nil {
				return fmt.Errorf("add memories.pinned: %w", err)
			}

			return nil
		},
	},
	{
		Version: 14,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE persona_revisions (
					id          INTEGER PRIMARY KEY,
					instance_id TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
					parent_id   INTEGER REFERENCES persona_revisions(id),
					description TEXT NOT NULL,
					created_at  TEXT NOT NULL
				)
			`); err != nil {
				return fmt.Errorf("create persona revisions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX idx_persona_revisions_instance
					ON persona_revisions (instance_id, id)
			`); err != nil {
				return fmt.Errorf("index persona revisions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE persona_lineages (
					instance_id         TEXT PRIMARY KEY REFERENCES instances(instance_id) ON DELETE CASCADE,
					baseline            TEXT NOT NULL,
					current_revision_id INTEGER NOT NULL REFERENCES persona_revisions(id),
					checkpoint          INTEGER NOT NULL DEFAULT 0,
					created_at          TEXT NOT NULL,
					reflected_at        TEXT
				)
			`); err != nil {
				return fmt.Errorf("create persona lineages: %w", err)
			}

			zeroTime := formatTime(time.Time{})
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO persona_revisions
					(instance_id, parent_id, description, created_at)
				SELECT instance_id, NULL,
				       coalesce(json_extract(data, '$.Persona'), ''), ?
				FROM instances
				WHERE coalesce(json_extract(data, '$.ModelID'), '') != ''
				ORDER BY instance_id
			`, zeroTime); err != nil {
				return fmt.Errorf("backfill persona revisions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO persona_lineages
					(instance_id, baseline, current_revision_id, checkpoint, created_at)
				SELECT instance_id,
				       coalesce(json_extract(data, '$.Persona'), ''),
				       (SELECT id FROM persona_revisions
				         WHERE persona_revisions.instance_id = instances.instance_id
				         ORDER BY id LIMIT 1),
				       0,
				       ?
				FROM instances
				WHERE coalesce(json_extract(data, '$.ModelID'), '') != ''
				ORDER BY instance_id
			`, zeroTime); err != nil {
				return fmt.Errorf("backfill persona lineages: %w", err)
			}

			return nil
		},
	},
	{
		Version: 15,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			statements := []migrationStatement{
				{"create persona experiences", `
					CREATE TABLE persona_experiences (
						id          INTEGER PRIMARY KEY,
						instance_id TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
						kind        TEXT NOT NULL,
						summary     TEXT NOT NULL,
						subject_id  TEXT,
						confidence  TEXT NOT NULL,
						occurred_at TEXT NOT NULL,
						created_at  TEXT NOT NULL
					)
				`},
				{"index persona experiences", `
					CREATE INDEX idx_persona_experiences_instance
						ON persona_experiences (instance_id, id)
				`},
				{"create persona experience sources", `
					CREATE TABLE persona_experience_sources (
						experience_id INTEGER NOT NULL REFERENCES persona_experiences(id) ON DELETE CASCADE,
						sequence      INTEGER NOT NULL,
						PRIMARY KEY (experience_id, sequence)
					)
				`},
				{"create persona amendments", `
					CREATE TABLE persona_amendments (
						id            INTEGER PRIMARY KEY,
						instance_id   TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
						scope         TEXT NOT NULL,
						counterpart_id TEXT,
						tendency      TEXT NOT NULL,
						confidence    TEXT NOT NULL,
						created_at    TEXT NOT NULL,
						expires_at    TEXT,
						supersedes_id INTEGER REFERENCES persona_amendments(id),
						consolidated_at TEXT
					)
				`},
				{"index persona amendments", `
					CREATE INDEX idx_persona_amendments_instance
						ON persona_amendments (instance_id, id)
				`},
				{"create persona amendment evidence", `
					CREATE TABLE persona_amendment_evidence (
						amendment_id INTEGER NOT NULL REFERENCES persona_amendments(id) ON DELETE CASCADE,
						experience_id INTEGER NOT NULL REFERENCES persona_experiences(id),
						PRIMARY KEY (amendment_id, experience_id)
					)
				`},
				{"create persona description evidence", `
					CREATE TABLE persona_description_evidence (
						revision_id INTEGER NOT NULL REFERENCES persona_revisions(id) ON DELETE CASCADE,
						experience_id INTEGER NOT NULL REFERENCES persona_experiences(id),
						PRIMARY KEY (revision_id, experience_id)
					)
				`},
				{"create persona revision experiences", `
					CREATE TABLE persona_revision_experiences (
						revision_id INTEGER NOT NULL REFERENCES persona_revisions(id) ON DELETE CASCADE,
						experience_id INTEGER NOT NULL REFERENCES persona_experiences(id),
						PRIMARY KEY (revision_id, experience_id)
					)
				`},
				{"create persona revision amendments", `
					CREATE TABLE persona_revision_amendments (
						revision_id INTEGER NOT NULL REFERENCES persona_revisions(id) ON DELETE CASCADE,
						amendment_id INTEGER NOT NULL REFERENCES persona_amendments(id),
						PRIMARY KEY (revision_id, amendment_id)
					)
				`},
				{"create persona transitions", `
					CREATE TABLE persona_transitions (
						id               INTEGER PRIMARY KEY,
						instance_id      TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
						from_revision_id INTEGER NOT NULL REFERENCES persona_revisions(id),
						to_revision_id   INTEGER NOT NULL REFERENCES persona_revisions(id),
						kind             TEXT NOT NULL,
						at               TEXT NOT NULL
					)
				`},
				{"create reflection runs", `
					CREATE TABLE reflection_runs (
						id                   TEXT PRIMARY KEY,
						instance_id          TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
						base_revision_id     INTEGER NOT NULL REFERENCES persona_revisions(id),
						prior_checkpoint     INTEGER NOT NULL,
						high_water_mark      INTEGER NOT NULL,
						result_revision_id   INTEGER NOT NULL REFERENCES persona_revisions(id),
						model_id             TEXT NOT NULL,
						outcome              TEXT NOT NULL,
						rejection_reason     TEXT NOT NULL,
						proposed_experiences INTEGER NOT NULL,
						accepted_experiences INTEGER NOT NULL,
						proposed_amendments  INTEGER NOT NULL,
						accepted_amendments  INTEGER NOT NULL,
						started_at           TEXT NOT NULL,
						finished_at          TEXT NOT NULL
					)
				`},
				{"index reflection runs", `
					CREATE INDEX idx_reflection_runs_instance
						ON reflection_runs (instance_id, finished_at, id)
				`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return nil
		},
	},
	{
		Version: 16,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE TABLE reflection_events (
					sequence     INTEGER PRIMARY KEY,
					instance_id  TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
					source_kind  INTEGER NOT NULL,
					source_id    INTEGER NOT NULL,
					window_kind  INTEGER NOT NULL,
					window_key   TEXT NOT NULL,
					message      TEXT NOT NULL,
					substantive  INTEGER NOT NULL,
					event_at     TEXT NOT NULL,
					created_at   TEXT NOT NULL,
					UNIQUE (
						instance_id, source_kind, source_id,
						window_kind, window_key, message
					)
				)
			`); err != nil {
				return fmt.Errorf("create reflection events: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX idx_reflection_events_instance
					ON reflection_events (instance_id, sequence)
			`); err != nil {
				return fmt.Errorf("index reflection events: %w", err)
			}

			return nil
		},
	},
	{
		Version: 17,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			statements := []migrationStatement{
				{"add persona template id", `ALTER TABLE persona_lineages ADD COLUMN template_id TEXT`},
				{"add persona template origin", `ALTER TABLE persona_lineages ADD COLUMN template_origin TEXT`},
				{"add persona template hash", `ALTER TABLE persona_lineages ADD COLUMN template_hash TEXT`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return nil
		},
	},
	{
		Version: 18,
		Apply:   normaliseStoredTimes,
	},
	{
		Version: 19,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			statements := []migrationStatement{
				{"add experience last cited", `
					ALTER TABLE persona_experiences
						ADD COLUMN last_cited_at TEXT NOT NULL DEFAULT ''
				`},
				{"add experience salience", `
					ALTER TABLE persona_experiences
						ADD COLUMN salience_at TEXT NOT NULL DEFAULT ''
				`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return backfillExperienceSalience(ctx, tx)
		},
	},
	{
		// A row carrying `consolidated_at` is a consolidation, which is
		// the only removal v19 recorded. A retraction or a supersession
		// from before this migration therefore keeps a null `departure`,
		// and naming a reason for those rows would invent one.
		Version: 20,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			statements := []migrationStatement{
				{"rename consolidated to departed", `
					ALTER TABLE persona_amendments
						RENAME COLUMN consolidated_at TO departed_at
				`},
				{"add amendment departure", `
					ALTER TABLE persona_amendments ADD COLUMN departure TEXT
				`},
				{"name the recorded departures", `
					UPDATE persona_amendments SET departure = 'consolidated'
					WHERE departed_at IS NOT NULL
				`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return nil
		},
	},
	{
		Version: 21,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			statements := []migrationStatement{
				{"rename the template pool", `
					ALTER TABLE personas RENAME TO persona_templates
				`},
				{"retag stored template lists in the channel log", `
					UPDATE events
					   SET type = 'persona_templates_list',
					       data = json_set(
					                json_remove(
					                  json_set(data, '$.type', 'persona_templates_list'),
					                  '$.data.personas'),
					                '$.data.templates',
					                json_extract(data, '$.data.personas'))
					 WHERE type = 'personas_list'
				`},
				{"retag stored template lists in the reply log", `
					UPDATE instance_replies
					   SET type = 'persona_templates_list',
					       data = json_set(
					                json_remove(
					                  json_set(data, '$.type', 'persona_templates_list'),
					                  '$.data.personas'),
					                '$.data.templates',
					                json_extract(data, '$.data.personas'))
					 WHERE type = 'personas_list'
				`},
				{"retag stored template lists in projected scrollback", `
					UPDATE channel_scrollback
					   SET type = 'persona_templates_list',
					       data = json_set(
					                json_remove(
					                  json_set(data, '$.type', 'persona_templates_list'),
					                  '$.data.personas'),
					                '$.data.templates',
					                json_extract(data, '$.data.personas'))
					 WHERE type = 'personas_list'
				`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return nil
		},
	},
	{
		Version: 22,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			// The stored `persona_templates_list` rows are left where
			// they are. `queryEventRows` and `queryInstanceReplyRows`
			// each skip a discriminator this build does not recognise,
			// which is what lets an event type be removed without
			// touching stored rows, and `last_read.event_id`
			// references `events(id)` with no cascade, so deleting a
			// row a read cursor points at would fail the whole
			// migration.
			statements := []migrationStatement{
				{"drop the template pool", `DROP TABLE persona_templates`},
				{"drop the lineage template id", `
					ALTER TABLE persona_lineages DROP COLUMN template_id
				`},
				{"drop the lineage template origin", `
					ALTER TABLE persona_lineages DROP COLUMN template_origin
				`},
				{"drop the lineage template hash", `
					ALTER TABLE persona_lineages DROP COLUMN template_hash
				`},
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
					return fmt.Errorf("%s: %w", statement.name, err)
				}
			}

			return nil
		},
	},
}

type migrationStoredEvent struct {
	id    int64
	event domain.PersistableEvent
}

type migrationChannel struct {
	name    domain.ChannelName
	members domain.MemberList
}

func backfillSafeChannelScrollback(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name, data FROM channels ORDER BY name`)
	if err != nil {
		return fmt.Errorf("list channels for scrollback backfill: %w", err)
	}

	var channels []migrationChannel
	for rows.Next() {
		var name domain.ChannelName
		var data string
		if err := rows.Scan(&name, &data); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan channel for scrollback backfill: %w", err)
		}

		var channel channelRow
		if err := json.Unmarshal([]byte(data), &channel); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode channel %q for scrollback backfill: %w", name, err)
		}
		if channel.Kind != domain.KindChannel || channel.Members.Len() == 0 {
			continue
		}
		channels = append(channels, migrationChannel{name: name, members: channel.Members})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read channels for scrollback backfill: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close channels for scrollback backfill: %w", err)
	}

	for _, channel := range channels {
		events, err := migrationChannelEvents(ctx, tx, channel.name)
		if err != nil {
			return err
		}
		for member := range channel.members.All() {
			if member.InstanceID == "" {
				continue
			}
			if err := backfillMemberScrollback(ctx, tx, channel.name, member.InstanceID, events); err != nil {
				return err
			}
		}
	}

	return nil
}

func migrationChannelEvents(
	ctx context.Context,
	tx *sql.Tx,
	channel domain.ChannelName,
) ([]migrationStoredEvent, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, data FROM events WHERE channel = ? ORDER BY id`, channel)
	if err != nil {
		return nil, fmt.Errorf("read events for channel %q: %w", channel, err)
	}
	defer func() { _ = rows.Close() }()

	var events []migrationStoredEvent
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("scan event for channel %q: %w", channel, err)
		}

		event, err := domain.UnmarshalPersistableEvent([]byte(data))
		if err != nil {
			if errors.Is(err, domain.ErrUnknownEventType) {
				continue
			}

			return nil, fmt.Errorf("decode event %d for channel %q: %w", id, channel, err)
		}
		events = append(events, migrationStoredEvent{id: id, event: event})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events for channel %q: %w", channel, err)
	}

	return events, nil
}

func backfillMemberScrollback(
	ctx context.Context,
	tx *sql.Tx,
	channel domain.ChannelName,
	actor domain.InstanceID,
	events []migrationStoredEvent,
) error {
	var existing int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM channel_scrollback
			 WHERE instance_id = ? AND channel = ?
		)
	`, actor, channel).Scan(&existing); err != nil {
		return fmt.Errorf("check scrollback for %q in %q: %w", actor, channel, err)
	}
	if existing != 0 {
		return nil
	}

	start := -1
	for index, stored := range events {
		joined, ok := stored.event.(domain.Join)
		if !ok {
			continue
		}
		id, identified := joined.Source.InstanceID()
		if identified && id == actor {
			start = index
		}
	}
	if start < 0 {
		return nil
	}
	joined := events[start].event.(domain.Join)
	if migrationMembershipClosed(events[start+1:], actor, joined.Source.Nick()) {
		return nil
	}

	for _, stored := range events[start:] {
		projected := safeMigrationProjection(stored.event, actor)
		if projected == nil {
			continue
		}
		data, err := domain.MarshalPersistableEvent(projected)
		if err != nil {
			return fmt.Errorf("encode scrollback event %d for %q: %w", stored.id, actor, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO channel_scrollback
				(instance_id, channel, type, data, at)
			VALUES (?, ?, ?, ?, ?)
		`, actor, channel, domain.EventType(projected), string(data),
			formatTime(domain.EventTime(projected))); err != nil {
			return fmt.Errorf("backfill scrollback event %d for %q: %w", stored.id, actor, err)
		}
	}

	return nil
}

func migrationMembershipClosed(
	events []migrationStoredEvent,
	actor domain.InstanceID,
	nick domain.Nick,
) bool {
	for _, stored := range events {
		switch event := stored.event.(type) {
		case domain.Part:
			if sourceBelongsToMigrationActor(event.Source, actor, nick) {
				return true
			}
		case domain.Quit:
			if sourceBelongsToMigrationActor(event.Source, actor, nick) {
				return true
			}
		case domain.Kicked:
			if domain.EqualNick(event.Subject, nick) {
				return true
			}
		case domain.NickChange:
			if sourceBelongsToMigrationActor(event.Source, actor, nick) {
				nick = event.NewNick
			}
		}
	}

	return false
}

func sourceBelongsToMigrationActor(
	source domain.Source,
	actor domain.InstanceID,
	nick domain.Nick,
) bool {
	id, identified := source.InstanceID()
	if identified {
		return id == actor
	}

	return domain.EqualNick(source.Nick(), nick)
}

func safeMigrationProjection(
	event domain.PersistableEvent,
	actor domain.InstanceID,
) domain.PersistableEvent {
	switch event := event.(type) {
	case domain.Message:
		id, identified := event.Source.InstanceID()
		if !identified || id != actor {
			event.Source = domain.AnonymousSource()
		}

		return event
	case domain.TopicChange:
		id, identified := event.Source.InstanceID()
		if !identified || id != actor {
			event.Source = domain.AnonymousSource()
		}

		return event
	default:
		return nil
	}
}

// applyMigrations reconciles the recorded schema version against
// [SchemaVersion]. A current database is a no-op; an older
// database runs every registered step whose Version is strictly
// greater than the recorded one, transactionally; a database
// from a newer build fails-loud because downgrades aren't
// supported.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	got, err := readSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if got > SchemaVersion {
		return &schemaTooNewError{Found: got, Supported: SchemaVersion}
	}

	if got == SchemaVersion {
		return nil
	}

	missing := missingMigrations(got)
	if len(missing) == 0 {
		return &missingMigrationError{From: got, To: SchemaVersion}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range missing {
		if err := m.Apply(ctx, tx); err != nil {
			return fmt.Errorf("migrate to v%d: %w", m.Version, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, value) VALUES ('schema_version', ?)`,
		fmt.Sprintf("%d", SchemaVersion),
	); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}

	return nil
}

// missingMigrations returns the registered steps strictly greater
// than `from`. Returns nil when the gap from `from` to
// [SchemaVersion] is unbridgeable — every intermediate step must
// be registered for the chain to be runnable.
func missingMigrations(from int) []migration {
	var chain []migration
	expected := from + 1
	for _, m := range migrations {
		if m.Version <= from {
			continue
		}
		if m.Version != expected {
			return nil
		}
		chain = append(chain, m)
		expected++
	}

	if expected-1 != SchemaVersion {
		return nil
	}

	return chain
}

// schemaRecorded reports whether db already records a schema version.
// The `INSERT OR IGNORE` that writes version 1 is the last statement in
// the `schema` constant, so the row is written only after every
// preceding schema statement has run: a first open interrupted part way
// through leaves no row, and the next open creates the schema again.
func schemaRecorded(ctx context.Context, db *sql.DB) (bool, error) {
	var present bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'state'
		)`).Scan(&present); err != nil {
		return false, fmt.Errorf("look for the state table: %w", err)
	}

	if !present {
		return false, nil
	}

	version, err := readSchemaVersion(ctx, db)
	if err != nil {
		return false, fmt.Errorf("read schema version: %w", err)
	}

	return version > 0, nil
}

// readSchemaVersion returns the recorded version, or 0 when no
// row exists. A 0 result from an empty database is normal: the
// `INSERT OR IGNORE` in `schema` seeds the row to '1' on first
// exec, and applyMigrations brings it to [SchemaVersion] right
// after. A 0 from a populated database indicates a state row that
// pre-dates the seed and is handled by applyMigrations's "no
// migration to reach" branch.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM state WHERE key = 'schema_version'`,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}
