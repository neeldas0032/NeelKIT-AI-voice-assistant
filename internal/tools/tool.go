// Package tools defines what the assistant is allowed to do, and the contract
// every capability implements.
//
// The design goal is that adding a capability never requires editing the
// assistant. A tool declares its own name, description, argument schema and
// risk level; the registry collects them; the planner builds the model's
// contract from the registry at request time; the policy gate reads the risk
// level; the runner logs and times every call uniformly. None of that is
// per-tool code.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors callers distinguish.
var (
	// ErrNotFound means no tool is registered under that name.
	ErrNotFound = errors.New("tools: no such tool")
	// ErrInvalidArgs means the arguments failed validation. It is a client
	// error, never a server fault.
	ErrInvalidArgs = errors.New("tools: invalid arguments")
	// ErrDenied means the policy gate refused the call.
	ErrDenied = errors.New("tools: denied by policy")
	// ErrOutsideWorkspace means a path resolved outside the sandbox root.
	ErrOutsideWorkspace = errors.New("tools: path is outside the workspace")
)

// RiskLevel describes what a tool can do to the machine. It is declared by the
// tool, never inferred by the caller: the policy engine must not have to guess.
type RiskLevel string

const (
	// RiskRead observes only: reading a file, listing a directory, git log.
	RiskRead RiskLevel = "read"
	// RiskWrite mutates local state: writing a file, creating a branch.
	RiskWrite RiskLevel = "write"
	// RiskExecute runs a program. The blast radius is arbitrary code, so this
	// is never auto-approved regardless of settings.
	RiskExecute RiskLevel = "execute"
	// RiskNetwork leaves the machine. Visibility here is what makes the
	// local-first promise checkable rather than just claimed.
	RiskNetwork RiskLevel = "network"
)

// Rank orders risk levels so decisions can be compared and escalated.
func (r RiskLevel) Rank() int {
	switch r {
	case RiskRead:
		return 0
	case RiskWrite:
		return 1
	case RiskNetwork:
		return 2
	case RiskExecute:
		return 3
	}
	return 3 // unknown risk is treated as the most dangerous
}

// Valid reports whether r is a known level.
func (r RiskLevel) Valid() bool {
	switch r {
	case RiskRead, RiskWrite, RiskExecute, RiskNetwork:
		return true
	}
	return false
}

// Example is a few-shot example, used both in the model's prompt and in the
// generated documentation, so the two can never drift apart.
type Example struct {
	Utterance string          `json:"utterance"`
	Args      json.RawMessage `json:"args"`
}

// Descriptor is everything the system knows about a tool without running it.
type Descriptor struct {
	// Name is namespace.verb, e.g. "git.branch_create". It is an API: changing
	// it breaks saved policy overrides and audit history.
	Name string `json:"name"`
	// Title is the human label shown in the UI.
	Title string `json:"title"`
	// Description is the model's entire understanding of what this does. It is
	// the highest-leverage string in the package.
	Description string `json:"description"`
	// Schema is JSON Schema for the arguments, served to the model, the API
	// and the UI's argument form from this one definition.
	Schema json.RawMessage `json:"schema"`
	// Risk drives the policy gate.
	Risk RiskLevel `json:"risk"`
	// Idempotent marks a tool that is safe to retry after an ambiguous
	// failure. git.commit is not; fs.read is.
	Idempotent bool `json:"idempotent"`
	// Examples help the planner and the docs.
	Examples []Example `json:"examples,omitempty"`
}

// Sandbox is the confined view of the machine a tool is given.
//
// Tools never resolve paths or build commands themselves. They receive this
// interface, which is the only route to the filesystem, to a subprocess or to
// the network, so containment is implemented and tested once rather than
// re-derived in every tool.
type Sandbox interface {
	// Root is the absolute workspace path, for display only.
	Root() string
	// ReadFile reads a file inside the workspace.
	ReadFile(name string) ([]byte, error)
	// WriteFile writes a file inside the workspace.
	WriteFile(name string, data []byte) error
	// ReadDir lists a directory inside the workspace.
	ReadDir(name string) ([]fs.DirEntry, error)
	// Stat stats a path inside the workspace.
	Stat(name string) (fs.FileInfo, error)
	// Run executes an allowlisted binary with an explicit argv. There is no
	// shell: metacharacters are not interpreted because nothing interprets
	// them.
	Run(ctx context.Context, name string, args ...string) (ExecResult, error)
	// ReadOnly reports whether write and execute tools are disabled.
	ReadOnly() bool
}

