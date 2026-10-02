// Package notes captures TODOs and notes without leaving the flow of work.
//
// Notes live in a single markdown file inside the workspace rather than in the
// database, so they are greppable, diffable and committable like everything
// else a developer owns.
package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

// File is where notes are appended, relative to the workspace root.
const File = "NOTES.md"

func init() {
	tools.MustRegister(&create{})
	tools.MustRegister(&list{})
}

type create struct{}

func (create) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "notes.create",
		Title:       "Add note",
		Description: "Append a dated note or TODO to " + File + " in the workspace.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "The note text"},
    "todo": {"type": "boolean", "description": "Record it as an unchecked task"}
  },
  "required": ["text"],
  "additionalProperties": false
}`),
		Risk: tools.RiskWrite,
		Examples: []tools.Example{
			{Utterance: "make a note to refactor the auth middleware",
				Args: json.RawMessage(`{"text":"refactor the auth middleware","todo":true}`)},
		},
	}
}

func (create) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Text string `json:"text"`
		Todo bool   `json:"todo"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	a.Text = strings.TrimSpace(a.Text)
	if a.Text == "" {
		return tools.Result{}, fmt.Errorf("%w: empty note", tools.ErrInvalidArgs)
	}

	existing, err := in.Sandbox.ReadFile(File)
	if err != nil && !errors.Is(err, tools.ErrInvalidArgs) {
		return tools.Result{}, err
	}
	body := string(existing)
	if body == "" {
		body = "# Notes\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	marker := "-"
	if a.Todo {
		marker = "- [ ]"
	}
	line := fmt.Sprintf("%s %s  _(%s)_\n", marker, a.Text, time.Now().Format("2006-01-02 15:04"))
	if err := in.Sandbox.WriteFile(File, []byte(body+line)); err != nil {
		return tools.Result{}, err
	}

	kind := "note"
	if a.Todo {
		kind = "TODO"
	}
	return tools.Result{
		Summary: fmt.Sprintf("Added a %s to %s.", kind, File),
		Data:    map[string]any{"file": File, "text": a.Text, "todo": a.Todo},
	}, nil
}

type list struct{}

func (list) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:        "notes.list",
		Title:       "List notes",
		Description: "Read the notes and TODOs recorded in " + File + ".",
		Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        tools.RiskRead,
		Idempotent:  true,
	}
}

func (list) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	data, err := in.Sandbox.ReadFile(File)
	if err != nil {
		return tools.Result{Summary: "You have no notes yet.", Data: map[string]any{"notes": []string{}}}, nil
	}
	var notes []string
	open := 0
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		notes = append(notes, t)
		if strings.HasPrefix(t, "- [ ]") {
			open++
		}
	}
	return tools.Result{
		Summary: fmt.Sprintf("%d notes, %d still open.", len(notes), open),
		Data:    map[string]any{"notes": notes, "open": open},
	}, nil
}
