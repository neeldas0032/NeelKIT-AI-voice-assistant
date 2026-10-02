package grammar

import (
	"strings"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/normalize"
)

// Match is a successful grammar match.
type Match struct {
	// Intent is the rule name, e.g. "git.branch_create".
	Intent string
	// Tool is the tool to invoke.
	Tool string
	// Args are the tool arguments built from the captured slots.
	Args map[string]any
	// Slots are the raw captures, kept for display and debugging.
	Slots map[string]string
	// Pattern is the template that matched, so a surprising match can be
	// traced to a specific rule instead of "the grammar did something".
	Pattern string
}

// Rule is one command family: several phrasings of the same intent, plus the
// function that turns captured slots into tool arguments.
type Rule struct {
	Name      string
	Tool      string
	Templates []string
	// Build converts slots to arguments. Returning false rejects the match, so
	// a pattern can accept the shape of an utterance and still decline it once
	// the captured text turns out to be unusable.
	Build func(slots map[string]string) (map[string]any, bool)

	patterns []*Pattern
}

// Matcher holds the compiled rule set.
type Matcher struct {
	rules []*Rule
}

// NewMatcher compiles the built-in rules. It panics on a malformed template:
// these are compile-time constants, and a broken one must stop startup rather
// than silently never matching.
func NewMatcher() *Matcher {
	m := &Matcher{}
	for _, r := range builtinRules() {
		for _, t := range r.Templates {
			r.patterns = append(r.patterns, MustCompile(t))
		}
		m.rules = append(m.rules, r)
	}
	return m
}

// Rules exposes the compiled rules, for documentation and the /v1/tools UI.
func (m *Matcher) Rules() []*Rule { return m.rules }

// Match returns the first rule that accepts the utterance.
//
// Rules are tried in declaration order, so a more specific rule must be
// declared before a more general one. That ordering is asserted by tests
// rather than left as a comment.
func (m *Matcher) Match(text string) (Match, bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.TrimRight(text, ".!?")
	if text == "" {
		return Match{}, false
	}
	for _, r := range m.rules {
		for _, p := range r.patterns {
			slots, ok := p.Match(text)
			if !ok {
				continue
			}
			args := map[string]any{}
			if r.Build != nil {
				built, accept := r.Build(slots)
				if !accept {
					continue
				}
				args = built
			}
			return Match{
				Intent:  r.Name,
				Tool:    r.Tool,
				Args:    args,
				Slots:   slots,
				Pattern: p.Source(),
			}, true
		}
	}
	return Match{}, false
}

// identifierArg builds a single-argument map whose value is an identifier,
// applying the aggressive identifier normalisation that a known name span
// deserves.
func identifierArg(key, slot string) func(map[string]string) (map[string]any, bool) {
	return func(s map[string]string) (map[string]any, bool) {
		v := normalize.Identifier(s[slot])
		if v == "" {
			return nil, false
		}
		return map[string]any{key: v}, true
	}
}

// pathArg builds a single-argument map whose value is a file path. Paths keep
// their structure so the policy gate can refuse a traversal for the right
// reason instead of the normaliser quietly rewriting it away.
func pathArg(key, slot string) func(map[string]string) (map[string]any, bool) {
	return func(s map[string]string) (map[string]any, bool) {
		v := normalize.Path(s[slot])
		if v == "" {
			return nil, false
		}
		return map[string]any{key: v}, true
	}
}

// textArg builds a single-argument map with the raw captured text.
func textArg(key, slot string) func(map[string]string) (map[string]any, bool) {
	return func(s map[string]string) (map[string]any, bool) {
		v := strings.TrimSpace(s[slot])
		if v == "" {
			return nil, false
		}
		return map[string]any{key: v}, true
	}
}

func noArgs(map[string]string) (map[string]any, bool) { return map[string]any{}, true }

