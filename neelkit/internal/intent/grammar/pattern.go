// Package grammar matches spoken commands deterministically.
//
// The everyday commands a developer gives by voice are a small, closed set.
// Sending them to a language model costs hundreds of milliseconds, burns
// tokens, and makes the result non-deterministic for input that was never
// ambiguous. Matching them with compiled patterns instead makes them instant,
// free, reproducible and unit-testable; anything the grammar misses still
// falls through to the model.
package grammar

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a compiled command template.
//
// The template syntax is deliberately tiny, because it has to be readable by
// whoever adds a rule six months from now:
//
//	word            a literal word
//	[a|the]         optional; any one alternative, or nothing
//	(open|show)     required; exactly one alternative
//	{name}          a named slot capturing one or more words
//
// Alternatives are separated by "|" in both group kinds, and an alternative may
// contain spaces ("[what is|whats]"). Example:
//
//	create [a|the] [git] branch [called|named] {name}
type Pattern struct {
	source string
	re     *regexp.Regexp
	slots  []string
}

// Source returns the original template, for error messages and docs.
func (p *Pattern) Source() string { return p.source }

// Slots returns the slot names this pattern captures.
func (p *Pattern) Slots() []string { return p.slots }

// piece is one compiled element of a template, without any separator. The
// separator is attached at assembly time, which is what lets an optional piece
// disappear without leaving a dangling "\s+" that nothing can match.
type piece struct {
	body     string
	optional bool
	slot     string
}

// Compile turns a template into a matcher.
func Compile(template string) (*Pattern, error) {
	tokens := strings.Fields(template)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("grammar: empty pattern")
	}

	var (
		pieces []piece
		slots  []string
	)

	for i := 0; i < len(tokens); {
		tok := tokens[i]
		switch {
		case strings.HasPrefix(tok, "["):
			alts, next, err := collect(tokens, i, '[', ']')
			if err != nil {
				return nil, err
			}
			pieces = append(pieces, piece{body: alternation(alts), optional: true})
			i = next

		case strings.HasPrefix(tok, "("):
			alts, next, err := collect(tokens, i, '(', ')')
			if err != nil {
				return nil, err
			}
			pieces = append(pieces, piece{body: alternation(alts)})
			i = next

		case strings.HasPrefix(tok, "{") && strings.HasSuffix(tok, "}"):
			name := tok[1 : len(tok)-1]
			if !validSlot.MatchString(name) {
				return nil, fmt.Errorf("grammar: invalid slot name %q in %q", name, template)
			}
			slots = append(slots, name)
			pieces = append(pieces, piece{body: `(?P<` + name + `>.+?)`, slot: name})
			i++

		default:
			if strings.ContainsAny(tok, "[](){}") {
				return nil, fmt.Errorf("grammar: stray bracket in token %q of %q", tok, template)
			}
			pieces = append(pieces, piece{body: regexp.QuoteMeta(tok)})
			i++
		}
	}

	// Separator placement is the whole difficulty of this compiler. An
	// optional piece must be able to vanish without leaving a mandatory "\s+"
	// that nothing can satisfy, and a template that opens with optionals must
	// still match when every one of them is absent.
	//
	// The rule: a piece owns the separator that PRECEDES it, unless nothing
	// mandatory has appeared yet — in which case it owns the separator that
	// FOLLOWS it, so the first surviving piece never demands a leading space.
	var b strings.Builder
	b.WriteString(`(?i)^\s*`)
	seenMandatory := false
	for _, p := range pieces {
		switch {
		case p.optional && !seenMandatory:
			b.WriteString(`(?:` + p.body + `\s+)?`)
		case p.optional:
			b.WriteString(`(?:\s+` + p.body + `)?`)
		case seenMandatory:
			b.WriteString(`\s+` + p.body)
		default:
			b.WriteString(p.body)
			seenMandatory = true
		}
	}
	b.WriteString(`\s*$`)

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("grammar: compile %q: %w", template, err)
	}
	// A template that is nothing but optionals would match the empty string,
	// and therefore every other rule's input. Reject it at construction.
	if !seenMandatory {
		return nil, fmt.Errorf("grammar: %q contains only optional elements", template)
	}
	return &Pattern{source: template, re: re, slots: slots}, nil
}

// alternation renders a group's alternatives, allowing multi-word ones
// ("what is|whats") by turning their internal spaces into flexible whitespace.
func alternation(alts []string) string {
	out := make([]string, 0, len(alts))
	for _, a := range alts {
		words := strings.Fields(a)
		if len(words) == 0 {
			continue
		}
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		out = append(out, strings.Join(words, `\s+`))
	}
	// Longest first so "test suite" is preferred over "test" when both fit.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return `(?:` + strings.Join(out, "|") + `)`
}

// MustCompile is Compile that panics. Patterns are compile-time constants in
// rules.go, so a malformed one must stop the process at startup rather than
// failing silently on the first utterance that would have matched it.
func MustCompile(template string) *Pattern {
	p, err := Compile(template)
	if err != nil {
		panic(err)
	}
	return p
}

// Match tests an utterance and returns the captured slots.
func (p *Pattern) Match(s string) (map[string]string, bool) {
	m := p.re.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	out := make(map[string]string, len(p.slots))
	for i, name := range p.re.SubexpNames() {
		if name == "" || i >= len(m) {
			continue
		}
		out[name] = strings.TrimSpace(m[i])
	}
	return out, true
}

var validSlot = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// collect gathers a bracketed group, which may span several whitespace-separated
// tokens and may contain multi-word alternatives: "[what is|whats|show]".
func collect(tokens []string, start int, open, closing byte) ([]string, int, error) {
	var raw []string
	i := start
	for i < len(tokens) {
		t := tokens[i]
		if i == start {
			t = strings.TrimPrefix(t, string(open))
		}
		done := strings.HasSuffix(t, string(closing))
		t = strings.TrimSuffix(t, string(closing))
		raw = append(raw, t)
		i++
		if done {
			// Re-join first, then split on "|", so an alternative may contain
			// spaces. Splitting per token would turn "what is|whats" into the
			// three alternatives "what", "is" and "whats".
			var parts []string
			for _, alt := range strings.Split(strings.Join(raw, " "), "|") {
				if alt = strings.TrimSpace(alt); alt != "" {
					parts = append(parts, alt)
				}
			}
			if len(parts) == 0 {
				return nil, 0, fmt.Errorf("grammar: empty group at token %d", start)
			}
			return parts, i, nil
		}
	}
	return nil, 0, fmt.Errorf("grammar: unclosed %c group at token %d", open, start)
}
