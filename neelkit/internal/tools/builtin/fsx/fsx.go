// Package fsx provides filesystem tools.
//
// The package is named fsx rather than fs because `fs` is a standard library
// package name and shadowing it inside tool code that also needs io/fs is a
// readability trap.
package fsx

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

func init() {
	tools.MustRegister(&readFile{})
	tools.MustRegister(&writeFile{})
	tools.MustRegister(&listDir{})
}

// ---------------------------------------------------------------- fs.read

type readFile struct{}

func (readFile) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "fs.read",
		Title: "Read file",
		Description: "Read a text file from the workspace and return its contents. " +
			"Use this before answering questions about code the user is looking at.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path, e.g. src/main.go"},
    "max_lines": {"type": "integer", "description": "Return at most this many lines", "minimum": 1}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:       tools.RiskRead,
		Idempotent: true,
		Examples: []tools.Example{
			{Utterance: "open the file src/main.go", Args: json.RawMessage(`{"path":"src/main.go"}`)},
		},
	}
}

func (readFile) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Path     string `json:"path"`
		MaxLines int    `json:"max_lines"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	data, err := in.Sandbox.ReadFile(a.Path)
	if err != nil {
		return tools.Result{}, err
	}
	content := string(data)
	lines := strings.Count(content, "\n") + 1
	truncated := false
	if a.MaxLines > 0 {
		parts := strings.SplitN(content, "\n", a.MaxLines+1)
		if len(parts) > a.MaxLines {
			content = strings.Join(parts[:a.MaxLines], "\n")
			truncated = true
		}
	}
	return tools.Result{
		Summary: fmt.Sprintf("Read %s (%d lines).", a.Path, lines),
		Data: map[string]any{
			"path": a.Path, "content": content,
			"lines": lines, "truncated": truncated,
		},
	}, nil
}

// --------------------------------------------------------------- fs.write

type writeFile struct{}

func (writeFile) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "fs.write",
		Title: "Write file",
		Description: "Create or overwrite a text file in the workspace. " +
			"Overwrites without warning, so the user is always asked to confirm first.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path"},
    "content": {"type": "string", "description": "Full file contents"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
		Risk: tools.RiskWrite,
	}
}

func (writeFile) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	existed := false
	if _, err := in.Sandbox.Stat(a.Path); err == nil {
		existed = true
	}
	if err := in.Sandbox.WriteFile(a.Path, []byte(a.Content)); err != nil {
		return tools.Result{}, err
	}
	verb := "Created"
	if existed {
		verb = "Overwrote"
	}
	return tools.Result{
		Summary: fmt.Sprintf("%s %s (%d bytes).", verb, a.Path, len(a.Content)),
		Data:    map[string]any{"path": a.Path, "bytes": len(a.Content), "overwrote": existed},
	}, nil
}

// ---------------------------------------------------------------- fs.list

type listDir struct{}

func (listDir) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "fs.list",
		Title:       "List directory",
		Description: "List the files and directories at a workspace path.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path; \".\" for the root"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Risk:       tools.RiskRead,
		Idempotent: true,
	}
}

// noisyDirs are skipped in listings. They are large, uninteresting, and reading
// them aloud is useless.
var noisyDirs = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "__pycache__": true, ".venv": true,
}

func (listDir) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	if a.Path == "" {
		a.Path = "."
	}
	entries, err := in.Sandbox.ReadDir(a.Path)
	if err != nil {
		return tools.Result{}, err
	}

	type item struct {
		Name  string `json:"name"`
		Dir   bool   `json:"dir"`
		Size  int64  `json:"size"`
		Notes string `json:"notes,omitempty"`
	}
	var out []item
	skipped := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && e.Name() != ".github" {
			skipped++
			continue
		}
		if e.IsDir() && noisyDirs[e.Name()] {
			skipped++
			continue
		}
		it := item{Name: e.Name(), Dir: e.IsDir()}
		if info, err := e.Info(); err == nil && !e.IsDir() {
			it.Size = info.Size()
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir // directories first, as every file listing does
		}
		return out[i].Name < out[j].Name
	})

	// This string is read aloud, so it has to survive being spoken. "." is a
	// real path but a poor thing to say, and naive formatting produced both
	// "2 entries in .." and "1 entries".
	where := path.Clean(a.Path)
	if where == "." {
		where = "the project root"
	}
	noun := "entries"
	if len(out) == 1 {
		noun = "entry"
	}
	summary := fmt.Sprintf("%d %s in %s", len(out), noun, where)
	if skipped > 0 {
		summary += fmt.Sprintf(" (%d hidden or generated skipped)", skipped)
	}
	return tools.Result{Summary: summary + ".", Data: map[string]any{"path": a.Path, "entries": out}}, nil
}
