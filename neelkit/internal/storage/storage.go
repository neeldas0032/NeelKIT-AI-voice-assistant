// Package storage persists conversations, transcripts, tool executions and the
// audit log.
//
// SQLite in WAL mode, because this is a single-process local-first tool: the
// workload is one writer and several readers, FTS5 provides transcript search
// with no extra service, and requiring a user to install Docker and Postgres
// before they can try the project would cost more than any query planner gains.
// The repository methods are the seam a Postgres implementation would slot into.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/id"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("storage: not found")

// DB is the database handle.
type DB struct {
	sql  *sql.DB
	path string
}

// Open opens (creating if needed) the database under dir.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create data dir: %w", err)
	}
	path := filepath.Join(dir, "neelkit.db")

	// _journal_mode=WAL       readers never block the writer
	// _busy_timeout=5000      wait instead of failing on a brief lock
	// _foreign_keys=on        SQLite disables these per connection by default
	// _synchronous=NORMAL     correct under WAL, and far faster than FULL
	dsn := path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL"
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	// SQLite permits exactly one writer. Capping the pool at one connection
	// turns "database is locked" from a runtime error into ordinary queueing.
	sdb.SetMaxOpenConns(1)
	sdb.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sdb.PingContext(ctx); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}

	db := &DB{sql: sdb, path: path}
	if err := db.migrate(ctx); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// Path returns the database file path, for diagnostics.
func (d *DB) Path() string { return d.path }

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the handle for tests and maintenance commands.
func (d *DB) SQL() *sql.DB { return d.sql }

// migrate applies pending migrations inside a transaction each.
//
// A half-applied migration is worse than a failed startup, so a failure rolls
// back and the server refuses to start rather than serving against an
// inconsistent schema.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("storage: migration table: %w", err)
	}

	applied := map[string]bool{}
	rows, err := d.sql.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names) // numeric prefixes make lexical order the right order

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("storage: migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			name, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("storage: commit migration %s: %w", name, err)
		}
	}
	return nil
}

func now() int64 { return time.Now().UnixMilli() }

// EnsureUser returns the single local user, creating it on first run.
func (d *DB) EnsureUser(ctx context.Context, name string) (string, error) {
	var uid string
	err := d.sql.QueryRowContext(ctx, `SELECT id FROM users ORDER BY created_at LIMIT 1`).Scan(&uid)
	if err == nil {
		return uid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	uid = id.New(id.PrefixUser)
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at) VALUES (?, ?, ?)`,
		uid, name, now()); err != nil {
		return "", err
	}
	return uid, nil
}

// ---------------------------------------------------------- api keys

// APIKey is a stored credential. Only a salted hash of the secret is kept: the
// token itself is shown once, when it is created, and never written anywhere.
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	SecretHash string
	Salt       string
	CreatedAt  int64
}

// SaveAPIKey stores a new key.
func (d *DB) SaveAPIKey(ctx context.Context, k APIKey) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO api_keys(id, user_id, name, secret_hash, salt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, k.Name, k.SecretHash, k.Salt, k.CreatedAt)
	return err
}

// ActiveAPIKeys returns the user's keys that have not been revoked.
func (d *DB) ActiveAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, user_id, name, secret_hash, salt, created_at
		   FROM api_keys WHERE user_id = ? AND revoked_at IS NULL
		  ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.SecretHash, &k.Salt, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKeys revokes every active key the user has, returning how many.
func (d *DB) RevokeAPIKeys(ctx context.Context, userID string) (int, error) {
	res, err := d.sql.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		now(), userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---------------------------------------------------------- conversations

// Conversation is a thread of turns.
type Conversation struct {
	ID        string `json:"id"`
	UserID    string `json:"-"`
	Title     string `json:"title"`
	Pinned    bool   `json:"pinned"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Messages  int    `json:"message_count,omitempty"`
}

// CreateConversation starts a new thread.
func (d *DB) CreateConversation(ctx context.Context, userID, title string) (Conversation, error) {
	c := Conversation{
		ID: id.New(id.PrefixConversation), UserID: userID, Title: title,
		CreatedAt: now(), UpdatedAt: now(),
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO conversations(id,user_id,title,pinned,created_at,updated_at) VALUES (?,?,?,0,?,?)`,
		c.ID, c.UserID, c.Title, c.CreatedAt, c.UpdatedAt)
	return c, err
}

// TouchConversation bumps updated_at and sets the title if it is still empty.
func (d *DB) TouchConversation(ctx context.Context, convID, title string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE conversations
		    SET updated_at = ?,
		        title = CASE WHEN title IS NULL OR title = '' THEN ? ELSE title END
		  WHERE id = ?`,
		now(), title, convID)
	return err
}

