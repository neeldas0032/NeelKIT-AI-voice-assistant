// Package git provides read-mostly git tools.
//
// Every command runs through the sandbox, which allowlists the git binary and
// refuses the destructive subcommands. Nothing here can rewrite history or
// touch a remote.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

func init() {
	tools.MustRegister(&status{})
	tools.MustRegister(&currentBranch{})
	tools.MustRegister(&diff{})
	tools.MustRegister(&logTool{})
	tools.MustRegister(&branchCreate{})
	tools.MustRegister(&commitMessage{})
}

const emptySchema = `{"type":"object","properties":{},"additionalProperties":false}`

// --------------------------------------------------------------- git.status

type status struct{}

func (status) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "git.status",
		Title:       "Git status",
		Description: "Show which files in the workspace are modified, staged or untracked.",
		Schema:      json.RawMessage(emptySchema),
		Risk:        tools.RiskRead,
		Idempotent:  true,
	}
}

func (status) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	res, err := in.Sandbox.Run(ctx, "git", "status", "--porcelain=v1", "--branch")
	if err != nil {
		return tools.Result{}, err
	}
	if res.ExitCode != 0 {
		return tools.Result{}, fmt.Errorf("git status failed: %s", firstLine(res.Stderr))
	}

	var branch string
	var changed, untracked []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "## "):
			branch = strings.SplitN(strings.TrimPrefix(line, "## "), "...", 2)[0]
		case strings.HasPrefix(line, "?? "):
			untracked = append(untracked, strings.TrimPrefix(line, "?? "))
		default:
			if len(line) > 3 {
				changed = append(changed, line[3:])
			}
		}
	}

	summary := fmt.Sprintf("On %s: %d changed, %d untracked.", branch, len(changed), len(untracked))
	if len(changed) == 0 && len(untracked) == 0 {
		summary = fmt.Sprintf("On %s: working tree clean.", branch)
	}
	return tools.Result{
		Summary: summary,
		Data:    map[string]any{"branch": branch, "changed": changed, "untracked": untracked},
		Stdout:  res.Stdout,
	}, nil
}

// ------------------------------------------------------- git.branch_current

type currentBranch struct{}

func (currentBranch) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "git.branch_current",
		Title:       "Current branch",
		Description: "Report which git branch the workspace is on.",
		Schema:      json.RawMessage(emptySchema),
		Risk:        tools.RiskRead,
		Idempotent:  true,
	}
}

func (currentBranch) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	res, err := in.Sandbox.Run(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return tools.Result{}, err
	}
	if res.ExitCode != 0 {
		return tools.Result{}, fmt.Errorf("not a git repository: %s", firstLine(res.Stderr))
	}
	b := strings.TrimSpace(res.Stdout)
	return tools.Result{Summary: "You are on " + b + ".", Data: map[string]any{"branch": b}}, nil
}

// ----------------------------------------------------------------- git.diff

type diff struct{}

func (diff) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "git.diff",
		Title:       "Git diff",
		Description: "Show the uncommitted changes in the workspace as a unified diff.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "staged": {"type": "boolean", "description": "Show staged changes instead of unstaged"},
    "stat": {"type": "boolean", "description": "Summarise per file instead of full diff"}
  },
  "additionalProperties": false
}`),
		Risk:       tools.RiskRead,
		Idempotent: true,
	}
}

func (diff) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Staged bool `json:"staged"`
		Stat   bool `json:"stat"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	args := []string{"diff"}
	if a.Staged {
		args = append(args, "--staged")
	}
	if a.Stat {
		args = append(args, "--stat")
	}
	res, err := in.Sandbox.Run(ctx, "git", args...)
	if err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return tools.Result{Summary: "No changes.", Data: map[string]any{"diff": ""}}, nil
	}
	files := strings.Count(res.Stdout, "\ndiff --git ") + 1
	return tools.Result{
		Summary: fmt.Sprintf("%d file(s) changed.", files),
		Data:    map[string]any{"diff": res.Stdout, "files": files},
		Stdout:  res.Stdout,
	}, nil
}

// ------------------------------------------------------------------ git.log

type logTool struct{}

