package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/sandbox"

	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fsx"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
)

// recordingSandbox captures the argv actually executed, so the preview shown to
// the user can be compared against it.
type recordingSandbox struct {
	tools.Sandbox
	argv []string
}

func (r *recordingSandbox) Run(ctx context.Context, name string, args ...string) (tools.ExecResult, error) {
	r.argv = append([]string{name}, args...)
	return tools.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
}

func newRunner(t *testing.T) (*Runner, *sandbox.Sandbox, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.New(sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	return New(Config{Sandbox: sb, Policy: policy.DefaultSettings()}), sb, root
}

func TestPlanAppliesPolicy(t *testing.T) {
	r, _, _ := newRunner(t)

	read, err := r.Plan(context.Background(), Call{ID: "c1", Tool: "fs.read", Args: map[string]any{"path": "hello.txt"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if read.Decision.Decision != policy.Allow {
		t.Errorf("fs.read -> %s, want allow", read.Decision.Decision)
	}

	write, err := r.Plan(context.Background(), Call{ID: "c2", Tool: "fs.write",
		Args: map[string]any{"path": "out.txt", "content": "x"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !write.NeedsApproval() {
		t.Errorf("fs.write -> %s, want confirm", write.Decision.Decision)
	}
}

// A plan awaiting approval must not be runnable. If Execute honoured the
// intent rather than the recorded decision, the whole gate would be advisory.
func TestExecuteRefusesUnapprovedPlan(t *testing.T) {
	r, _, _ := newRunner(t)

	p, err := r.Plan(context.Background(), Call{ID: "c", Tool: "fs.write",
		Args: map[string]any{"path": "out.txt", "content": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("Execute on an unapproved plan returned %v, want ErrDenied", err)
	}

	if _, err := r.Execute(context.Background(), Approve(p)); err != nil {
		t.Fatalf("Execute after approval: %v", err)
	}
}

func TestExecuteRefusesDeniedPlan(t *testing.T) {
	r, _, _ := newRunner(t)

	p, err := r.Plan(context.Background(), Call{ID: "c", Tool: "fs.write",
		Args: map[string]any{"path": "../escape.txt", "content": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Denied() {
		t.Fatalf("path traversal -> %s, want deny", p.Decision.Decision)
	}
	// Approving a denied plan must not make it runnable.
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("Execute on a denied plan returned %v", err)
	}
}

// The approval card must describe the command that actually runs. Showing one
// thing and running another is the exact failure the gate exists to prevent.
func TestPreviewMatchesExecutedCommand(t *testing.T) {
	root := t.TempDir()
	base, err := sandbox.New(sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	rec := &recordingSandbox{Sandbox: base}

	r := New(Config{
		Sandbox:         rec,
		Policy:          policy.DefaultSettings(),
		WorkspaceConfig: map[string]string{"task.test": "go test ./internal/..."},
	})

	cases := []struct {
		call Call
		want string
	}{
		{Call{ID: "1", Tool: "git.branch_create", Args: map[string]any{"name": "feature-auth"}},
			"git switch -c feature-auth"},
		{Call{ID: "2", Tool: "shell.task", Args: map[string]any{"task": "test"}},
			"go test ./internal/..."},
	}

	for _, c := range cases {
		p, err := r.Plan(context.Background(), c.call)
		if err != nil {
			t.Fatalf("Plan %s: %v", c.call.Tool, err)
		}
		if !strings.HasPrefix(p.Preview, c.want) {
			t.Fatalf("%s preview = %q, want it to start with %q", c.call.Tool, p.Preview, c.want)
		}
		rec.argv = nil
		if _, err := r.Execute(context.Background(), Approve(p)); err != nil {
			t.Fatalf("Execute %s: %v", c.call.Tool, err)
		}
		got := strings.Join(rec.argv, " ")
		if got != c.want {
			t.Fatalf("%s: previewed %q but executed %q", c.call.Tool, c.want, got)
		}
	}
}

func TestPlanRejectsUnknownTool(t *testing.T) {
	r, _, _ := newRunner(t)
	if _, err := r.Plan(context.Background(), Call{Tool: "does.not_exist"}); !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A model that invents an argument name is wrong. Ignoring the extra field
// would execute something other than what the preview showed.
func TestPlanRejectsUnknownAndMissingArguments(t *testing.T) {
	r, _, _ := newRunner(t)

	if _, err := r.Plan(context.Background(), Call{Tool: "fs.read",
		Args: map[string]any{"path": "hello.txt", "recursive": true}}); !errors.Is(err, tools.ErrInvalidArgs) {
		t.Errorf("unknown argument accepted: %v", err)
	}
	if _, err := r.Plan(context.Background(), Call{Tool: "fs.read", Args: map[string]any{}}); !errors.Is(err, tools.ErrInvalidArgs) {
		t.Errorf("missing required argument accepted: %v", err)
	}
	if _, err := r.Plan(context.Background(), Call{Tool: "fs.read",
		Args: map[string]any{"path": "   "}}); !errors.Is(err, tools.ErrInvalidArgs) {
		t.Errorf("blank required argument accepted: %v", err)
	}
}

func TestAuditRecordsBeforeAndAfter(t *testing.T) {
	r, _, _ := newRunner(t)
	a := &recordingAudit{}
	r.cfg.Audit = a

	p, err := r.Plan(context.Background(), Call{ID: "c", Tool: "fs.read", Args: map[string]any{"path": "hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !a.started || !a.finished {
		t.Fatalf("audit: started=%v finished=%v, want both", a.started, a.finished)
	}
	if a.order != "started,finished" {
		t.Fatalf("audit order = %q, want the record written before execution", a.order)
	}
}

func TestDeniedCallsAreAudited(t *testing.T) {
	r, _, _ := newRunner(t)
	a := &recordingAudit{}
	r.cfg.Audit = a

	p, _ := r.Plan(context.Background(), Call{ID: "c", Tool: "fs.write",
		Args: map[string]any{"path": "../x", "content": "y"}})
	_, _ = r.Execute(context.Background(), p)
	if !a.denied {
		t.Fatal("a denied call left no audit record")
	}
}

func TestFsToolsRoundTrip(t *testing.T) {
	r, _, _ := newRunner(t)
	ctx := context.Background()

	p, err := r.Plan(ctx, Call{ID: "w", Tool: "fs.write", Args: map[string]any{"path": "note.md", "content": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(ctx, Approve(p)); err != nil {
		t.Fatal(err)
	}

	p, err = r.Plan(ctx, Call{ID: "r", Tool: "fs.read", Args: map[string]any{"path": "note.md"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(map[string]any)
	if data["content"] != "hello" {
		t.Fatalf("content = %v", data["content"])
	}
}

func TestEveryRegisteredToolHasAUsableDescriptor(t *testing.T) {
	for _, d := range tools.Default.Descriptors() {
		if !strings.Contains(d.Name, ".") {
			t.Errorf("%q is not namespaced", d.Name)
		}
		if len(d.Description) < 20 {
			t.Errorf("%q has a %d-character description; the model has little to go on", d.Name, len(d.Description))
		}
		if !d.Risk.Valid() {
			t.Errorf("%q declares invalid risk %q", d.Name, d.Risk)
		}
		if !json.Valid(d.Schema) {
			t.Errorf("%q has invalid JSON schema", d.Name)
		}
		if d.Title == "" {
			t.Errorf("%q has no title for the UI", d.Name)
		}
	}
	if tools.Default.Len() < 8 {
		t.Fatalf("only %d tools registered", tools.Default.Len())
	}
}

func TestRegistryRejectsDuplicatesAndBadDescriptors(t *testing.T) {
	reg := tools.NewRegistry()
	ok := &stubTool{name: "x.y", risk: tools.RiskRead}
	if err := reg.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ok); err == nil {
		t.Error("duplicate registration accepted")
	}
	for _, bad := range []*stubTool{
		{name: "", risk: tools.RiskRead},
		{name: "nodot", risk: tools.RiskRead},
		{name: "a.b", risk: "bogus"},
		{name: "a.c", risk: tools.RiskRead, noDesc: true},
		{name: "a.d", risk: tools.RiskRead, badSchema: true},
	} {
		if err := reg.Register(bad); err == nil {
			t.Errorf("registry accepted a bad descriptor: %+v", bad)
		}
	}
}

type stubTool struct {
	name      string
	risk      tools.RiskLevel
	noDesc    bool
	badSchema bool
}

func (s *stubTool) Descriptor() tools.Descriptor {
	d := tools.Descriptor{Name: s.name, Title: "t", Description: "a stub tool for tests",
		Risk: s.risk, Schema: json.RawMessage(`{"type":"object"}`)}
	if s.noDesc {
		d.Description = ""
	}
	if s.badSchema {
		d.Schema = json.RawMessage(`{not json`)
	}
	return d
}
func (s *stubTool) Execute(context.Context, tools.Input) (tools.Result, error) {
	return tools.Result{}, nil
}

type recordingAudit struct {
	started, finished, denied bool
	order                     string
}

func (a *recordingAudit) ToolStarted(context.Context, Plan) error {
	a.started = true
	a.order += "started"
	return nil
}
func (a *recordingAudit) ToolFinished(context.Context, Plan, tools.Result, error, time.Duration) {
	a.finished = true
	a.order += ",finished"
}
func (a *recordingAudit) ToolDenied(context.Context, Plan) { a.denied = true }