// ExecResult is the outcome of a subprocess.
type ExecResult struct {
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	ExitCode  int           `json:"exit_code"`
	Duration  time.Duration `json:"-"`
	Truncated bool          `json:"truncated"`
}

// TurnContext identifies the conversation a call belongs to, for audit and
// tracing.
type TurnContext struct {
	ConversationID string
	TurnID         string
	CallID         string
	RequestID      string
	UserID         string
}

// Input is one invocation.
type Input struct {
	Args    json.RawMessage
	Sandbox Sandbox
	Turn    TurnContext
	// Config carries workspace-scoped settings such as named shell tasks.
	Config map[string]string
}

// Artifact is a file a tool produced, surfaced in the UI for download.
type Artifact struct {
	Path string `json:"path"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
}

// Result is what a tool returns.
type Result struct {
	// Summary is one line, spoken back to the user. It is the only part that
	// reaches text-to-speech, so it must stand alone.
	Summary string `json:"summary"`
	// Data is structured output rendered by the UI.
	Data any `json:"data,omitempty"`
	// Stdout is captured output, already truncated.
	Stdout string `json:"stdout,omitempty"`
	// ExitCode is meaningful for execute-class tools.
	ExitCode int `json:"exit_code,omitempty"`
	// Artifacts are files produced by the call.
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Tool is a capability. Implementations must be safe for concurrent use: one
// registered instance serves every request.
type Tool interface {
	Descriptor() Descriptor
	Execute(ctx context.Context, in Input) (Result, error)
}

// Decode unmarshals arguments, rejecting unknown fields.
//
// Strictness here is deliberate. A model that invents an argument name is
// wrong, and silently ignoring it would execute something other than what the
// user approved in the preview.
func Decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	return nil
}

// Registry holds the registered tools.
//
// Registration happens in each tool package's init, and cmd/neelkitd decides
// which packages to link in by blank-importing them. That import list is the
// plugin manifest, and it is the only file outside a tool's own package that
// changes when a tool is added.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry. Tests use this for isolation; the
// process uses Default.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Default is the registry package init functions write to.
var Default = NewRegistry()

// Register adds a tool.
func (r *Registry) Register(t Tool) error {
	d := t.Descriptor()
	if d.Name == "" {
		return fmt.Errorf("tools: descriptor has no name")
	}
	if !strings.Contains(d.Name, ".") {
		return fmt.Errorf("tools: %q must be namespaced as namespace.verb", d.Name)
	}
	if d.Description == "" {
		return fmt.Errorf("tools: %q has no description; the model has nothing to go on", d.Name)
	}
	if !d.Risk.Valid() {
		return fmt.Errorf("tools: %q declares unknown risk %q", d.Name, d.Risk)
	}
	if len(d.Schema) == 0 {
		return fmt.Errorf("tools: %q has no argument schema", d.Name)
	}
	if !json.Valid(d.Schema) {
		return fmt.Errorf("tools: %q has an invalid JSON schema", d.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[d.Name]; dup {
		return fmt.Errorf("tools: %q is already registered", d.Name)
	}
	r.tools[d.Name] = t
	return nil
}

// MustRegister registers or panics.
//
// A duplicate name or a malformed schema is a programming error that must stop
// the process at startup, not surface at 2am on the first invocation.
func MustRegister(t Tool) {
	if err := Default.Register(t); err != nil {
		panic(err)
	}
}

// Lookup returns a tool by name.
func (r *Registry) Lookup(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return t, nil
}

// Descriptors returns every descriptor, sorted by name so the model's prompt
// and the API response are byte-stable between runs.
func (r *Registry) Descriptors() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Descriptor())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Names returns registered tool names, sorted.
func (r *Registry) Names() []string {
	ds := r.Descriptors()
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}
