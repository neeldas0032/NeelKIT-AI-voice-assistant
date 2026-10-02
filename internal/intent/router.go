// Package intent decides what a spoken sentence means.
//
// Routing is two-tier on purpose. A compiled grammar answers the small set of
// commands a developer says every day — instantly, deterministically, at zero
// token cost and with unit tests. Everything else goes to a language model,
// which is flexible but slow, non-deterministic and optional. The system stays
// useful with no model installed at all, which is what makes the local-first
// claim more than marketing.
package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent/grammar"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

// Source records which tier resolved an utterance. It is surfaced in the UI:
// a user should be able to see at a glance whether a command was understood by
// a rule they can read or inferred by a model they cannot.
type Source string

const (
	SourceGrammar Source = "grammar"
	SourceLLM     Source = "llm"
	SourceNone    Source = "none"
)

// Call is a proposed tool invocation.
type Call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Intent is the router's decision.
type Intent struct {
	// Name is the matched rule, or "chat" when the model answered without
	// proposing any tool.
	Name string `json:"name"`
	// Source is which tier produced this.
	Source Source `json:"source"`
	// Confidence is 1.0 for a grammar match, which is exact by construction,
	// and a fixed lower value for the model, which is a guess by construction.
	Confidence float64 `json:"confidence"`
	// Calls are the proposed tool invocations. Proposals only: each still
	// passes schema validation, the policy gate and, where required, a human.
	Calls []Call `json:"calls"`
	// Reply is prose to show the user when there is nothing to run.
	Reply string `json:"reply,omitempty"`
	// Slots are the grammar's raw captures, for display and debugging.
	Slots map[string]string `json:"slots,omitempty"`
	// Pattern is the grammar template that matched.
	Pattern string `json:"pattern,omitempty"`
	// Latency is how long routing took.
	Latency time.Duration `json:"-"`
}

// MarshalJSON always writes calls as a list, never null; see assistant.Turn.
func (in Intent) MarshalJSON() ([]byte, error) {
	type plain Intent
	if in.Calls == nil {
		in.Calls = []Call{}
	}
	return json.Marshal(plain(in))
}

// Config configures a Router.
type Config struct {
	// Registry supplies the tool descriptors the model is told about.
	Registry *tools.Registry
	// Provider is the language model. Nil disables the fallback tier entirely,
	// which is a supported configuration, not a broken one.
	Provider llm.Provider
	// Timeout bounds one model call.
	Timeout time.Duration
	// History is how many previous turns to include as context.
	History int
}

// Router resolves utterances to intents.
type Router struct {
	cfg     Config
	matcher *grammar.Matcher
}

