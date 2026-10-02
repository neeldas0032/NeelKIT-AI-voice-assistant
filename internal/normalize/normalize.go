// Package normalize turns raw ASR output into text a command parser can use.
//
// This is the stage that decides whether voice commands work at all. Parakeet
// emits lowercase, unpunctuated words: "create a git branch called feature
// auth". Acting on that verbatim creates a branch named "feature auth", which
// is useless. Every rule here exists because it fixes a concrete failure of
// that kind, and each one is a pure function over a string, so each one is
// exhaustively testable.
package normalize

import (
	"regexp"
	"strconv"
	"strings"
)

// Options controls which passes run.
type Options struct {
	// Lexicon fixes spoken forms of technical words ("jason" -> "JSON").
	Lexicon bool
	// Identifiers joins spoken separators ("feature dash auth" -> "feature-auth").
	Identifiers bool
	// Numbers converts spelled-out numbers to digits ("port eight thousand").
	Numbers bool
	// Sentences adds capitalisation and a terminating period. Off for commands:
	// a trailing period breaks a branch name or a file path.
	Sentences bool
	// Custom is a user-supplied replacement table applied after Lexicon, for
	// project-specific vocabulary the model has never seen.
	Custom map[string]string
}

// CommandOptions is the profile for spoken commands: fix vocabulary and
// identifiers, leave sentence punctuation alone.
func CommandOptions() Options {
	return Options{Lexicon: true, Identifiers: true, Numbers: true}
}

// DictationOptions is the profile for prose: everything, including sentence
// punctuation.
func DictationOptions() Options {
	return Options{Lexicon: true, Identifiers: true, Numbers: true, Sentences: true}
}

// lexicon maps what the recogniser hears to what was meant. Keys are matched
// case-insensitively on whole words only, so "jsonify" is never touched.
//
// The entries are not guesses: each is a transcription error observed for a
// term that appears in developer speech constantly, where the wrong form makes
// the whole utterance unparseable.
var lexicon = map[string]string{
	"jason":        "JSON",
	"json":         "JSON",
	"yaml":         "YAML",
	"yml":          "YAML",
	"sequel":       "SQL",
	"sql":          "SQL",
	"my sequel":    "MySQL",
	"post grays":   "Postgres",
	"postgres":     "Postgres",
	"pull request": "pull request",
	"pee are":      "PR",
	"get hub":      "GitHub",
	"github":       "GitHub",
	"get lab":      "GitLab",
	"curl":         "curl",
	"kernel":       "kernel",
	"cube control": "kubectl",
	"cube cuddle":  "kubectl",
	"kubectl":      "kubectl",
	"docker":       "Docker",
	"api":          "API",
	"apis":         "APIs",
	"url":          "URL",
	"urls":         "URLs",
	"uuid":         "UUID",
	"http":         "HTTP",
	"https":        "HTTPS",
	"css":          "CSS",
	"html":         "HTML",
	"npm":          "npm",
	"node":         "Node",
	"golang":       "Go",
	"typescript":   "TypeScript",
	"javascript":   "JavaScript",
	"react":        "React",
	"next js":      "Next.js",
	"dot env":      ".env",
	"read me":      "README",
	"stack trace":  "stack trace",
	"regex":        "regex",
	"cli":          "CLI",
	"ssh":          "SSH",
	"tee ell ess":  "TLS",
	"repo":         "repo",
	"refactor":     "refactor",
}

// separators are spoken punctuation inside identifiers.
//
// "at" is deliberately absent. It is one of the most common words in English
// ("look at this", "meet me at noon"), so a generic rule turns ordinary prose
// into garbage. Email addresses, the case that actually needs it, are handled
// by emailPattern below, which requires the full "<user> at <host> dot <tld>"
// shape before it fires.
var separators = map[string]string{
	"dash":        "-",
	"hyphen":      "-",
	"underscore":  "_",
	"under score": "_",
	"slash":       "/",
	"dot":         ".",
	"period":      ".",
	"colon":       ":",
}

