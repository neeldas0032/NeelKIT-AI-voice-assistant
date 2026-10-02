// Package policy decides whether a tool call may run.
//
// This is the control that makes a voice assistant with shell access
// defensible. Speech recognition makes mistakes and language models propose
// things nobody asked for; the gate assumes both and requires a human to
// confirm anything that writes, executes or leaves the machine.
//
// Decide is a pure function, which is what lets it be tested exhaustively.
package policy

import (
	"regexp"
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

// Decision is the outcome of the gate.
type Decision string

const (
	// Allow runs immediately.
	Allow Decision = "allow"
	// Confirm suspends the turn until a human approves the exact command.
	Confirm Decision = "confirm"
	// Deny refuses outright. No approval can override it.
	Deny Decision = "deny"
)

// rank orders decisions so they can only ever be escalated.
func rank(d Decision) int {
	switch d {
	case Allow:
		return 0
	case Confirm:
		return 1
	case Deny:
		return 2
	}
	return 2
}

// stricter returns the more restrictive of two decisions.
func stricter(a, b Decision) Decision {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Settings are the user's configured defaults, one per risk level.
type Settings struct {
	Read    Decision
	Write   Decision
	Execute Decision
	Network Decision
	// ReadOnly forces every non-read tool to Deny, for demos and screen shares.
	ReadOnly bool
	// ProtectedBranches are branch names that always require confirmation even
	// when Write is set to Allow.
	ProtectedBranches []string
	// Overrides are scoped exceptions the user created by approving a call
	// with "remember". The key is "tool" or "tool:arghash"; the value is the
	// decision. Overrides may only relax to Allow, and never for execute-class
	// tools — see Decide.
	Overrides map[string]Decision
}

// DefaultSettings are the shipped defaults: reading is free, everything else
// asks.
func DefaultSettings() Settings {
	return Settings{
		Read:              Allow,
		Write:             Confirm,
		Execute:           Confirm,
		Network:           Confirm,
		ProtectedBranches: []string{"main", "master", "develop", "release", "production", "prod"},
	}
}

// Result is the gate's verdict, with the reason that produced it. The reason is
// shown in the approval card: a prompt that cannot say why it is asking trains
// the user to approve everything.
type Result struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason"`
	// Escalated marks a decision made stricter by inspecting the arguments
	// rather than by the risk level alone.
	Escalated bool `json:"escalated"`
}

// Decide returns the verdict for one call.
//
// Argument inspection may make a decision stricter. It can never make one more
// permissive: the result is the strictest of the risk default and every
// argument rule, and a property test asserts no input ever produces something
// weaker than the risk default.
func Decide(d tools.Descriptor, args map[string]any, s Settings) Result {
	base, reason := baseDecision(d.Risk, s)

	if s.ReadOnly && d.Risk != tools.RiskRead {
		return Result{Decision: Deny, Reason: "workspace is in read-only mode", Escalated: true}
	}

	// An override may relax a decision, but never for execute-class tools.
	// "Always allow running commands" is not a preference a user can hold
	// safely, so it is not offered.
	if ov, ok := s.Overrides[d.Name]; ok && d.Risk != tools.RiskExecute {
		if rank(ov) < rank(base) {
			base, reason = ov, "you previously chose to always allow this"
		}
	}

	out := Result{Decision: base, Reason: reason}

	for _, rule := range argumentRules {
		esc, why := rule(d, args, s)
		if esc == "" {
			continue
		}
		if next := stricter(out.Decision, esc); next != out.Decision {
			out.Decision = next
			out.Reason = why
			out.Escalated = true
		}
	}
	return out
}

func baseDecision(r tools.RiskLevel, s Settings) (Decision, string) {
	switch r {
	case tools.RiskRead:
		return orDefault(s.Read, Allow), "reads only"
	case tools.RiskWrite:
		return orDefault(s.Write, Confirm), "modifies files in your workspace"
	case tools.RiskExecute:
		return orDefault(s.Execute, Confirm), "runs a program on your machine"
	case tools.RiskNetwork:
		return orDefault(s.Network, Confirm), "sends a request off your machine"
	}
	return Deny, "unknown risk level"
}

func orDefault(d, fallback Decision) Decision {
	if d == "" {
		return fallback
	}
	return d
}

// argumentRule inspects a call and optionally escalates it.
type argumentRule func(tools.Descriptor, map[string]any, Settings) (Decision, string)

var argumentRules = []argumentRule{
	ruleExecuteNeverAuto,
	ruleProtectedBranch,
	ruleDangerousPath,
	ruleNetworkTarget,
}

// ruleExecuteNeverAuto is the floor under every execute-class tool. Even if
// settings say Allow, running arbitrary programs by voice always asks.
func ruleExecuteNeverAuto(d tools.Descriptor, _ map[string]any, _ Settings) (Decision, string) {
	if d.Risk == tools.RiskExecute {
		return Confirm, "running a program always needs your confirmation"
	}
	return "", ""
}

var branchArgKeys = []string{"name", "branch", "ref", "target"}

func ruleProtectedBranch(d tools.Descriptor, args map[string]any, s Settings) (Decision, string) {
	if d.Risk == tools.RiskRead {
		return "", ""
	}
	for _, k := range branchArgKeys {
		v, _ := args[k].(string)
		if v == "" {
			continue
		}
		for _, p := range s.ProtectedBranches {
			if strings.EqualFold(strings.TrimSpace(v), p) {
				return Confirm, "“" + v + "” is a protected branch"
			}
		}
	}
	return "", ""
}

// suspiciousPath catches paths that leave the project subtree or target build
// and version-control internals. The sandbox already refuses to resolve them;
// escalating here means the user sees the attempt rather than a bare error.
var suspiciousPath = regexp.MustCompile(`(^|/)\.\.(/|$)|^~|^/|(^|/)\.git(/|$)|(^|/)node_modules(/|$)`)

func ruleDangerousPath(d tools.Descriptor, args map[string]any, _ Settings) (Decision, string) {
	for _, k := range []string{"path", "file", "dir", "target"} {
		v, _ := args[k].(string)
		if v == "" {
			continue
		}
		if suspiciousPath.MatchString(v) {
			if d.Risk == tools.RiskRead {
				return Confirm, "“" + v + "” points outside the project tree"
			}
			return Deny, "“" + v + "” points outside the project tree"
		}
	}
	return "", ""
}

// ruleNetworkTarget escalates outbound requests to anything that is not
// obviously public, so an attempt to reach a metadata service or an internal
// host is visible rather than silent.
func ruleNetworkTarget(d tools.Descriptor, args map[string]any, _ Settings) (Decision, string) {
	if d.Risk != tools.RiskNetwork {
		return "", ""
	}
	u, _ := args["url"].(string)
	if u == "" {
		return "", ""
	}
	lower := strings.ToLower(u)
	for _, bad := range []string{"localhost", "127.0.0.1", "0.0.0.0", "169.254.169.254", "::1", ".internal", ".local"} {
		if strings.Contains(lower, bad) {
			return Deny, "“" + u + "” targets a local or internal address"
		}
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return Deny, "only http and https URLs are allowed"
	}
	return "", ""
}