// New returns a router.
func New(cfg Config) *Router {
	if cfg.Registry == nil {
		cfg.Registry = tools.Default
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.History <= 0 {
		cfg.History = 6
	}
	return &Router{cfg: cfg, matcher: grammar.NewMatcher()}
}

// Matcher exposes the compiled grammar, for documentation endpoints.
func (r *Router) Matcher() *grammar.Matcher { return r.matcher }

// HasModel reports whether an LLM tier is configured.
func (r *Router) HasModel() bool { return r.cfg.Provider != nil }

// systemPrompt frames the model's job.
//
// It is short on purpose: a long prompt on a small local model crowds out the
// tool schemas, which are the part that actually determines whether the output
// is usable.
const systemPrompt = `You are NeelKIT, a voice assistant for a software developer.
The user speaks; their words arrive as transcribed text that may contain recognition errors.

Be concise: your reply is read aloud, so one or two sentences.
Prefer a tool call over a guess. If the request is ambiguous, ask one short question instead of acting.
You never have permission to act on your own; every tool call you propose is shown to the user for approval.`

// Route resolves an utterance.
//
// Grammar first. If it misses and a model is configured, the model is asked. If
// the model is unavailable, the failure is returned as a reply rather than an
// error: the user should be told the assistant could not understand them, not
// shown a stack trace.
func (r *Router) Route(ctx context.Context, text string, history []llm.Message) (Intent, error) {
	start := time.Now()
	text = strings.TrimSpace(text)
	if text == "" {
		return Intent{Name: "empty", Source: SourceNone, Latency: time.Since(start)}, nil
	}

	if m, ok := r.matcher.Match(text); ok {
		return Intent{
			Name:       m.Intent,
			Source:     SourceGrammar,
			Confidence: 1.0,
			Calls:      []Call{{Tool: m.Tool, Args: m.Args}},
			Slots:      m.Slots,
			Pattern:    m.Pattern,
			Latency:    time.Since(start),
		}, nil
	}

	if r.cfg.Provider == nil {
		return Intent{
			Name:   "unmatched",
			Source: SourceNone,
			Reply: "I didn't recognise that as a command, and no language model is configured. " +
				"Try one of the built-in commands, or set up a model in Settings.",
			Latency: time.Since(start),
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	msgs := trimHistory(history, r.cfg.History)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: text})

	resp, err := r.cfg.Provider.Complete(ctx, llm.Request{
		System:      systemPrompt,
		Messages:    msgs,
		Tools:       toolSpecs(r.cfg.Registry),
		Temperature: 0.1, // planning wants the likeliest answer, not a creative one
		MaxTokens:   512,
	})
	if err != nil {
		// Degrade rather than fail. An unreachable model means the grammar
		// tier still works, and the user is told plainly what happened.
		if errors.Is(err, llm.ErrUnavailable) {
			return Intent{
				Name:    "unmatched",
				Source:  SourceNone,
				Reply:   "I couldn't reach the language model, so I only understood built-in commands just now.",
				Latency: time.Since(start),
			}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			// The model is reachable but too slow for this machine. On a CPU
			// that is the common case, not an exception, so it is a reply the
			// user can act on rather than a failed turn.
			return Intent{
				Name:   "unmatched",
				Source: SourceNone,
				Reply: fmt.Sprintf("The language model took longer than %s to answer, so I stopped waiting. "+
					"Built-in commands are instant; a smaller model is faster on this machine.",
					r.cfg.Timeout.Round(time.Second)),
				Latency: time.Since(start),
			}, nil
		}
		return Intent{}, fmt.Errorf("planner: %w", err)
	}

	in := Intent{
		Name:       "chat",
		Source:     SourceLLM,
		Confidence: 0.7, // a model's proposal is a guess; the UI should say so
		Reply:      resp.Text,
		Latency:    time.Since(start),
	}
	for _, c := range resp.ToolCalls {
		args := map[string]any{}
		if len(c.Args) > 0 {
			if err := json.Unmarshal(c.Args, &args); err != nil {
				return in, fmt.Errorf("%w: arguments for %q are not an object", llm.ErrInvalidPlan, c.Name)
			}
		}
		in.Calls = append(in.Calls, Call{Tool: c.Name, Args: args})
	}
	if len(in.Calls) > 0 {
		in.Name = in.Calls[0].Tool
	}
	return in, nil
}

// Warm sends one throwaway request so the model is loaded and the fixed part of
// every prompt — the system prompt and the tool list, about a thousand tokens —
// is already evaluated and cached before the user speaks.
//
// On a GPU this saves little. On a CPU, evaluating that prefix is most of the
// cost of a request, and without warming it the first question after startup
// is the one that times out. Ollama reuses the cached prefix for later
// requests as long as the model stays loaded, which the provider arranges.
//
// It is safe to call in the background and does nothing without a model.
func (r *Router) Warm(ctx context.Context) error {
	if r.cfg.Provider == nil {
		return nil
	}
	_, err := r.cfg.Provider.Complete(ctx, llm.Request{
		System:      systemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Tools:       toolSpecs(r.cfg.Registry),
		Temperature: 0.1,
		MaxTokens:   1,
	})
	return err
}

// toolSpecs renders the registry as the model's contract. Building it per
// request rather than caching it is what makes a newly registered tool
// immediately callable with no wiring.
func toolSpecs(reg *tools.Registry) []llm.ToolSpec {
	ds := reg.Descriptors()
	out := make([]llm.ToolSpec, 0, len(ds))
	for _, d := range ds {
		out = append(out, llm.ToolSpec{
			Name:        d.Name,
			Description: d.Description,
			Schema:      d.Schema,
		})
	}
	return out
}

// trimHistory keeps the most recent n messages. Local models have modest
// context windows, and an unbounded history silently pushes the tool schemas
// out of the prompt — which looks like the model getting worse over a long
// session for no visible reason.
func trimHistory(h []llm.Message, n int) []llm.Message {
	if len(h) <= n {
		return append([]llm.Message(nil), h...)
	}
	return append([]llm.Message(nil), h[len(h)-n:]...)
}
