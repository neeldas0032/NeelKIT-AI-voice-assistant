// Package llm is the boundary between NeelKIT and whatever language model is
// configured.
//
// The interface exists so the assistant never depends on a vendor. The default
// provider is Ollama on localhost, which is what makes the local-first promise
// real: with it installed, no audio, transcript or source code leaves the
// machine, and no API key is needed to run the project at all.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Errors callers distinguish.
var (
	// ErrUnavailable means the provider could not be reached. The assistant
	// degrades to grammar-only rather than failing the turn.
	ErrUnavailable = errors.New("llm: provider unavailable")
	// ErrInvalidPlan means the model returned something that is not a valid
	// plan. It is never retried blindly: a model that produced malformed JSON
	// once will usually do it again, and a retry loop against a local model is
	// a CPU stall the user watches.
	ErrInvalidPlan = errors.New("llm: model returned an invalid plan")
)

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries tool output back to the model. It is always delimited
	// and marked untrusted: tool output is data the model reads, never
	// instructions it follows.
	RoleTool Role = "tool"
)

// Message is one turn of conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ToolSpec describes a callable tool to the model. It is built from the tool
// registry at request time, so registering a tool makes it callable with no
// change here.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Request is one completion.
type Request struct {
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature float64
	// JSONOnly asks the provider to constrain output to a JSON object. Used
	// for planning, where free prose is never a valid answer.
	JSONOnly bool
}

// ToolCall is one action the model proposes. It is a proposal only: it still
// passes schema validation, the policy gate and, for anything that writes or
// executes, an explicit human approval.
type ToolCall struct {
	Name string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// Usage reports token counts when the provider supplies them.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is one completion result.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Model     string
	Usage     Usage
	Latency   time.Duration
}

// Provider is a language-model backend.
type Provider interface {
	// Name identifies the provider for logs and settings.
	Name() string
	// Model is the configured model identifier.
	Model() string
	// Complete runs one completion.
	Complete(ctx context.Context, req Request) (Response, error)
	// Health reports whether the provider is reachable. Used by /readyz, and
	// by the router to decide whether to attempt an LLM fallback at all.
	Health(ctx context.Context) error
}
