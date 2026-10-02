package grammar

import "testing"

func TestPatternCompileAndMatch(t *testing.T) {
	p := MustCompile("create [a|the] [git] branch [called|named] {name}")

	positives := map[string]string{
		"create a git branch called feature-auth": "feature-auth",
		"create branch feature-auth":              "feature-auth",
		"create the branch named my thing":        "my thing",
		"CREATE A BRANCH CALLED Upper":            "Upper",
	}
	for in, want := range positives {
		slots, ok := p.Match(in)
		if !ok {
			t.Errorf("%q did not match", in)
			continue
		}
		if slots["name"] != want {
			t.Errorf("%q captured name=%q, want %q", in, slots["name"], want)
		}
	}

	// Negatives matter more than positives: a grammar that matches too much is
	// worse than one that matches too little, because the fallback is correct
	// and a wrong match is not.
	for _, in := range []string{
		"create a branch",             // no name
		"delete a branch called x",    // wrong verb
		"create a git tag called x",   // wrong noun
		"",                            // empty
		"tell me about branch naming", // unrelated prose
	} {
		if _, ok := p.Match(in); ok {
			t.Errorf("%q matched but should not have", in)
		}
	}
}

func TestPatternRequiredAlternation(t *testing.T) {
	p := MustCompile("(open|show) file {path}")
	for _, in := range []string{"open file main.go", "show file main.go"} {
		if _, ok := p.Match(in); !ok {
			t.Errorf("%q did not match", in)
		}
	}
	if _, ok := p.Match("file main.go"); ok {
		t.Error("required alternation was treated as optional")
	}
}

func TestPatternCompileErrors(t *testing.T) {
	for _, bad := range []string{"", "create [unclosed name", "create {} thing", "create {Bad-Name}"} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("Compile(%q) succeeded, want an error", bad)
		}
	}
}

func TestMatcherBranchCreation(t *testing.T) {
	m := NewMatcher()

	// The end-to-end reason this package exists: a spoken branch name has to
	// come out as a usable git ref.
	got, ok := m.Match("create a git branch called feature auth")
	if !ok {
		t.Fatal("no match")
	}
	if got.Tool != "git.branch_create" {
		t.Fatalf("tool = %q", got.Tool)
	}
	if got.Args["name"] != "feature-auth" {
		t.Fatalf("name = %v, want feature-auth", got.Args["name"])
	}
}

func TestMatcherCoversSpokenPhrasings(t *testing.T) {
	m := NewMatcher()
	cases := map[string]string{
		"create a new branch called release 2 dot 1": "git.branch_create",
		"whats the git status":                       "git.status",
		"show me the diff":                           "git.diff",
		"which branch am i on":                       "git.branch_current",
		"write a commit message from these changes":  "git.commit_message",
		"run the tests":                              "shell.task",
		"build the project":                          "shell.task",
		"run the linter":                             "shell.task",
		"search my project for authentication logic": "search.code",
		"where is authentication defined":            "search.code",
		"open the file src/main.go":                  "fs.read",
		"list the files":                             "fs.list",
		"make a note call the dentist":               "notes.create",
		"show me my todos":                           "notes.list",
	}
	for in, wantTool := range cases {
		got, ok := m.Match(in)
		if !ok {
			t.Errorf("%q did not match any rule", in)
			continue
		}
		if got.Tool != wantTool {
			t.Errorf("%q -> tool %q, want %q (rule %s, pattern %q)", in, got.Tool, wantTool, got.Intent, got.Pattern)
		}
	}
}

// Anything conversational must fall through to the language model. A grammar
// that swallows questions turns the assistant into a command line.
func TestMatcherFallsThroughOnProse(t *testing.T) {
	m := NewMatcher()
	for _, in := range []string{
		"why is my build failing",
		"what does this error mean",
		"can you explain how transducers work",
		"convert this json into a typescript interface",
		"summarise the last deploy log",
		"",
	} {
		if got, ok := m.Match(in); ok {
			t.Errorf("%q matched rule %q, but it should fall through to the LLM", in, got.Intent)
		}
	}
}

// Rules are tried in declaration order, so a specific rule must precede a
// general one. This asserts that the ordering actually holds instead of
// trusting a comment.
func TestSpecificRulesPrecedeGeneralOnes(t *testing.T) {
	m := NewMatcher()
	got, ok := m.Match("run the tests")
	if !ok {
		t.Fatal("no match")
	}
	if got.Intent != "shell.task" {
		t.Fatalf("intent = %q, want the specific shell.task rule, not %q", got.Intent, got.Intent)
	}
	if got.Args["task"] != "test" {
		t.Fatalf("task = %v, want test", got.Args["task"])
	}
}

// A pattern may accept the shape of an utterance and still decline it once the
// captured slot turns out to be unusable.
func TestBuildCanRejectAMatch(t *testing.T) {
	m := NewMatcher()
	if got, ok := m.Match("create a branch called dash"); ok && got.Args["name"] == "" {
		t.Fatal("accepted a match with an empty branch name")
	}
}

func TestAllTemplatesCompile(t *testing.T) {
	// NewMatcher panics on a bad template; this makes that an explicit test so
	// the failure names the package rather than crashing an unrelated one.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a built-in template failed to compile: %v", r)
		}
	}()
	m := NewMatcher()
	if len(m.Rules()) == 0 {
		t.Fatal("no rules compiled")
	}
	for _, r := range m.Rules() {
		if r.Tool == "" {
			t.Errorf("rule %q has no tool", r.Name)
		}
		if len(r.Templates) == 0 {
			t.Errorf("rule %q has no templates", r.Name)
		}
	}
}

func BenchmarkMatcherHit(b *testing.B) {
	m := NewMatcher()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Match("create a git branch called feature auth")
	}
}

func BenchmarkMatcherMiss(b *testing.B) {
	m := NewMatcher()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Match("why is the build failing on the release branch")
	}
}
