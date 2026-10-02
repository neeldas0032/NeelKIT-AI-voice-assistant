package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newDB(t *testing.T) (*DB, string) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	uid, err := db.EnsureUser(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return db, uid
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		db, err := Open(dir)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		db.Close()
	}
}

func TestEnsureUserIsStable(t *testing.T) {
	db, uid := newDB(t)
	again, err := db.EnsureUser(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if again != uid {
		t.Fatalf("EnsureUser returned a different id on the second call: %s vs %s", again, uid)
	}
}

func TestConversationLifecycle(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	c, err := db.CreateConversation(ctx, uid, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMessage(ctx, Message{
		ConversationID: c.ID, TurnID: "turn_1", Role: "user",
		Content: "create a branch", Source: "voice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchConversation(ctx, c.ID, "create a branch"); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "create a branch" {
		t.Fatalf("title = %q, want it derived from the first turn", got.Title)
	}

	// A second touch must not overwrite a title that already exists.
	if err := db.TouchConversation(ctx, c.ID, "something else"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetConversation(ctx, c.ID)
	if got.Title != "create a branch" {
		t.Fatalf("title was overwritten to %q", got.Title)
	}

	list, err := db.ListConversations(ctx, uid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Messages != 1 {
		t.Fatalf("list = %+v", list)
	}

	if err := db.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetConversation(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := db.DeleteConversation(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// Searchable voice history is a headline feature, and it exists because FTS5
// is in the file rather than in a separate service.
func TestTranscriptFullTextSearch(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	for _, text := range []string{
		"create a git branch called feature auth",
		"the deployment pipeline is failing on the staging environment",
		"remind me to review the authentication middleware",
	} {
		if _, err := db.SaveTranscript(ctx, Transcript{
			UserID: uid, RawText: text, Text: text, Language: "en",
			DurationMS: 2000, Confidence: 0.9, Model: "test", Device: "cpu",
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // keep created_at ordering deterministic
	}

	hits, err := db.SearchTranscripts(ctx, uid, "authentication", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits for 'authentication', want 1", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "[") {
		t.Errorf("snippet %q is not highlighted", hits[0].Snippet)
	}

	// Prefix matching: a partially remembered word still finds the utterance.
	// This is the behaviour a person expects from a search box, and the reason
	// the index does not use the Porter stemmer (see migration 0002).
	for _, q := range []string{"deploy", "auth", "environ"} {
		hits, err := db.SearchTranscripts(ctx, uid, q, 10)
		if err != nil || len(hits) == 0 {
			t.Errorf("prefix search %q returned %d hits, %v", q, len(hits), err)
		}
	}

	// Multi-word queries are conjunctive.
	if hits, err = db.SearchTranscripts(ctx, uid, "deployment pipeline", 10); err != nil || len(hits) != 1 {
		t.Errorf("multi-term search returned %d hits, %v", len(hits), err)
	}
}

// A user speaking punctuation, or a stray FTS5 operator in a transcript, must
// not turn a search into a syntax error.
func TestSearchHandlesOperatorCharacters(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()
	if _, err := db.SaveTranscript(ctx, Transcript{
		UserID: uid, RawText: "hello", Text: "hello world", Model: "t", Device: "cpu",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`"`, `foo OR`, `a NEAR b`, `*`, `(unbalanced`} {
		if _, err := db.SearchTranscripts(ctx, uid, q, 10); err != nil {
			t.Errorf("query %q errored: %v", q, err)
		}
	}
	if hits, _ := db.SearchTranscripts(ctx, uid, "  ", 10); len(hits) != 0 {
		t.Error("blank query returned hits")
	}
}

func TestToolExecutionRecordedBeforeAndAfter(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	e := ToolExecution{
		ID: "call_1", TurnID: "turn_1", UserID: uid, Tool: "shell.task",
		ArgsJSON: `{"task":"test"}`, Risk: "execute", Decision: "confirm",
		Preview: "go test ./...", Status: "pending",
	}
	if err := db.StartToolExecution(ctx, e); err != nil {
		t.Fatal(err)
	}

	// The pending row must be readable before the call finishes; that is the
	// whole point of writing it first.
	rows, err := db.ListToolExecutions(ctx, "turn_1")
	if err != nil || len(rows) != 1 || rows[0].Status != "pending" {
		t.Fatalf("pending row = %+v, %v", rows, err)
	}

	e.Status = "succeeded"
	e.ExitCode = 0
	e.Summary = "test passed."
	e.DurationMS = 1234
	if err := db.FinishToolExecution(ctx, e); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListToolExecutions(ctx, "turn_1")
	if rows[0].Status != "succeeded" || rows[0].DurationMS != 1234 {
		t.Fatalf("finished row = %+v", rows[0])
	}
}

// Deleting a conversation must not erase the record of what ran on the machine.
func TestAuditSurvivesConversationDeletion(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	c, _ := db.CreateConversation(ctx, uid, "temp")
	if err := db.Audit(ctx, AuditEvent{
		UserID: uid, Actor: "user", Action: "tool.invoke",
		Subject: "shell.task", Risk: "execute", Outcome: "allowed",
		DetailJSON: `{"conversation":"` + c.ID + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	events, err := db.ListAudit(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("audit rows = %d after deleting the conversation, want 1", len(events))
	}
}

func TestSettingsRoundTripAndUpsert(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	if _, err := db.GetSetting(ctx, uid, "policy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing setting returned %v", err)
	}
	if err := db.SetSetting(ctx, uid, "policy", `{"write":"confirm"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, uid, "policy", `{"write":"allow"}`); err != nil {
		t.Fatal(err)
	}
	v, err := db.GetSetting(ctx, uid, "policy")
	if err != nil || v != `{"write":"allow"}` {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db, _ := newDB(t)
	// SQLite disables foreign keys per connection unless asked; if the DSN
	// flag were lost, orphan rows would accumulate silently.
	_, err := db.SQL().Exec(
		`INSERT INTO messages(id,conversation_id,turn_id,role,content,source,created_at)
		 VALUES ('m1','conv_does_not_exist','t','user','x','text',1)`)
	if err == nil {
		t.Fatal("inserted a message referencing a missing conversation")
	}
}

func TestTranscriptDeletionKeepsSearchIndexInSync(t *testing.T) {
	db, uid := newDB(t)
	ctx := context.Background()

	c, _ := db.CreateConversation(ctx, uid, "x")
	if _, err := db.SaveTranscript(ctx, Transcript{
		UserID: uid, ConversationID: c.ID,
		RawText: "unique phrase here", Text: "unique phrase here", Model: "t", Device: "cpu",
	}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.SearchTranscripts(ctx, uid, "unique", 10); len(hits) != 1 {
		t.Fatalf("expected 1 hit before deletion, got %d", len(hits))
	}
	if err := db.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	// A stale index would keep returning a row whose transcript is gone.
	if hits, _ := db.SearchTranscripts(ctx, uid, "unique", 10); len(hits) != 0 {
		t.Fatalf("search index still returns %d deleted transcripts", len(hits))
	}
}