func (logTool) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "git.log",
		Title:       "Git log",
		Description: "List recent commits with their author and subject.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {"limit": {"type": "integer", "minimum": 1, "maximum": 50}},
  "additionalProperties": false
}`),
		Risk:       tools.RiskRead,
		Idempotent: true,
	}
}

func (logTool) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	if a.Limit <= 0 || a.Limit > 50 {
		a.Limit = 10
	}
	res, err := in.Sandbox.Run(ctx, "git", "log",
		fmt.Sprintf("-%d", a.Limit), "--pretty=format:%h\x1f%an\x1f%ar\x1f%s")
	if err != nil {
		return tools.Result{}, err
	}

	type commit struct {
		SHA, Author, When, Subject string
	}
	var commits []commit
	for _, line := range strings.Split(res.Stdout, "\n") {
		p := strings.Split(line, "\x1f")
		if len(p) == 4 {
			commits = append(commits, commit{p[0], p[1], p[2], p[3]})
		}
	}
	summary := fmt.Sprintf("%d recent commits.", len(commits))
	if len(commits) > 0 {
		summary = fmt.Sprintf("Latest: %s (%s).", commits[0].Subject, commits[0].When)
	}
	return tools.Result{Summary: summary, Data: map[string]any{"commits": commits}}, nil
}

// -------------------------------------------------------- git.branch_create

type branchCreate struct{}

func (branchCreate) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "git.branch_create",
		Title: "Create branch",
		Description: "Create a new git branch and switch to it. " +
			"The name must already be a valid git ref: lowercase, no spaces.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "pattern": "^[A-Za-z0-9._/-]+$", "description": "Branch name, e.g. feature-auth"}
  },
  "required": ["name"],
  "additionalProperties": false
}`),
		Risk: tools.RiskWrite,
		Examples: []tools.Example{
			{Utterance: "create a branch called feature auth", Args: json.RawMessage(`{"name":"feature-auth"}`)},
		},
	}
}

func (branchCreate) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	if err := validateRef(a.Name); err != nil {
		return tools.Result{}, err
	}
	res, err := in.Sandbox.Run(ctx, "git", "switch", "-c", a.Name)
	if err != nil {
		return tools.Result{}, err
	}
	if res.ExitCode != 0 {
		return tools.Result{}, fmt.Errorf("could not create branch %q: %s", a.Name, firstLine(res.Stderr))
	}
	return tools.Result{
		Summary: "Created and switched to " + a.Name + ".",
		Data:    map[string]any{"branch": a.Name},
	}, nil
}

// validateRef rejects names git would refuse or that would be interpreted as
// flags. The sandbox blocks the dangerous subcommands; this blocks a name that
// turns a safe subcommand into something else.
func validateRef(name string) error {
	if name == "" {
		return fmt.Errorf("%w: branch name is empty", tools.ErrInvalidArgs)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: branch name may not start with '-'", tools.ErrInvalidArgs)
	}
	if len(name) > 200 {
		return fmt.Errorf("%w: branch name is too long", tools.ErrInvalidArgs)
	}
	for _, bad := range []string{"..", "~", "^", ":", "?", "*", "[", "\\", " ", "@{"} {
		if strings.Contains(name, bad) {
			return fmt.Errorf("%w: branch name may not contain %q", tools.ErrInvalidArgs, bad)
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: branch name contains a control character", tools.ErrInvalidArgs)
		}
	}
	return nil
}

// ------------------------------------------------------- git.commit_message

type commitMessage struct{}

func (commitMessage) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "git.commit_message",
		Title: "Draft commit message",
		Description: "Read the staged and unstaged diff so a commit message can be written from it. " +
			"This only gathers the changes; it does not commit anything.",
		Schema:     json.RawMessage(emptySchema),
		Risk:       tools.RiskRead,
		Idempotent: true,
	}
}

func (commitMessage) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	staged, err := in.Sandbox.Run(ctx, "git", "diff", "--staged", "--stat")
	if err != nil {
		return tools.Result{}, err
	}
	unstaged, err := in.Sandbox.Run(ctx, "git", "diff", "--stat")
	if err != nil {
		return tools.Result{}, err
	}
	body, err := in.Sandbox.Run(ctx, "git", "diff", "--staged", "--unified=1")
	if err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(staged.Stdout)+strings.TrimSpace(unstaged.Stdout) == "" {
		return tools.Result{Summary: "Nothing to commit; there are no changes."}, nil
	}
	return tools.Result{
		Summary: "Collected the diff for a commit message.",
		Data: map[string]any{
			"staged_stat":   staged.Stdout,
			"unstaged_stat": unstaged.Stdout,
			"staged_diff":   body.Stdout,
		},
	}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s = strings.TrimSpace(s); s == "" {
		return "no output"
	}
	return s
}
