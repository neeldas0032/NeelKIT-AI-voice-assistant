-- NeelKIT initial schema.
--
-- Conventions:
--   * ids are ULID-shaped TEXT with a type prefix, so they sort by creation
--     time as plain strings and a misrouted id is obvious in a log line;
--   * timestamps are INTEGER Unix milliseconds UTC, which order correctly as
--     integers and carry no timezone ambiguity;
--   * user_id is present from the first migration even though v1 is
--     single-user, because adding it later means backfilling every row and
--     rewriting every query.

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  display_name  TEXT NOT NULL,
  created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE api_keys (
  id            TEXT PRIMARY KEY,            -- the public key id inside the token
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  secret_hash   TEXT NOT NULL,               -- PBKDF2-SHA256; the secret is never stored
  salt          TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER,
  revoked_at    INTEGER
) STRICT;
CREATE INDEX idx_api_keys_user ON api_keys(user_id);

CREATE TABLE conversations (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title         TEXT,
  pinned        INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
) STRICT;
-- Matches the list query exactly (WHERE user_id ORDER BY updated_at DESC), so
-- the sort is served by the index instead of a filesort.
CREATE INDEX idx_conv_user_updated ON conversations(user_id, updated_at DESC);

CREATE TABLE transcripts (
  id              TEXT PRIMARY KEY,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id TEXT REFERENCES conversations(id) ON DELETE CASCADE,
  raw_text        TEXT NOT NULL,             -- straight from the decoder
  text            TEXT NOT NULL,             -- after normalisation
  language        TEXT NOT NULL DEFAULT 'en',
  duration_ms     INTEGER NOT NULL,
  confidence      REAL,
  model           TEXT NOT NULL,
  device          TEXT NOT NULL,
  rtf             REAL,
  words_json      TEXT,
  created_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_transcripts_user_created ON transcripts(user_id, created_at DESC);
CREATE INDEX idx_transcripts_conv ON transcripts(conversation_id);

CREATE TABLE messages (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  turn_id         TEXT NOT NULL,
  role            TEXT NOT NULL,             -- user | assistant | tool | system
  content         TEXT NOT NULL,
  source          TEXT NOT NULL,             -- voice | text | tool
  transcript_id   TEXT REFERENCES transcripts(id) ON DELETE SET NULL,
  created_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_messages_conv_created ON messages(conversation_id, created_at);
CREATE INDEX idx_messages_turn ON messages(turn_id);

CREATE TABLE tool_executions (
  id            TEXT PRIMARY KEY,            -- the call id
  turn_id       TEXT NOT NULL,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tool          TEXT NOT NULL,
  args_json     TEXT NOT NULL,
  risk          TEXT NOT NULL,
  decision      TEXT NOT NULL,
  preview       TEXT NOT NULL,
  status        TEXT NOT NULL,               -- pending|running|succeeded|failed|denied
  exit_code     INTEGER,
  summary       TEXT,
  stdout_trunc  TEXT,
  error         TEXT,
  duration_ms   INTEGER,
  created_at    INTEGER NOT NULL,
  finished_at   INTEGER
) STRICT;
CREATE INDEX idx_tool_exec_turn ON tool_executions(turn_id);
CREATE INDEX idx_tool_exec_tool_created ON tool_executions(tool, created_at DESC);

-- Append-only. No UPDATE, no DELETE, and deliberately no cascade: deleting a
-- conversation must not erase the record of what ran on the machine.
CREATE TABLE audit_events (
  id            TEXT PRIMARY KEY,
  user_id       TEXT,
  request_id    TEXT,
  actor         TEXT NOT NULL,               -- user | assistant | system
  action        TEXT NOT NULL,
  subject       TEXT,
  risk          TEXT,
  outcome       TEXT NOT NULL,               -- allowed | denied | failed
  detail_json   TEXT NOT NULL DEFAULT '{}',
  created_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_audit_created ON audit_events(created_at DESC);
CREATE INDEX idx_audit_action ON audit_events(action, created_at DESC);

CREATE TABLE settings (
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key           TEXT NOT NULL,
  value_json    TEXT NOT NULL,
  updated_at    INTEGER NOT NULL,
  PRIMARY KEY (user_id, key)
) STRICT, WITHOUT ROWID;
