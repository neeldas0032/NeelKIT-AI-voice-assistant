package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
)

// Every request pins the context window and keeps the model resident. Both
// are silent failures when missing: a too-small window drops the system prompt
// and tool list from the front, and an unloaded model makes the next request
// pay the load time again.
func TestRequestPinsContextAndResidency(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"{\"reply\":\"ok\",\"calls\":[]}"}}`))
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Tools:    []llm.ToolSpec{{Name: "fs.list", Description: "list", Schema: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.NumCtx != contextTokens {
		t.Fatalf("num_ctx = %d, want %d", got.Options.NumCtx, contextTokens)
	}
	if got.KeepAlive != keepAlive {
		t.Fatalf("keep_alive = %q, want %q", got.KeepAlive, keepAlive)
	}
}

// A slow model is a timeout, not an outage.
func TestSlowModelIsATimeoutNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server notices the client hanging up.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "m", Timeout: 50 * time.Millisecond})
	_, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("err = %v is reported as unavailable", err)
	}
}
