package intent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	fakellm "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm/fake"

	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fsx"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
)

func TestGrammarTierAnswersWithoutTheModel(t *testing.T) {
	p := fakellm.New("should not be called")
	r := New(Config{Provider: p})

	in, err := r.Route(context.Background(), "create a git branch called feature auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Source != SourceGrammar {
		t.Fatalf("source = %s, want grammar", in.Source)
	}
	if in.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 for an exact rule match", in.Confidence)
	}
	if len(in.Calls) != 1 || in.Calls[0].Tool != "git.branch_create" {
		t.Fatalf("calls = %+v", in.Calls)
	}
	if in.Calls[0].Args["name"] != "feature-auth" {
		t.Errorf("name = %v", in.Calls[0].Args["name"])
	}
	if len(p.Requests) != 0 {
		t.Errorf("the model was called %d times for a grammar hit; it should never be", len(p.Requests))
	}
}

func TestLLMTierHandlesTheLongTail(t *testing.T) {
	p := fakellm.WithCalls("Reading the file now.",
		llm.ToolCall{Name: "fs.read", Args: json.RawMessage(`{"path":"src/main.go"}`)})
	r := New(Config{Provider: p})

	in, err := r.Route(context.Background(), "what does the main file actually do", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Source != SourceLLM {
		t.Fatalf("source = %s, want llm", in.Source)
	}
	if len(in.Calls) != 1 || in.Calls[0].Tool != "fs.read" {
		t.Fatalf("calls = %+v", in.Calls)
	}
	if in.Confidence >= 1.0 {
		t.Error("a model proposal should not claim certainty")
	}
}

// The model is told about the tools at request time, so a newly registered
// tool is immediately callable with no other change.
func TestModelReceivesTheToolRegistry(t *testing.T) {
	p := fakellm.New("ok")
	r := New(Config{Provider: p})

	if _, err := r.Route(context.Background(), "some freeform question", nil); err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 1 {
		t.Fatalf("model called %d times", len(p.Requests))
	}
	specs := p.Requests[0].Tools
	if len(specs) < 8 {
		t.Fatalf("model was told about %d tools, want the whole registry", len(specs))
	}
	seen := map[string]bool{}
	for _, s := range specs {
		seen[s.Name] = true
		if s.Description == "" || len(s.Schema) == 0 {
			t.Errorf("tool %q was described to the model without a description or schema", s.Name)
		}
	}
	for _, want := range []string{"fs.read", "git.status", "shell.task"} {
		if !seen[want] {
			t.Errorf("%q was not offered to the model", want)
		}
	}
}

// With no model configured the assistant must still work for built-in
// commands, and say so clearly for anything else. This is the local-first
// promise, tested.
func TestWorksWithNoModelConfigured(t *testing.T) {
	r := New(Config{})
	if r.HasModel() {
		t.Fatal("HasModel is true with no provider")
	}

	in, err := r.Route(context.Background(), "run the tests", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Source != SourceGrammar || len(in.Calls) != 1 {
		t.Fatalf("grammar tier broken without a model: %+v", in)
	}

	in, err = r.Route(context.Background(), "explain quantum entanglement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Source != SourceNone || in.Reply == "" {
		t.Fatalf("unmatched utterance without a model gave %+v; want a plain explanation", in)
	}
}

// An unreachable model degrades to a readable message rather than an error the
// user has to interpret.
func TestUnavailableModelDegradesGracefully(t *testing.T) {
	p := fakellm.New("")
	p.Err = llm.ErrUnavailable
	r := New(Config{Provider: p})

	in, err := r.Route(context.Background(), "something conversational", nil)
	if err != nil {
		t.Fatalf("an unreachable model should not fail the turn: %v", err)
	}
	if in.Reply == "" {
		t.Fatal("no explanation given to the user")
	}
	if len(in.Calls) != 0 {
		t.Fatal("calls proposed despite the model being unavailable")
	}
}

func TestHistoryIsTrimmed(t *testing.T) {
	p := fakellm.New("ok")
	r := New(Config{Provider: p, History: 3})

	var hist []llm.Message
	for i := 0; i < 20; i++ {
		hist = append(hist, llm.Message{Role: llm.RoleUser, Content: "old"})
	}
	if _, err := r.Route(context.Background(), "new question", hist); err != nil {
		t.Fatal(err)
	}
	// 3 trimmed history messages plus the current utterance.
	if got := len(p.Requests[0].Messages); got != 4 {
		t.Fatalf("model received %d messages, want 4 (3 history + current)", got)
	}
}

func TestEmptyUtterance(t *testing.T) {
	r := New(Config{})
	in, err := r.Route(context.Background(), "   ", nil)
	if err != nil || in.Source != SourceNone || len(in.Calls) != 0 {
		t.Fatalf("got %+v, %v", in, err)
	}
}

// stalled is a provider that is reachable but never answers in time, which is
// what a large model on a laptop CPU looks like from here.
type stalled struct{}

func (stalled) Name() string                 { return "stalled" }
func (stalled) Model() string                { return "slow" }
func (stalled) Health(context.Context) error { return nil }
func (stalled) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

// A model that is too slow must produce an answer the user can act on, not a
// failed turn — and must not be described as unreachable, which sends people
// hunting for a connection problem that does not exist.
func TestSlowModelDegradesToAReply(t *testing.T) {
	r := New(Config{Provider: stalled{}, Timeout: 50 * time.Millisecond})
	in, err := r.Route(context.Background(), "summarise what changed this week", nil)
	if err != nil {
		t.Fatalf("Route returned an error for a slow model: %v", err)
	}
	if !strings.Contains(in.Reply, "took longer") {
		t.Fatalf("reply = %q, want it to say the model was too slow", in.Reply)
	}
	if strings.Contains(in.Reply, "couldn't reach") {
		t.Fatalf("reply = %q blames connectivity for slowness", in.Reply)
	}
}
