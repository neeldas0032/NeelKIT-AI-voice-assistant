// Package shell runs named project tasks.
//
// Voice never supplies an argv. It names a task, and the task list is
// configured by the user before anything is spoken — so the set of commands a
// spoken instruction can reach was fixed, in writing, in advance. That is a far
// stronger guarantee than filtering an argv a speech recogniser produced.
package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/shellpreview"
)

func init() { tools.MustRegister(&task{}) }

type task struct{}

func (task) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "shell.task",
		Title: "Run project task",
		Description: "Run one of the project's configured tasks, such as test, build or lint. " +
			"Only tasks defined in the workspace configuration can be run; arbitrary commands cannot.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "Task name, e.g. test, build, lint"}
  },
  "required": ["task"],
  "additionalProperties": false
}`),
		Risk: tools.RiskExecute,
		Examples: []tools.Example{
			{Utterance: "run the tests", Args: json.RawMessage(`{"task":"test"}`)},
		},
	}
}

func (task) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Task string `json:"task"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	a.Task = strings.ToLower(strings.TrimSpace(a.Task))

	tasks := Tasks(in.Config)
	cmdline, ok := shellpreview.Resolve(in.Config, a.Task)
	if !ok {
		names := make([]string, 0, len(tasks))
		for k := range tasks {
			names = append(names, k)
		}
		sort.Strings(names)
		return tools.Result{}, fmt.Errorf("%w: no task %q; configured tasks are: %s",
			tools.ErrInvalidArgs, a.Task, strings.Join(names, ", "))
	}

	// The configured command is split on whitespace into an explicit argv. It
	// is never handed to a shell, so a task definition cannot smuggle in a
	// pipeline or a substitution — the sandbox rejects those characters too.
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return tools.Result{}, fmt.Errorf("%w: task %q has an empty command", tools.ErrInvalidArgs, a.Task)
	}

	res, err := in.Sandbox.Run(ctx, parts[0], parts[1:]...)
	if err != nil {
		return tools.Result{}, err
	}

	out := res.Stdout
	if res.Stderr != "" {
		out = strings.TrimRight(out, "\n") + "\n" + res.Stderr
	}
	summary := fmt.Sprintf("%s passed.", a.Task)
	if res.ExitCode != 0 {
		summary = fmt.Sprintf("%s failed with exit code %d.", a.Task, res.ExitCode)
	}
	return tools.Result{
		Summary:  summary,
		Stdout:   out,
		ExitCode: res.ExitCode,
		Data: map[string]any{
			"task": a.Task, "command": cmdline,
			"exit_code": res.ExitCode, "duration_ms": res.Duration.Milliseconds(),
		},
	}, nil
}

// Tasks merges the workspace configuration over the defaults.
//
// The table lives in shellpreview so that this tool and the runner's approval
// preview resolve a task name through exactly the same code. Two copies of a
// task table is two commands that can disagree, which would mean approving one
// thing and running another.
func Tasks(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(shellpreview.DefaultTasks)+len(cfg))
	for k, v := range shellpreview.DefaultTasks {
		out[k] = v
	}
	for k, v := range cfg {
		if name, ok := strings.CutPrefix(k, "task."); ok && v != "" {
			out[strings.ToLower(name)] = v
		}
	}
	return out
}