// stopwords are words that never appear inside an identifier. A separator
// surrounded by one of them is being used as an ordinary English word, not as
// punctuation: "make a dash for it" must not become "make a-for it".
//
// This keeps the global pass conservative. Where the caller knows a span is an
// identifier — the grammar layer extracting a branch name after "called" —
// Identifier() applies the aggressive rule with no stopword guard.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "for": true, "it": true, "to": true,
	"of": true, "in": true, "on": true, "and": true, "or": true, "but": true,
	"my": true, "your": true, "our": true, "their": true, "his": true, "her": true,
	"is": true, "was": true, "are": true, "be": true, "this": true, "that": true,
	"make": true, "made": true, "time": true, "out": true, "up": true, "off": true,
	"me": true, "you": true, "we": true, "they": true, "i": true, "at": true,
	"with": true, "from": true, "by": true, "as": true, "into": true, "over": true,
}

var (
	multiSpace  = regexp.MustCompile(`\s{2,}`)
	sentenceEnd = regexp.MustCompile(`([.!?])\s+([a-z])`)
	// Requires the whole address shape, so "meet me at noon" cannot match.
	emailPattern = regexp.MustCompile(
		`(?i)\b([\w.]+)\s+at\s+([\w]+)\s+dot\s+(com|org|net|io|dev|ai|co|edu|gov|in|uk|me)\b`)
)

// Text applies the configured passes.
func Text(s string, opt Options) string {
	s = strings.TrimSpace(multiSpace.ReplaceAllString(s, " "))
	if s == "" {
		return ""
	}
	if opt.Lexicon {
		s = applyLexicon(s, lexicon)
	}
	if len(opt.Custom) > 0 {
		s = applyLexicon(s, opt.Custom)
	}
	if opt.Numbers {
		s = applyNumbers(s)
	}
	if opt.Identifiers {
		s = applyIdentifiers(s)
	}
	if opt.Sentences {
		s = applySentences(s)
	}
	return strings.TrimSpace(multiSpace.ReplaceAllString(s, " "))
}

// applyLexicon replaces whole-word and whole-phrase matches, longest first so
// "my sequel" wins over "sequel".
func applyLexicon(s string, table map[string]string) string {
	type entry struct{ from, to string }
	entries := make([]entry, 0, len(table))
	for k, v := range table {
		entries = append(entries, entry{k, v})
	}
	// Longest phrase first; ties broken alphabetically so the result is
	// deterministic regardless of map iteration order.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			a, b := entries[i], entries[j]
			if len(b.from) > len(a.from) || (len(b.from) == len(a.from) && b.from < a.from) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	for _, e := range entries {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(e.from) + `\b`)
		s = re.ReplaceAllString(s, e.to)
	}
	return s
}

// applyIdentifiers joins spoken separators into the symbol they name.
//
// The guard is what makes this safe: a separator word is only converted when it
// sits between two word characters. "feature dash auth" becomes "feature-auth",
// while "make a dash for it" is untouched because "a" and "for" surround it
// with spaces on both sides of the match, not word characters.
func applyIdentifiers(s string) string {
	// Email addresses first: the pattern is specific enough to be safe, and
	// running it before the generic "dot" rule keeps the TLD intact.
	s = emailPattern.ReplaceAllString(s, "$1@$2.$3")

	for word, symbol := range separators {
		re := regexp.MustCompile(`(?i)\b(\w+)\s+` + regexp.QuoteMeta(word) + `\s+(\w+)\b`)
		for {
			next := re.ReplaceAllStringFunc(s, func(m string) string {
				sub := re.FindStringSubmatch(m)
				if len(sub) < 3 {
					return m
				}
				if stopwords[strings.ToLower(sub[1])] || stopwords[strings.ToLower(sub[2])] {
					return m // ordinary English, not an identifier
				}
				return sub[1] + symbol + sub[2]
			})
			// Repeat until stable: "a dash b dash c" needs two passes, because
			// the first match consumes the token the second one needs.
			if next == s {
				break
			}
			s = next
		}
	}

	// "camel case user id" -> "userId"; a spoken convention that is otherwise
	// impossible to express.
	s = joinCase(s, "camel case", func(words []string) string {
		var b strings.Builder
		for i, w := range words {
			if i == 0 {
				b.WriteString(strings.ToLower(w))
				continue
			}
			b.WriteString(strings.ToUpper(w[:1]) + strings.ToLower(w[1:]))
		}
		return b.String()
	})
	s = joinCase(s, "snake case", func(words []string) string {
		for i := range words {
			words[i] = strings.ToLower(words[i])
		}
		return strings.Join(words, "_")
	})
	return s
}

