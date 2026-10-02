# NeelKIT — Data Model

**SQLite**, WAL mode, `foreign_keys=ON`, `busy_timeout=5000`.
Driver: `modernc.org/sqlite` (pure Go). Migrations are numbered `.sql` files
embedded with `//go:embed` and applied in a transaction at startup; the applied
version is recorded in `schema_migrations`. Startup fails loudly on a dirty
migration rather than continuing against a half-applied schema.

Conventions:
- Ids are `TEXT` ULIDs with a type prefix (`conv_01J9…`) — sortable by creation
  time, greppable in logs, and impossible to confuse across tables.
- Timestamps are `INTEGER` Unix milliseconds UTC. No timezone ambiguity, no
  string parsing, correct integer ordering.
- `user_id` is on every domain table from migration 0001 even though v1 is
  single-user (AD-007). Adding it later means backfilling every row and every
  query; adding it now costs one column.
- No `ON DELETE CASCADE` from `audit_events`. Audit rows outlive their subjects
  on purpose.

---

## Schema

```sql
-- 0001_init.sql

CREATE TABLE users (
  id            TEXT PRIMARY KEY,          -- usr_…
  display_name  TEXT NOT NULL,
  created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE api_keys (
  id            TEXT PRIMARY KEY,          -- the public key_id in the token
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  secret_hash   TEXT NOT NULL,             -- argon2id; the secret is never stored
  scopes        TEXT NOT NULL DEFAULT 'full',
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER,
  expires_at    INTEGER,
  revoked_at    INTEGER
) STRICT;
CREATE INDEX idx_api_keys_user ON api_keys(user_id);
-- lookup is by PK: one indexed read, constant-time compare of the hash.

CREATE TABLE conversations (
  id            TEXT PRIMARY KEY,          -- conv_…
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title         TEXT,                      -- derived from the first final transcript
  pinned        INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_conv_user_updated ON conversations(user_id, updated_at DESC);
--            ^ exactly matches the list query: WHERE user_id=? ORDER BY updated_at DESC

CREATE TABLE sessions (                    -- one realtime WebSocket connection
  id            TEXT PRIMARY KEY,          -- sess_…
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL,
  mode          TEXT NOT NULL,             -- vad | push_to_talk
  client        TEXT,                      -- user agent / cli
  started_at    INTEGER NOT NULL,
  ended_at      INTEGER,
  end_reason    TEXT                       -- client_close | idle | error | shutdown
) STRICT;
CREATE INDEX idx_sessions_conv ON sessions(conversation_id);

CREATE TABLE messages (
  id            TEXT PRIMARY KEY,          -- msg_…
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  turn_id       TEXT NOT NULL,             -- groups user msg + tool calls + reply
  role          TEXT NOT NULL,             -- user | assistant | tool | system
  content       TEXT NOT NULL,
  source        TEXT NOT NULL,             -- voice | text | tool
  transcript_id TEXT REFERENCES transcripts(id) ON DELETE SET NULL,
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_messages_conv_created ON messages(conversation_id, created_at);
CREATE INDEX idx_messages_turn ON messages(turn_id);

CREATE TABLE transcripts (
  id            TEXT PRIMARY KEY,          -- tr_…
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id    TEXT REFERENCES sessions(id) ON DELETE SET NULL,
  conversation_id TEXT REFERENCES conversations(id) ON DELETE CASCADE,
  raw_text      TEXT NOT NULL,             -- straight from the decoder
  text          TEXT NOT NULL,             -- after normalize/
  language      TEXT NOT NULL DEFAULT 'en',
  duration_ms   INTEGER NOT NULL,
  confidence    REAL,                      -- geometric mean of token probs
  model         TEXT NOT NULL,
  device        TEXT NOT NULL,             -- cpu | cuda
  rtf           REAL,                      -- real-time factor for this decode
  audio_path    TEXT,                      -- NULL unless privacy.store_audio
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_transcripts_user_created ON transcripts(user_id, created_at DESC);

-- Word timings live apart: most reads want the text only, and a 10-minute
-- transcript has thousands of words. Loading them into every list query would
-- dominate the response.
CREATE TABLE transcript_words (
  transcript_id TEXT NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
  idx           INTEGER NOT NULL,
  word          TEXT NOT NULL,
  start_ms      INTEGER NOT NULL,
  end_ms        INTEGER NOT NULL,
  confidence    REAL,
  PRIMARY KEY (transcript_id, idx)
) STRICT, WITHOUT ROWID;

-- Full-text search, kept in sync by triggers. FTS5 is why this project needs
-- no search service.
CREATE VIRTUAL TABLE transcripts_fts USING fts5(
  text, content='transcripts', content_rowid='rowid', tokenize='porter unicode61'
);
CREATE TRIGGER transcripts_ai AFTER INSERT ON transcripts BEGIN
  INSERT INTO transcripts_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER transcripts_ad AFTER DELETE ON transcripts BEGIN
  INSERT INTO transcripts_fts(transcripts_fts, rowid, text)
    VALUES('delete', old.rowid, old.text);
END;
CREATE TRIGGER transcripts_au AFTER UPDATE ON transcripts BEGIN
  INSERT INTO transcripts_fts(transcripts_fts, rowid, text)
    VALUES('delete', old.rowid, old.text);
  INSERT INTO transcripts_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TABLE intents (                     -- what the router decided, per turn
  id            TEXT PRIMARY KEY,
  turn_id       TEXT NOT NULL,
  transcript_id TEXT REFERENCES transcripts(id) ON DELETE SET NULL,
  name          TEXT NOT NULL,             -- git.branch_create | chat | unknown
  source        TEXT NOT NULL,             -- grammar | llm
  confidence    REAL,
  slots_json    TEXT NOT NULL DEFAULT '{}',
  latency_ms    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_intents_turn ON intents(turn_id);
CREATE INDEX idx_intents_name ON intents(name, created_at DESC);
-- ^ answers "which commands do I actually use?" — feeds grammar rule design.

CREATE TABLE tool_executions (
  id            TEXT PRIMARY KEY,          -- = call_id
  turn_id       TEXT NOT NULL,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tool          TEXT NOT NULL,
  args_json     TEXT NOT NULL,
  risk          TEXT NOT NULL,             -- read | write | execute | network
  decision      TEXT NOT NULL,             -- allow | confirm | deny
  approved_by   TEXT,                      -- user | policy | NULL when denied
  status        TEXT NOT NULL,             -- pending|running|succeeded|failed|denied|timeout
  exit_code     INTEGER,
  stdout_trunc  TEXT,                      -- capped at 64 KiB
  stderr_trunc  TEXT,
  error         TEXT,
  started_at    INTEGER,
  finished_at   INTEGER,
  duration_ms   INTEGER,
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_tool_exec_turn ON tool_executions(turn_id);
CREATE INDEX idx_tool_exec_tool_created ON tool_executions(tool, created_at DESC);

CREATE TABLE settings (
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key           TEXT NOT NULL,
  value_json    TEXT NOT NULL,
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY (user_id, key)
) STRICT, WITHOUT ROWID;

-- Append-only. No UPDATE, no DELETE, no cascade. If a conversation is deleted
-- the audit trail of what ran on the machine survives it.
CREATE TABLE audit_events (
  id            TEXT PRIMARY KEY,
  user_id       TEXT,
  request_id    TEXT,
  trace_id      TEXT,
  actor         TEXT NOT NULL,             -- user | assistant | system
  action        TEXT NOT NULL,             -- tool.invoke | tool.deny | auth.fail | settings.change | key.create
  subject       TEXT,
  risk          TEXT,
  outcome       TEXT NOT NULL,             -- allowed | denied | failed
  detail_json   TEXT NOT NULL DEFAULT '{}',
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_audit_created ON audit_events(created_at DESC);
CREATE INDEX idx_audit_action ON audit_events(action, created_at DESC);
```

