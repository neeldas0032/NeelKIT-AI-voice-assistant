// Package runner turns an approved tool call into an executed one.
//
// Everything that happens to every call happens here: schema decoding, the
// policy gate, the human-readable preview, the audit record, the timeout and
// the metrics. Putting it in one place is what makes those guarantees uniform
// rather than per-tool good intentions.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/shellpreview"
)

// Call is a proposed tool invocation.
type Call struct {
	ID   string         `json:"call_id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Plan is a call that has been validated and gated but not yet run.
type Plan struct {
	Call
	Descriptor tools.Descriptor `json:"-"`
	Risk       tools.RiskLevel  `json:"risk"`
	Decision   policy.Result    `json:"decision"`
	// Preview is the resolved command in the user's terms. It is what the
	// approval card shows, and an integration test asserts it describes the
	// same command that actually runs.
	Preview string `json:"preview"`
}

// NeedsApproval reports whether a human must confirm before execution.
func (p Plan) NeedsApproval() bool { return p.Decision.Decision == policy.Confirm }

// Denied reports whether the plan may never run.
func (p Plan) Denied() bool { return p.Decision.Decision == policy.Deny }

// Audit records attempts. It is called before execution and after, so a process
// killed mid-command still leaves evidence of what was attempted.
type Audit interface {
	ToolStarted(ctx context.Context, p Plan) error
	ToolFinished(ctx context.Context, p Plan, res tools.Result, err error, d time.Duration)
	ToolDenied(ctx context.Context, p Plan)
}

// NopAudit discards audit events. Used in tests and when storage is disabled.
type NopAudit struct{}

func (NopAudit) ToolStarted(context.Context, Plan) error { return nil }
func (NopAudit) ToolFinished(context.Context, Plan, tools.Result, error, time.Duration) {
}
func (NopAudit) ToolDenied(context.Context, Plan) {}

// Observer receives timing for metrics.
type Observer func(tool string, outcome string, d time.Duration)

// Config configures a Runner.
type Config struct {
	Registry *tools.Registry
	Sandbox  tools.Sandbox
	Policy   policy.Settings
	Audit    Audit
	Observe  Observer
	// WorkspaceConfig carries workspace settings such as named tasks.
	WorkspaceConfig map[string]string
	// Timeout bounds one tool call.
	Timeout time.Duration
}

// Runner plans and executes tool calls.
type Runner struct {
	cfg Config
}

// New returns a runner.
func New(cfg Config) *Runner {
	if cfg.Registry == nil {
		cfg.Registry = tools.Default
	}
	if cfg.Audit == nil {
		cfg.Audit = NopAudit{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Runner{cfg: cfg}
}

// Registry exposes the tool registry, for the API and the planner.
func (r *Runner) Registry() *tools.Registry { return r.cfg.Registry }

// Plan validates a call and applies the policy gate. It never touches the
// machine, so it is also the dry-run path used by `nk ask --dry-run` and by
// the tests that assert policy decisions without side effects.
func (r *Runner) Plan(ctx context.Context, c Call) (Plan, error) {
	t, err := r.cfg.Registry.Lookup(c.Tool)
	if err != nil {
		return Plan{}, err
	}
	d := t.Descriptor()

	// Decode before gating: a malformed call must be rejected before a human
	// is asked to approve it, because nobody can meaningfully approve
	// arguments they cannot read.
	raw, err := json.Marshal(c.Args)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", tools.ErrInvalidArgs, err)
	}
	if err := validateAgainstSchema(d.Schema, c.Args); err != nil {
		return Plan{}, err
	}
	_ = raw

	p := Plan{
		Call:       c,
		Descriptor: d,
		Risk:       d.Risk,
		Decision:   policy.Decide(d, c.Args, r.cfg.Policy),
	}
	p.Preview = preview(d, c.Args, r.cfg.WorkspaceConfig, r.cfg.Sandbox)
	return p, nil
}

// Execute runs a planned call. A plan that still needs approval, or that was
// denied, is refused here: approval is recorded by the caller changing the
// decision, never by the runner assuming it.
func (r *Runner) Execute(ctx context.Context, p Plan) (tools.Result, error) {
	if p.Denied() {
		r.cfg.Audit.ToolDenied(ctx, p)
		return tools.Result{}, fmt.Errorf("%w: %s", tools.ErrDenied, p.Decision.Reason)
	}
	if p.NeedsApproval() {
		return tools.Result{}, fmt.Errorf("%w: %s requires approval", tools.ErrDenied, p.Tool)
	}

	t, err := r.cfg.Registry.Lookup(p.Tool)
	if err != nil {
		return tools.Result{}, err
	}
	raw, err := json.Marshal(p.Args)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrInvalidArgs, err)
	}

	// The audit row is written before the call, so a crash mid-execution still
	// leaves a record of what was attempted with what arguments.
	if err := r.cfg.Audit.ToolStarted(ctx, p); err != nil {
		return tools.Result{}, fmt.Errorf("audit: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	start := time.Now()
	res, execErr := t.Execute(ctx, tools.Input{
		Args:    raw,
		Sandbox: r.cfg.Sandbox,
		Config:  r.cfg.WorkspaceConfig,
		Turn: tools.TurnContext{
			CallID: p.ID,
		},
	})
	elapsed := time.Since(start)

	r.cfg.Audit.ToolFinished(ctx, p, res, execErr, elapsed)
	if r.cfg.Observe != nil {
		outcome := "ok"
		if execErr != nil {
			outcome = "error"
		}
		r.cfg.Observe(p.Tool, outcome, elapsed)
	}
	return res, execErr
}

// Approve returns a copy of the plan with the decision changed to allow,
// recording who approved it. This is the only way a Confirm plan becomes
// runnable, and it is deliberately explicit rather than a flag on Execute.
func Approve(p Plan) Plan {
	p.Decision.Decision = policy.Allow
	p.Decision.Reason = "approved by you"
	return p
}

// preview renders the resolved command in the user's terms.
//
// The approval card must show what will actually run, not the intent name and
// not the transcript. Showing "Create a branch?" while running something else
// is precisely the failure the gate exists to prevent.
func preview(d tools.Descriptor, args map[string]any, cfg map[string]string, sb tools.Sandbox) string {
	root := ""
	if sb != nil {
		root = "  (in " + sb.Root() + ")"
	}
	str := func(k string) string { v, _ := args[k].(string); return v }

	switch d.Name {
	case "git.branch_create":
		return "git switch -c " + str("name") + root
	case "git.status":
		return "git status" + root
	case "git.diff":
		return "git diff" + root
	case "git.log":
		return "git log" + root
	case "shell.task":
		name := str("task")
		// Resolved from the same task table Execute uses, so the preview and
		// the execution cannot describe different commands.
		if cmd, ok := shellpreview.Resolve(cfg, name); ok {
			return cmd + root
		}
		return "unknown task " + name
	case "fs.write":
		content, _ := args["content"].(string)
		return fmt.Sprintf("write %d bytes to %s%s", len(content), str("path"), root)
	case "fs.read":
		return "read " + str("path") + root
	case "fs.list":
		return "list " + str("path") + root
	case "search.code":
		return "search for " + quote(str("query")) + root
	case "notes.create":
		return "append a note to NOTES.md" + root
	case "http.request":
		method := str("method")
		if method == "" {
			method = "GET"
		}
		return method + " " + str("url")
	}

	// Generic fallback: the tool name and its arguments, sorted so the same
	// call always previews identically.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(d.Name)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, args[k])
	}
	return b.String() + root
}

func quote(s string) string { return `"` + s + `"` }

// validateAgainstSchema performs the structural checks that matter without
// pulling in a full JSON Schema implementation: required fields must be
// present and non-empty, and unknown fields are rejected.
//
// A complete validator is a dependency and a lot of surface for what the
// schemas here actually express. The tools themselves decode with
// DisallowUnknownFields, so anything this misses is caught one layer down —
// but catching it here means the user sees a clear error instead of a decode
// failure inside a tool.
func validateAgainstSchema(schema json.RawMessage, args map[string]any) error {
	var s struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil // a malformed schema is caught at registration
	}
	for _, req := range s.Required {
		v, ok := args[req]
		if !ok || v == nil {
			return fmt.Errorf("%w: missing required argument %q", tools.ErrInvalidArgs, req)
		}
		if str, isStr := v.(string); isStr && strings.TrimSpace(str) == "" {
			return fmt.Errorf("%w: argument %q is empty", tools.ErrInvalidArgs, req)
		}
	}
	if s.AdditionalProperties != nil && !*s.AdditionalProperties {
		for k := range args {
			if _, ok := s.Properties[k]; !ok {
				return fmt.Errorf("%w: unknown argument %q", tools.ErrInvalidArgs, k)
			}
		}
	}
	return nil
}