// Path normalises a spoken file path.
//
// It converts spoken separators ("src slash main dot go") and collapses the
// whitespace a recogniser leaves behind, but it never rewrites the structure of
// the path. That distinction matters: an earlier version ran paths through
// Identifier, which collapsed "../../etc/passwd" into "etc/passwd" — safe by
// accident, and deeply confusing, because the user was told the file did not
// exist rather than that they had asked to leave the project. Preserving the
// path lets the policy gate see what was actually requested and refuse it for
// the real reason.
func Path(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for word, symbol := range separators {
		re := regexp.MustCompile(`(?i)\s*\b` + regexp.QuoteMeta(word) + `\b\s*`)
		s = re.ReplaceAllString(s, symbol)
	}
	s = multiSpace.ReplaceAllString(s, " ")
	// Remaining spaces inside a spoken path are word boundaries in a file name.
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "-")
}

// Identifier normalises a span the caller already knows is an identifier, such
// as a branch name extracted from "...called <span>".
//
// Inside a known identifier there is no ambiguity left to protect against, so
// the stopword guard is dropped and every separator word is converted. This is
// the aggressive counterpart to the conservative global pass.
func Identifier(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	for word, symbol := range separators {
		re := regexp.MustCompile(`(?i)\s*\b` + regexp.QuoteMeta(word) + `\b\s*`)
		s = re.ReplaceAllString(s, symbol)
	}
	// Anything still separated by whitespace joins with a hyphen: spoken
	// identifiers are overwhelmingly kebab-case in git and file names.
	s = multiSpace.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "-")
	// Collapse runs a double separator could have produced.
	for _, sym := range []string{"--", "__", "//", ".."} {
		for strings.Contains(s, sym) {
			s = strings.ReplaceAll(s, sym, sym[:1])
		}
	}
	return strings.Trim(s, "-_./:")
}

// joinCase finds "<marker> w1 w2 ..." and joins the following words with join.
// It consumes words until punctuation or the end of the string.
func joinCase(s, marker string, join func([]string) string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(marker) + `\s+((?:[A-Za-z]+\s*){1,5})`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		words := strings.Fields(sub[1])
		if len(words) == 0 {
			return m
		}
		return join(words)
	})
}

var smallNumbers = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

var multipliers = map[string]int{"hundred": 100, "thousand": 1000, "million": 1000000}

// applyNumbers converts runs of spoken number words into digits.
//
// "port eight thousand eighty" has to become "port 8080" or no command
// involving a port can ever work.
func applyNumbers(s string) string {
	words := strings.Fields(s)
	var out []string

	for i := 0; i < len(words); {
		clean := strings.ToLower(strings.Trim(words[i], ".,!?;:"))
		_, isSmall := smallNumbers[clean]
		if !isSmall {
			out = append(out, words[i])
			i++
			continue
		}

		// Consume the longest run of number words starting here.
		total, current := 0, 0
		j := i
		for j < len(words) {
			w := strings.ToLower(strings.Trim(words[j], ".,!?;:"))
			if v, ok := smallNumbers[w]; ok {
				current += v
				j++
				continue
			}
			if m, ok := multipliers[w]; ok && current > 0 {
				if m >= 1000 {
					total += current * m
					current = 0
				} else {
					current *= m
				}
				j++
				continue
			}
			break
		}
		total += current
		// Preserve any trailing punctuation the run carried.
		suffix := ""
		if last := words[j-1]; len(last) > 0 {
			if t := strings.TrimLeft(last, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"); t != "" {
				suffix = t
			}
		}
		out = append(out, strconv.Itoa(total)+suffix)
		i = j
	}
	return strings.Join(out, " ")
}

// applySentences capitalises sentence starts and the pronoun "I", and adds a
// terminating period when the text has none.
func applySentences(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	s = sentenceEnd.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(m)
	})
	s = regexp.MustCompile(`\bi\b`).ReplaceAllString(s, "I")
	if last := s[len(s)-1]; last != '.' && last != '!' && last != '?' {
		s += "."
	}
	return s
}