// GetConversation reads one thread.
func (d *DB) GetConversation(ctx context.Context, convID string) (Conversation, error) {
	var c Conversation
	var title sql.NullString
	err := d.sql.QueryRowContext(ctx,
		`SELECT id,user_id,title,pinned,created_at,updated_at FROM conversations WHERE id = ?`, convID).
		Scan(&c.ID, &c.UserID, &title, &c.Pinned, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Title = title.String
	return c, err
}

// ListConversations returns threads newest first.
func (d *DB) ListConversations(ctx context.Context, userID string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT c.id, c.title, c.pinned, c.created_at, c.updated_at,
		        (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id)
		   FROM conversations c
		  WHERE c.user_id = ?
		  ORDER BY c.updated_at DESC
		  LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var title sql.NullString
		if err := rows.Scan(&c.ID, &title, &c.Pinned, &c.CreatedAt, &c.UpdatedAt, &c.Messages); err != nil {
			return nil, err
		}
		c.Title = title.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConversation removes a thread and its messages and transcripts. Audit
// rows are deliberately retained.
func (d *DB) DeleteConversation(ctx context.Context, convID string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------- messages

// Message is one entry in a conversation.
type Message struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	TurnID         string `json:"turn_id"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	Source         string `json:"source"`
	TranscriptID   string `json:"transcript_id,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// AddMessage appends to a conversation.
func (d *DB) AddMessage(ctx context.Context, m Message) (Message, error) {
	if m.ID == "" {
		m.ID = id.New(id.PrefixMessage)
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now()
	}
	var tid any
	if m.TranscriptID != "" {
		tid = m.TranscriptID
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO messages(id,conversation_id,turn_id,role,content,source,transcript_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, m.ConversationID, m.TurnID, m.Role, m.Content, m.Source, tid, m.CreatedAt)
	return m, err
}

// ListMessages returns a conversation's messages in order.
func (d *DB) ListMessages(ctx context.Context, convID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id,conversation_id,turn_id,role,content,source,COALESCE(transcript_id,''),created_at
		   FROM messages WHERE conversation_id = ? ORDER BY created_at LIMIT ?`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.TurnID, &m.Role,
			&m.Content, &m.Source, &m.TranscriptID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------- transcripts

// Transcript is one recognised utterance.
type Transcript struct {
	ID             string  `json:"id"`
	UserID         string  `json:"-"`
	ConversationID string  `json:"conversation_id,omitempty"`
	RawText        string  `json:"raw_text"`
	Text           string  `json:"text"`
	Language       string  `json:"language"`
	DurationMS     int64   `json:"duration_ms"`
	Confidence     float64 `json:"confidence"`
	Model          string  `json:"model"`
	Device         string  `json:"device"`
	RTF            float64 `json:"rtf"`
	WordsJSON      string  `json:"-"`
	CreatedAt      int64   `json:"created_at"`
	Snippet        string  `json:"snippet,omitempty"`
}

// SaveTranscript stores an utterance.
func (d *DB) SaveTranscript(ctx context.Context, t Transcript) (Transcript, error) {
	if t.ID == "" {
		t.ID = id.New(id.PrefixTranscript)
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = now()
	}
	var conv any
	if t.ConversationID != "" {
		conv = t.ConversationID
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO transcripts(id,user_id,conversation_id,raw_text,text,language,
		                         duration_ms,confidence,model,device,rtf,words_json,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.UserID, conv, t.RawText, t.Text, t.Language,
		t.DurationMS, t.Confidence, t.Model, t.Device, t.RTF, t.WordsJSON, t.CreatedAt)
	return t, err
}

// buildFTSQuery turns free text into a safe FTS5 expression.
//
// Every term is quoted so an operator the user typed - or that the recogniser
// produced by accident - is matched as text instead of blowing up as a syntax
// error. The final term gets a prefix wildcard, so a partially remembered word
// still finds the utterance.
func buildFTSQuery(q string) string {
	fields := strings.Fields(q)
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		// Strip characters that carry meaning to FTS5 but none to the user.
		f = strings.Trim(f, `"'*^()[]{}:,.;!?`)
		if f == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	if len(terms) == 0 {
		return ""
	}
	terms[len(terms)-1] += "*"
	return strings.Join(terms, " ")
}

// SearchTranscripts runs a full-text query, newest first, with a highlighted
// snippet.
func (d *DB) SearchTranscripts(ctx context.Context, userID, query string, limit int) ([]Transcript, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	fts := buildFTSQuery(q)
	if fts == "" {
		return nil, nil
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT t.id, COALESCE(t.conversation_id,''), t.text, t.confidence, t.duration_ms, t.created_at,
		        snippet(transcripts_fts, 0, '[', ']', '...', 12)
		   FROM transcripts_fts f
		   JOIN transcripts t ON t.rowid = f.rowid
		  WHERE transcripts_fts MATCH ? AND t.user_id = ?
		  ORDER BY t.created_at DESC
		  LIMIT ?`, fts, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Transcript
	for rows.Next() {
		var t Transcript
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.Text, &t.Confidence,
			&t.DurationMS, &t.CreatedAt, &t.Snippet); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ------------------------------------------------------- tool executions

// ToolExecution records one tool call.
type ToolExecution struct {
	ID         string `json:"call_id"`
	TurnID     string `json:"turn_id"`
	UserID     string `json:"-"`
	Tool       string `json:"tool"`
	ArgsJSON   string `json:"args"`
	Risk       string `json:"risk"`
	Decision   string `json:"decision"`
	Preview    string `json:"preview"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Summary    string `json:"summary"`
	Stdout     string `json:"stdout,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	CreatedAt  int64  `json:"created_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

// StartToolExecution records an attempt before it runs, so a process killed
// mid-command still leaves evidence of what was tried.
func (d *DB) StartToolExecution(ctx context.Context, e ToolExecution) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = now()
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO tool_executions(id,turn_id,user_id,tool,args_json,risk,decision,preview,status,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TurnID, e.UserID, e.Tool, e.ArgsJSON, e.Risk, e.Decision, e.Preview, e.Status, e.CreatedAt)
	return err
}

// FinishToolExecution updates the record after the call completes.
func (d *DB) FinishToolExecution(ctx context.Context, e ToolExecution) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE tool_executions
		    SET status=?, exit_code=?, summary=?, stdout_trunc=?, error=?, duration_ms=?, finished_at=?
		  WHERE id = ?`,
		e.Status, e.ExitCode, e.Summary, e.Stdout, e.Error, e.DurationMS, now(), e.ID)
	return err
}

// ListToolExecutions returns a turn's tool calls.
func (d *DB) ListToolExecutions(ctx context.Context, turnID string) ([]ToolExecution, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id,turn_id,tool,args_json,risk,decision,preview,status,
		        COALESCE(exit_code,0),COALESCE(summary,''),COALESCE(stdout_trunc,''),
		        COALESCE(error,''),COALESCE(duration_ms,0),created_at,COALESCE(finished_at,0)
		   FROM tool_executions WHERE turn_id = ? ORDER BY created_at`, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolExecution
	for rows.Next() {
		var e ToolExecution
		if err := rows.Scan(&e.ID, &e.TurnID, &e.Tool, &e.ArgsJSON, &e.Risk, &e.Decision,
			&e.Preview, &e.Status, &e.ExitCode, &e.Summary, &e.Stdout, &e.Error,
			&e.DurationMS, &e.CreatedAt, &e.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ audit log

// AuditEvent is one append-only record.
type AuditEvent struct {
	ID         string `json:"id"`
	UserID     string `json:"-"`
	RequestID  string `json:"request_id,omitempty"`
	Actor      string `json:"actor"`
	Action     string `json:"action"`
	Subject    string `json:"subject,omitempty"`
	Risk       string `json:"risk,omitempty"`
	Outcome    string `json:"outcome"`
	DetailJSON string `json:"detail,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// Audit appends an event. It never returns an error to the caller's critical
// path in practice, but the error is returned so a full disk is visible.
func (d *DB) Audit(ctx context.Context, e AuditEvent) error {
	if e.ID == "" {
		e.ID = id.New(id.PrefixAudit)
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = now()
	}
	if e.DetailJSON == "" {
		e.DetailJSON = "{}"
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO audit_events(id,user_id,request_id,actor,action,subject,risk,outcome,detail_json,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.UserID, e.RequestID, e.Actor, e.Action, e.Subject, e.Risk, e.Outcome, e.DetailJSON, e.CreatedAt)
	return err
}

// ListAudit returns recent audit events, newest first.
func (d *DB) ListAudit(ctx context.Context, since time.Time, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id,COALESCE(user_id,''),COALESCE(request_id,''),actor,action,
		        COALESCE(subject,''),COALESCE(risk,''),outcome,detail_json,created_at
		   FROM audit_events WHERE created_at >= ? ORDER BY created_at DESC LIMIT ?`,
		since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.UserID, &e.RequestID, &e.Actor, &e.Action,
			&e.Subject, &e.Risk, &e.Outcome, &e.DetailJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- settings

// GetSetting reads a JSON-encoded setting.
func (d *DB) GetSetting(ctx context.Context, userID, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting writes a JSON-encoded setting.
func (d *DB) SetSetting(ctx context.Context, userID, key, valueJSON string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO settings(user_id,key,value_json,updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(user_id,key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		userID, key, valueJSON, now())
	return err
}
