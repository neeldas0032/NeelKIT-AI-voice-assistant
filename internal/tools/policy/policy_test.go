package policy

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

func desc(name string, risk tools.RiskLevel) tools.Descriptor {
	return tools.Descriptor{
		Name: name, Title: name, Description: "test tool",
		Schema: json.RawMessage(`{"type":"object"}`), Risk: risk,
	}
}

func TestRiskDefaults(t *testing.T) {
	s := DefaultSettings()
	cases := map[tools.RiskLevel]Decision{
		tools.RiskRead:    Allow,
		tools.RiskWrite:   Confirm,
		tools.RiskExecute: Confirm,
		tools.RiskNetwork: Confirm,
	}
	for risk, want := range cases {
		got := Decide(desc("x.y", risk), nil, s)
		if got.Decision != want {
			t.Errorf("risk %s -> %s, want %s (%s)", risk, got.Decision, want, got.Reason)
		}
	}
}

// The asymmetry that makes the gate trustworthy: argument inspection may only
// ever make a decision stricter. If any input could relax one, the whole model
// collapses, so this is asserted against randomised input rather than a fixed
// list of cases.
func TestDecisionsAreNeverWeakenedByArguments(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	risks := []tools.RiskLevel{tools.RiskRead, tools.RiskWrite, tools.RiskExecute, tools.RiskNetwork}
	keys := []string{"name", "branch", "path", "file", "url", "query", "text", "task"}
	values := []string{
		"main", "feature-auth", "../../etc/passwd", "~/.ssh/id_rsa", "src/main.go",
		"http://169.254.169.254/", "https://example.com", "", ".git/config",
		"node_modules/x", "/etc/shadow", "release", "PRODUCTION",
	}

	for i := 0; i < 5000; i++ {
		risk := risks[rng.Intn(len(risks))]
		args := map[string]any{}
		for j := 0; j < rng.Intn(4); j++ {
			args[keys[rng.Intn(len(keys))]] = values[rng.Intn(len(values))]
		}
		s := DefaultSettings()
		base := Decide(desc("t.t", risk), nil, s)
		got := Decide(desc("t.t", risk), args, s)
		if rank(got.Decision) < rank(base.Decision) {
			t.Fatalf("args %v weakened %s from %s to %s", args, risk, base.Decision, got.Decision)
		}
	}
}

// "Always allow running commands" is not a preference a user can hold safely,
// so no override can grant it.
func TestOverrideCannotAutoApproveExecution(t *testing.T) {
	s := DefaultSettings()
	s.Overrides = map[string]Decision{"shell.task": Allow}

	got := Decide(desc("shell.task", tools.RiskExecute), nil, s)
	if got.Decision != Confirm {
		t.Fatalf("execute tool with an Allow override -> %s, want confirm", got.Decision)
	}
}

func TestOverrideCanRelaxWrite(t *testing.T) {
	s := DefaultSettings()
	s.Overrides = map[string]Decision{"notes.create": Allow}

	if got := Decide(desc("notes.create", tools.RiskWrite), nil, s); got.Decision != Allow {
		t.Fatalf("remembered write override -> %s, want allow", got.Decision)
	}
}

func TestExecuteAlwaysConfirmsEvenWhenSettingsSayAllow(t *testing.T) {
	s := DefaultSettings()
	s.Execute = Allow

	got := Decide(desc("shell.task", tools.RiskExecute), nil, s)
	if got.Decision != Confirm || !got.Escalated {
		t.Fatalf("got %+v, want an escalated confirm", got)
	}
}

func TestProtectedBranchEscalates(t *testing.T) {
	s := DefaultSettings()
	s.Write = Allow

	for _, name := range []string{"main", "MAIN", "production", " release "} {
		got := Decide(desc("git.branch_create", tools.RiskWrite), map[string]any{"name": name}, s)
		if got.Decision != Confirm {
			t.Errorf("branch %q -> %s, want confirm", name, got.Decision)
		}
	}
	got := Decide(desc("git.branch_create", tools.RiskWrite), map[string]any{"name": "feature-auth"}, s)
	if got.Decision != Allow {
		t.Errorf("ordinary branch -> %s, want allow when write is allowed", got.Decision)
	}
}

func TestDangerousPathsAreDenied(t *testing.T) {
	s := DefaultSettings()
	for _, p := range []string{"../../etc/passwd", "~/.ssh/id_rsa", "/etc/shadow", ".git/config", "node_modules/pkg"} {
		got := Decide(desc("fs.write", tools.RiskWrite), map[string]any{"path": p}, s)
		if got.Decision != Deny {
			t.Errorf("write to %q -> %s, want deny", p, got.Decision)
		}
	}
	// Reading such a path is escalated rather than denied: the sandbox will
	// refuse it anyway, and the user should see what was attempted.
	got := Decide(desc("fs.read", tools.RiskRead), map[string]any{"path": "../secrets"}, s)
	if got.Decision != Confirm {
		t.Errorf("read of a suspicious path -> %s, want confirm", got.Decision)
	}
}

func TestNetworkTargetsAreScreened(t *testing.T) {
	s := DefaultSettings()
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/admin",
		"http://127.0.0.1/",
		"https://db.internal/query",
		"file:///etc/passwd",
		"ftp://example.com",
	} {
		if got := Decide(desc("http.request", tools.RiskNetwork), map[string]any{"url": u}, s); got.Decision != Deny {
			t.Errorf("url %q -> %s, want deny", u, got.Decision)
		}
	}
	if got := Decide(desc("http.request", tools.RiskNetwork), map[string]any{"url": "https://example.com/api"}, s); got.Decision != Confirm {
		t.Errorf("public url -> %s, want confirm", got.Decision)
	}
}

func TestReadOnlyModeDeniesEverythingButReads(t *testing.T) {
	s := DefaultSettings()
	s.ReadOnly = true

	for _, risk := range []tools.RiskLevel{tools.RiskWrite, tools.RiskExecute, tools.RiskNetwork} {
		if got := Decide(desc("x.y", risk), nil, s); got.Decision != Deny {
			t.Errorf("risk %s in read-only mode -> %s, want deny", risk, got.Decision)
		}
	}
	if got := Decide(desc("fs.read", tools.RiskRead), nil, s); got.Decision != Allow {
		t.Errorf("read in read-only mode -> %s, want allow", got.Decision)
	}
}

func TestUnknownRiskIsDenied(t *testing.T) {
	if got := Decide(desc("x.y", tools.RiskLevel("whatever")), nil, DefaultSettings()); got.Decision != Deny {
		t.Fatalf("unknown risk -> %s, want deny", got.Decision)
	}
}

func TestEveryDecisionCarriesAReason(t *testing.T) {
	// An approval prompt that cannot say why it is asking trains the user to
	// approve everything without reading it.
	s := DefaultSettings()
	for _, risk := range []tools.RiskLevel{tools.RiskRead, tools.RiskWrite, tools.RiskExecute, tools.RiskNetwork} {
		if got := Decide(desc("x.y", risk), map[string]any{"name": "main"}, s); got.Reason == "" {
			t.Errorf("risk %s produced a decision with no reason", risk)
		}
	}
}
