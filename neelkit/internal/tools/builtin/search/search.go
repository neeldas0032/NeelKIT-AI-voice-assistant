// Package search provides code search over the workspace.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

func init() { tools.MustRegister(&code{}) }

type code struct{}

func (code) Descriptor() tools.Descriptor {
	return tools.Descriptor{
		Name:  "search.code",
		Title: "Search code",
		Description: "Search the workspace for a string or regular expression and return matching " +
			"lines with their file and line number.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Text or regular expression to find"},
    "glob": {"type": "string", "description": "Restrict to files matching this glob, e.g. *.go"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200}
  },
  "required": ["query"],
  "additionalProperties": false
}`),
		Risk:       tools.RiskRead,
		Idempotent: true,
		Examples: []tools.Example{
			{Utterance: "search my project for authentication logic",
				Args: json.RawMessage(`{"query":"auth"}`)},
		},
	}
}

func (code) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
	var a struct {
		Query string `json:"query"`
		Glob  string `json:"glob"`
		Limit int    `json:"limit"`
	}
	if err := tools.Decode(in.Args, &a); err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(a.Query) == "" {
		return tools.Result{}, fmt.Errorf("%w: empty query", tools.ErrInvalidArgs)
	}
	if a.Limit <= 0 || a.Limit > 200 {
		a.Limit = 50
	}

	// ripgrep is preferred; grep is the fallback so the tool works on a machine
	// without it. Both are invoked with an explicit argv and a leading "--" so
	// a query starting with "-" can never become a flag.
	args := []string{
		"--line-number", "--no-heading", "--color=never",
		"--max-count=5", "--max-columns=200",
		"-m", strconv.Itoa(a.Limit), "--smart-case",
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	args = append(args, "--", a.Query)

	res, err := in.Sandbox.Run(ctx, "rg", args...)
	if err != nil {
		res, err = in.Sandbox.Run(ctx, "grep", "-rnI", "--color=never", "--", a.Query, ".")
		if err != nil {
			return tools.Result{}, err
		}
	}

	type hit struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	var hits []hit
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		// rg and grep both emit path:line:text.
		p := strings.SplitN(line, ":", 3)
		if len(p) < 3 {
			continue
		}
		n, err := strconv.Atoi(p[1])
		if err != nil {
			continue
		}
		hits = append(hits, hit{File: strings.TrimPrefix(p[0], "./"), Line: n, Text: strings.TrimSpace(p[2])})
		if len(hits) >= a.Limit {
			break
		}
	}

	files := map[string]bool{}
	for _, h := range hits {
		files[h.File] = true
	}
	summary := fmt.Sprintf("%d matches in %d files for %q.", len(hits), len(files), a.Query)
	if len(hits) == 0 {
		summary = fmt.Sprintf("No matches for %q.", a.Query)
	}
	return tools.Result{Summary: summary, Data: map[string]any{"query": a.Query, "hits": hits}}, nil
}