---

## Index rationale

Every index exists for a named query; none are speculative.

| Index | Query it serves |
|---|---|
| `idx_conv_user_updated` | conversation list: `WHERE user_id=? ORDER BY updated_at DESC LIMIT ?` — covering for the sort, so no filesort |
| `idx_messages_conv_created` | loading one conversation in order |
| `idx_messages_turn` | assembling a turn (user msg + tool calls + reply) for the approval UI |
| `idx_transcripts_user_created` | history page, cursor pagination |
| `transcripts_fts` | `/v1/transcripts/search` |
| `idx_intents_name` | "top commands" analytics → tells me which grammar rules to write next |
| `idx_tool_exec_tool_created` | "show me the last 10 times `shell.run` fired" |
| `idx_audit_action` | security review: all `auth.fail` in a window |

`transcript_words` is `WITHOUT ROWID` with a composite PK — the table *is* the
index, and rows are stored in `(transcript_id, idx)` order, so fetching one
transcript's words is a single sequential range scan.

---

## Retention and privacy

- **Audio is not stored by default.** `privacy.store_audio=false` means the ring
  buffer is the only place samples ever exist, and it is overwritten
  continuously. Turning it on writes WAVs under `data/audio/` and sets
  `transcripts.audio_path`.
- **Retention job** runs hourly: deletes transcripts and messages older than
  `privacy.retention_days` (default 90; `0` = keep forever). Audit events have
  their own, longer retention (default 365 days) and are never deleted by the
  conversation delete path.
- **Export before delete.** `GET /v1/conversations/{id}/export` exists so
  "delete my data" is not "lose my data".
- **`VACUUM` on demand** via `nk admin vacuum` — SQLite does not reclaim space
  on delete, and a user who deleted everything for privacy reasons should be
  able to see the file shrink.

## Why not Postgres

Answered in `docs/architecture.md` AD-007. Short version: this is a
single-process, single-writer, local-first tool; WAL mode covers the
concurrency, FTS5 covers the search, and requiring Docker before a user can try
the project is a worse trade than any query-planner advantage. The repository
interface means Postgres is a new implementation in `internal/storage/postgres/`,
not a rewrite — and the ULID primary keys and millisecond-integer timestamps
were chosen so that port needs no data migration.