// builtinRules is the command set. Each entry earns its place by being
// something said often enough that a round trip to a language model for it is
// waste.
func builtinRules() []*Rule {
	return []*Rule{
		// --- git ---------------------------------------------------------
		{
			Name: "git.branch_create",
			Tool: "git.branch_create",
			Templates: []string{
				"create [a|an|the] [new] [git] branch [called|named] {name}",
				"make [a|an|the] [new] [git] branch [called|named] {name}",
				"new [git] branch [called|named] {name}",
				"(checkout|switch) [to] [a] new branch [called|named] {name}",
			},
			Build: identifierArg("name", "name"),
		},
		{
			Name:      "git.status",
			Tool:      "git.status",
			Templates: []string{"[what is|whats|show] [me] [the] [git] status", "[show] [me] what changed"},
			Build:     noArgs,
		},
		{
			Name:      "git.branch_current",
			Tool:      "git.branch_current",
			Templates: []string{"(what|which) branch am i on", "[what is|whats] [the] current branch"},
			Build:     noArgs,
		},
		{
			Name:      "git.diff",
			Tool:      "git.diff",
			Templates: []string{"show [me] [the] (diff|changes|differences)", "what have i changed"},
			Build:     noArgs,
		},
		{
			Name:      "git.log",
			Tool:      "git.log",
			Templates: []string{"show [me] [the] [recent|last] (commits|log|history)", "what are [the] [recent|last] commits"},
			Build:     noArgs,
		},
		{
			Name: "git.commit_message",
			Tool: "git.commit_message",
			Templates: []string{
				"write [me] [a] commit message",
				"write [a] commit message (from|for) [these|the|my] changes",
				"generate [a] commit message",
			},
			Build: noArgs,
		},

		// --- shell -------------------------------------------------------
		// Spoken commands name a task, never an argv. The task list is
		// configured by the user, so voice can only run commands that were
		// deliberately allowed beforehand.
		{
			Name:      "shell.task",
			Tool:      "shell.task",
			Templates: []string{"run [the] (tests|test suite|test)", "(test|run tests)"},
			Build:     func(map[string]string) (map[string]any, bool) { return map[string]any{"task": "test"}, true },
		},
		{
			Name:      "shell.task.build",
			Tool:      "shell.task",
			Templates: []string{"(build|compile) [the] [project|app|code]", "run [the] build"},
			Build:     func(map[string]string) (map[string]any, bool) { return map[string]any{"task": "build"}, true },
		},
		{
			Name:      "shell.task.lint",
			Tool:      "shell.task",
			Templates: []string{"(lint|vet) [the] [project|app|code]", "run [the] (linter|lint)"},
			Build:     func(map[string]string) (map[string]any, bool) { return map[string]any{"task": "lint"}, true },
		},
		{
			Name:      "shell.task.named",
			Tool:      "shell.task",
			Templates: []string{"run [the] task {task}", "run {task} task"},
			Build:     identifierArg("task", "task"),
		},

		// --- search and files --------------------------------------------
		{
			Name: "search.code",
			Tool: "search.code",
			Templates: []string{
				"search [my|the] (project|code|codebase|repo) for {query}",
				"find {query} in [my|the] (project|code|codebase|repo)",
				"(grep|search) for {query}",
				"where is {query} (defined|used)",
			},
			Build: textArg("query", "query"),
		},
		{
			Name: "fs.read",
			Tool: "fs.read",
			Templates: []string{
				"(open|read|show) [me] [the] file {path}",
				"(open|read) {path}",
				"what is in [the] [file] {path}",
			},
			Build: pathArg("path", "path"),
		},
		{
			Name: "fs.list",
			Tool: "fs.list",
			Templates: []string{
				"list [the] files [in] {path}",
				"(list|show) [me] [the] (directory|folder) {path}",
				"what files are in {path}",
			},
			Build: pathArg("path", "path"),
		},
		{
			Name:      "fs.list.root",
			Tool:      "fs.list",
			Templates: []string{"list [the] files", "what files are (here|in this project)"},
			Build:     func(map[string]string) (map[string]any, bool) { return map[string]any{"path": "."}, true },
		},

		// --- notes -------------------------------------------------------
		{
			Name: "notes.create",
			Tool: "notes.create",
			Templates: []string{
				"(make|add|create) [a] (note|todo|to do) {text}",
				"(note|remember) that {text}",
				"add {text} to [my] (notes|todo|todos)",
			},
			Build: textArg("text", "text"),
		},
		{
			Name:      "notes.list",
			Tool:      "notes.list",
			Templates: []string{"(show|list|read) [me] [my] (notes|todos|todo list)", "what are my (notes|todos)"},
			Build:     noArgs,
		},
	}
}
