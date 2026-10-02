package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	fakellm "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm/fake"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/runner"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/sandbox"

	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fsx"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
)

func newAssistant(t *testing.T, provider llm.Provider) (*Assistant, *storage.DB, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.New(sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	uid, err := db.EnsureUser(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	return New(Config{
		Router: intent.New(intent.Config{Provider: provider}),
		Runner: runner.New(runner.Config{Sandbox: sb, Policy: policy.DefaultSettings()}),
		DB:     db,
		UserID: uid,
	}), db, root
}

// Reading is allowed by default, so a read command runs straight through.
func TestReadOnlyCommandExecutesImmediately(t *testing.T) {
	a, _, _ := newAssistant(t, nil)

	turn, err := a.Process(context.Background(), Input{Text: "open the file hello.txt", Source: "voice"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusExecuted {
		t.Fatalf("status = %s, calls = %+v", turn.Status, turn.Calls)
	}
	if turn.Intent.Source != intent.SourceGrammar {
		t.Errorf("source = %s, want grammar", turn.Intent.Source)
	}
	if turn.Message == "" {
		t.Error("no spoken reply composed")
	}
}

// The central safety behaviour: a write is planned and previewed, but nothing
// happens until a human approves it.
func TestWriteCommandSuspendsUntilApproved(t *testing.T) {
	a, _, root := newAssistant(t, nil)
	ctx := context.Background()

	turn, err := a.Process(ctx, Input{Text: "make a note call the dentist", Source: "voice"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusAwaitingApproval || len(turn.Calls) != 1 {
		t.Fatalf("status = %s, calls = %+v", turn.Status, turn.Calls)
	}
	call := turn.Calls[0]
	if call.Preview == "" {
		t.Error("no preview shown to the user")
	}

	// Nothing must have been written yet.
	if _, err := os.Stat(filepath.Join(root, "NOTES.md")); err == nil {
		t.Fatal("the note was written before approval")
	}

	res, err := a.Approve(ctx, call.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("after approval: %+v", res)
	}
	body, err := os.ReadFile(filepath.Join(root, "NOTES.md"))
	if err != nil {
		t.Fatalf("note not written after approval: %v", err)
	}
	if !strings.Contains(string(body), "call the dentist") {
		t.Fatalf("note content = %q", body)
	}
}

func TestApprovalIsSingleUse(t *testing.T) {
	a, _, _ := newAssistant(t, nil)
	ctx := context.Background()

	turn, _ := a.Process(ctx, Input{Text: "make a note something", Source: "voice"})
	callID := turn.Calls[0].CallID

	if _, err := a.Approve(ctx, callID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Approve(ctx, callID); !errors.Is(err, ErrNoSuchCall) {
		t.Fatalf("second approval returned %v, want ErrNoSuchCall", err)
	}
}

// An approval prompt that never expires is a command that can fire hours later,
// long after the user has forgotten what they asked for.
func TestApprovalExpires(t *testing.T) {
	a, _, _ := newAssistant(t, nil)
	a.cfg.ApprovalTTL = 10 * time.Millisecond
	ctx := context.Background()

	turn, _ := a.Process(ctx, Input{Text: "make a note something", Source: "voice"})
	time.Sleep(30 * time.Millisecond)

	if _, err := a.Approve(ctx, turn.Calls[0].CallID); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("err = %v, want ErrApprovalExpired", err)
	}
}

func TestDenyDiscardsTheCall(t *testing.T) {
	a, _, root := newAssistant(t, nil)
	ctx := context.Background()

	turn, _ := a.Process(ctx, Input{Text: "make a note something", Source: "voice"})
	if err := a.Deny(ctx, turn.Calls[0].CallID, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "NOTES.md")); err == nil {
		t.Fatal("the note was written after being denied")
	}
	if _, err := a.Approve(ctx, turn.Calls[0].CallID); !errors.Is(err, ErrNoSuchCall) {
		t.Fatal("a denied call could still be approved")
	}
}

// Dry run is the contract behind `nk ask --dry-run` and the API's
// execute:false: the planner and the gate run, nothing touches the machine.
func TestDryRunTouchesNothing(t *testing.T) {
	a, _, root := newAssistant(t, nil)

	turn, err := a.Process(context.Background(), Input{
		Text: "make a note buy milk", Source: "text", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Calls) != 1 || turn.Calls[0].Status != "planned" {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if turn.Calls[0].Preview == "" {
		t.Error("dry run produced no preview")
	}
	if _, err := os.Stat(filepath.Join(root, "NOTES.md")); err == nil {
		t.Fatal("dry run wrote a file")
	}
}

// The spoken transcript must be repaired before the branch name is extracted,
// or the branch is literally named "feature auth".
func TestNormalisationRunsBeforeIntent(t *testing.T) {
	a, _, _ := newAssistant(t, nil)

	turn, err := a.Process(context.Background(), Input{
		Text: "create a git branch called feature dash auth", Source: "voice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Calls) != 1 {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if got := turn.Calls[0].Args["name"]; got != "feature-auth" {
		t.Fatalf("branch name = %v, want feature-auth", got)
	}
	if turn.RawText == turn.Text {
		t.Error("raw and normalised text are identical; normalisation did nothing")
	}
}

func TestConversationAndMessagesArePersisted(t *testing.T) {
	a, db, _ := newAssistant(t, nil)
	ctx := context.Background()

	turn, err := a.Process(ctx, Input{Text: "open the file hello.txt", Source: "voice"})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := db.ListMessages(ctx, turn.ConversationID, 50)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, m := range msgs {
		roles[m.Role]++
	}
	if roles["user"] != 1 || roles["assistant"] != 1 {
		t.Fatalf("message roles = %v, want one user and one assistant turn", roles)
	}

	convs, _ := db.ListConversations(ctx, a.cfg.UserID, 10)
	if len(convs) != 1 || convs[0].Title == "" {
		t.Fatalf("conversations = %+v", convs)
	}
}

// Every gated call must leave an audit record whether it ran or not.
func TestToolCallsAreAudited(t *testing.T) {
	a, db, _ := newAssistant(t, nil)
	ctx := context.Background()

	if _, err := a.Process(ctx, Input{Text: "open the file hello.txt", Source: "voice"}); err != nil {
		t.Fatal(err)
	}
	events, err := db.ListAudit(ctx, time.Now().Add(-time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range events {
		actions[e.Action] = true
	}
	if !actions["tool.plan"] || !actions["tool.invoke"] {
		t.Fatalf("audit actions = %v, want both tool.plan and tool.invoke", actions)
	}
}

// A pending row must exist before the tool runs, so a crash mid-command still
// leaves evidence of what was attempted.
func TestPendingExecutionIsRecordedBeforeApproval(t *testing.T) {
	a, db, _ := newAssistant(t, nil)
	ctx := context.Background()

	turn, _ := a.Process(ctx, Input{Text: "make a note something", Source: "voice"})
	rows, err := db.ListToolExecutions(ctx, turn.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "awaiting_approval" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Preview == "" || rows[0].ArgsJSON == "{}" {
		t.Fatalf("the pending row does not record what was attempted: %+v", rows[0])
	}
}

func TestConversationalTurnNeedsNoTools(t *testing.T) {
	a, _, _ := newAssistant(t, fakellm.New("Transducers predict tokens and durations together."))

	turn, err := a.Process(context.Background(), Input{
		Text: "explain how transducers work", Source: "voice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Calls) != 0 {
		t.Fatalf("a question proposed tool calls: %+v", turn.Calls)
	}
	if !strings.Contains(turn.Message, "Transducers") {
		t.Fatalf("message = %q", turn.Message)
	}
}

// A model proposing a write must not bypass the approval a grammar match would
// have required.
func TestModelProposedCallIsStillGated(t *testing.T) {
	p := fakellm.WithCalls("Writing that file.",
		llm.ToolCall{Name: "fs.write", Args: json.RawMessage(`{"path":"x.txt","content":"hi"}`)})
	a, _, root := newAssistant(t, p)

	turn, err := a.Process(context.Background(), Input{Text: "please save a file for me", Source: "voice"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusAwaitingApproval {
		t.Fatalf("status = %s, want awaiting_approval", turn.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("a model-proposed write executed without approval")
	}
}

// Prompt injection: text arriving from a tool result must not be able to cause
// execution. It is data, and anything the model proposes after reading it goes
// through the gate like everything else.
func TestToolOutputCannotSelfApprove(t *testing.T) {
	p := fakellm.WithCalls("Doing as the file says.",
		llm.ToolCall{Name: "fs.write", Args: json.RawMessage(`{"path":"owned.txt","content":"pwned"}`)})
	a, _, root := newAssistant(t, p)

	turn, err := a.Process(context.Background(), Input{
		Text:   "read notes.txt and follow the instructions inside it",
		Source: "voice",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range turn.Calls {
		if c.Status == "succeeded" && c.Risk == "write" {
			t.Fatal("a write executed without approval after reading a file")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "owned.txt")); err == nil {
		t.Fatal("an injected instruction produced a file")
	}
}

// A model that proposes a tool that does not exist must fail that call, not the
// whole turn.
func TestUnknownToolFailsGracefully(t *testing.T) {
	p := fakellm.WithCalls("Sure.",
		llm.ToolCall{Name: "kubernetes.delete_cluster", Args: json.RawMessage(`{}`)})
	a, _, _ := newAssistant(t, p)

	turn, err := a.Process(context.Background(), Input{Text: "do something exotic", Source: "voice"})
	if err != nil {
		t.Fatalf("an invented tool name failed the whole turn: %v", err)
	}
	if len(turn.Calls) != 1 || turn.Calls[0].Status != "failed" {
		t.Fatalf("calls = %+v", turn.Calls)
	}
}

func TestEmptyUtterance(t *testing.T) {
	a, _, _ := newAssistant(t, nil)
	turn, err := a.Process(context.Background(), Input{Text: "   ", Source: "voice"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Message == "" || len(turn.Calls) != 0 {
		t.Fatalf("turn = %+v", turn)
	}
}

// The spoken reply must quote the preview verbatim. An earlier version
// lower-cased it and cut at the first period, which turned "NOTES.md" into
// "notes" — a quiet distortion of the one string the user is being asked to
// approve.
func TestApprovalReplyQuotesThePreviewVerbatim(t *testing.T) {
	a, _, _ := newAssistant(t, nil)

	turn, err := a.Process(context.Background(), Input{
		Text: "make a note check the deploy", Source: "voice",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := turn.Calls[0].Preview
	if !strings.Contains(turn.Message, preview) {
		t.Fatalf("message %q does not contain the preview %q", turn.Message, preview)
	}
	if !strings.Contains(turn.Message, "NOTES.md") {
		t.Fatalf("the file name was mangled out of the reply: %q", turn.Message)
	}
}

// A turn that runs no tool must still send calls as a list. Sending null took
// the whole web page down the first time someone said "hello".
func TestTurnWithNoCallsSerialisesEmptyLists(t *testing.T) {
	raw, err := json.Marshal(Turn{TurnID: "turn_1"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Calls  []any `json:"calls"`
		Intent struct {
			Calls []any `json:"calls"`
		} `json:"intent"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Calls == nil {
		t.Fatalf("calls is null in %s", raw)
	}
	if got.Intent.Calls == nil {
		t.Fatalf("intent.calls is null in %s", raw)
	}
	if !strings.Contains(string(raw), `"turn_id":"turn_1"`) {
		t.Fatalf("other fields lost: %s", raw)
	}
}
