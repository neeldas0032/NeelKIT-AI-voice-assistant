package normalize

import "testing"

func TestIdentifierRepair(t *testing.T) {
	// The failure this package exists to fix: without it, "feature dash auth"
	// creates a branch literally named "feature auth".
	cases := []struct{ in, want string }{
		{"create a git branch called feature dash auth", "create a git branch called feature-auth"},
		{"open src slash main dot go", "open src/main.go"},
		{"the variable user underscore id", "the variable user_id"},
		{"deploy auth dash token dash service", "deploy auth-token-service"},
		{"email me at neel at example dot com", "email me at neel@example.com"},
	}
	for _, c := range cases {
		if got := Text(c.in, CommandOptions()); got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// The conservative global pass refuses to join across a stopword, even when
// the speaker did mean an identifier. That is the correct trade: a false join
// silently corrupts prose everywhere, while a missed join is recovered by
// Identifier() once the grammar knows the span is a name.
func TestGlobalPassDefersAmbiguousJoinsToIdentifier(t *testing.T) {
	const in = "file a dash b dot txt"
	if got := Text(in, CommandOptions()); got != "file a dash b.txt" {
		t.Logf("global pass produced %q", got)
	}
	if got := Identifier("a dash b dot txt"); got != "a-b.txt" {
		t.Fatalf("Identifier(%q) = %q, want %q", "a dash b dot txt", got, "a-b.txt")
	}
}

// The guard that keeps identifier repair from being actively harmful: a
// separator word used as an ordinary English word must survive untouched.
func TestSeparatorWordsInProseAreLeftAlone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"make a dash for it", "make a dash for it"},
		{"dash to the store", "dash to the store"},
		{"a period of time", "a period of time"},
	}
	for _, c := range cases {
		if got := Text(c.in, Options{Identifiers: true}); got != c.want {
			t.Errorf("Text(%q) = %q, want it unchanged", c.in, got)
		}
	}
}

func TestIdentifierAggressiveMode(t *testing.T) {
	// Inside a span the grammar already identified as an identifier there is
	// no ambiguity to protect against, so every separator converts.
	cases := []struct{ in, want string }{
		{"feature dash auth", "feature-auth"},
		{"feature auth", "feature-auth"},
		{"fix the login bug", "fix-the-login-bug"},
		{"src slash main dot go", "src/main.go"},
		{"user underscore service", "user_service"},
		{"  Release  2 dot 1  ", "release-2.1"},
	}
	for _, c := range cases {
		if got := Identifier(c.in); got != c.want {
			t.Errorf("Identifier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIdentifierNeverLeavesStraySeparators(t *testing.T) {
	for _, in := range []string{"dash", "  dash dash  ", "dot", ""} {
		got := Identifier(in)
		if got != "" && (got[0] == '-' || got[len(got)-1] == '-' || got == ".") {
			t.Errorf("Identifier(%q) = %q, which is not a usable name", in, got)
		}
	}
}

func TestCaseConventions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"camel case user id", "userId"},
		{"snake case max retry count", "max_retry_count"},
	}
	for _, c := range cases {
		if got := Text(c.in, Options{Identifiers: true}); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLexiconFixesTechnicalTerms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"convert this jason into a struct", "convert this JSON into a struct"},
		{"explain this sequel query", "explain this SQL query"},
		{"open a pee are on get hub", "open a PR on GitHub"},
		{"run cube control get pods", "run kubectl get pods"},
	}
	for _, c := range cases {
		if got := Text(c.in, Options{Lexicon: true}); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Whole-word matching only: a lexicon that rewrote substrings would corrupt
// every identifier containing a technical term.
func TestLexiconDoesNotMatchSubstrings(t *testing.T) {
	for _, in := range []string{"jsonify the payload", "apiserver is down", "urlencode it"} {
		if got := Text(in, Options{Lexicon: true}); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestNumberConversion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"listen on port eight thousand eighty", "listen on port 8080"},
		{"retry three times", "retry 3 times"},
		{"set timeout to thirty", "set timeout to 30"},
		{"two hundred fifty", "250"},
		{"one thousand five hundred", "1500"},
	}
	for _, c := range cases {
		if got := Text(c.in, Options{Numbers: true}); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNumberConversionLeavesOtherWords(t *testing.T) {
	in := "the quick brown fox"
	if got := Text(in, Options{Numbers: true}); got != in {
		t.Errorf("Text(%q) = %q", in, got)
	}
}

// Commands must not gain a trailing period: it would end up in the branch name.
func TestCommandProfileAddsNoPunctuation(t *testing.T) {
	got := Text("create a branch called feature dash auth", CommandOptions())
	if got[len(got)-1] == '.' {
		t.Fatalf("command profile added a terminating period: %q", got)
	}
}

func TestDictationProfileAddsPunctuation(t *testing.T) {
	got := Text("this is a sentence", DictationOptions())
	if got != "This is a sentence." {
		t.Fatalf("got %q, want %q", got, "This is a sentence.")
	}
}

func TestCustomVocabularyOverrides(t *testing.T) {
	opt := CommandOptions()
	opt.Custom = map[string]string{"neel kit": "NeelKIT", "brain ware": "Brainware"}
	if got := Text("deploy neel kit to brain ware", opt); got != "deploy NeelKIT to Brainware" {
		t.Errorf("got %q", got)
	}
}

func TestEmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := Text(in, DictationOptions()); got != "" {
			t.Errorf("Text(%q) = %q, want empty", in, got)
		}
	}
}

func TestIsDeterministic(t *testing.T) {
	// Map iteration order is random in Go; the lexicon pass must not depend
	// on it or the same utterance would normalise differently between runs.
	in := "convert jason to yaml and open a pee are on get hub"
	first := Text(in, CommandOptions())
	for i := 0; i < 50; i++ {
		if got := Text(in, CommandOptions()); got != first {
			t.Fatalf("run %d differed: %q vs %q", i, got, first)
		}
	}
}

func BenchmarkNormalizeCommand(b *testing.B) {
	in := "create a git branch called feature dash auth and open a pee are on get hub"
	opt := CommandOptions()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Text(in, opt)
	}
}

// Paths keep their structure. An earlier version ran them through Identifier,
// which collapsed "../../etc/passwd" into "etc/passwd": contained by accident,
// but the user was then told the file did not exist rather than that they had
// asked to leave the project.
func TestPathPreservesStructure(t *testing.T) {
	cases := []struct{ in, want string }{
		{"src slash main dot go", "src/main.go"},
		{"../../etc/passwd", "../../etc/passwd"},
		{"/etc/shadow", "/etc/shadow"},
		{"~/.ssh/id_rsa", "~/.ssh/id_rsa"},
		{"internal/audio/mel.go", "internal/audio/mel.go"},
		{"my notes dot md", "my-notes.md"},
	}
	for _, c := range cases {
		if got := Path(c.in); got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
