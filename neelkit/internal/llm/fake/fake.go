// Package fake provides a deterministic llm.Provider for tests and for running
// NeelKIT with no model installed.
package fake

import (
	"context"
	"sync"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
)

// Provider returns scripted responses.
type Provider struct {
	mu sync.Mutex
	// Responses are returned in order; the last one repeats.
	Responses []llm.Response
	// Err, when set, is returned instead.
	Err error
	// Unhealthy makes Health fail, for testing graceful degradation.
	Unhealthy bool
	// Requests records everything received.
	Requests []llm.Request

	idx int
}

// New returns a provider that replies with the given text and no tool calls.
func New(text string) *Provider {
	return &Provider{Responses: []llm.Response{{Text: text, Model: "fake"}}}
}

// WithCalls returns a provider that proposes the given tool calls.
func WithCalls(reply string, calls ...llm.ToolCall) *Provider {
	return &Provider{Responses: []llm.Response{{Text: reply, ToolCalls: calls, Model: "fake"}}}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "fake" }

// Model implements llm.Provider.
func (p *Provider) Model() string { return "fake" }

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Requests = append(p.Requests, req)
	if p.Err != nil {
		return llm.Response{}, p.Err
	}
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if len(p.Responses) == 0 {
		return llm.Response{Model: "fake"}, nil
	}
	i := p.idx
	if i >= len(p.Responses) {
		i = len(p.Responses) - 1
	}
	p.idx++
	return p.Responses[i], nil
}

// Health implements llm.Provider.
func (p *Provider) Health(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Unhealthy {
		return llm.ErrUnavailable
	}
	return nil
}

var _ llm.Provider = (*Provider)(nil)
