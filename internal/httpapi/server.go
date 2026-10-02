// Package httpapi is the transport layer.
//
// Handlers decode a request, call exactly one application method, and encode
// the result. They contain no business logic, which is what makes the CLI and
// the browser provably equivalent clients of the same API: if the CLI can do
// something, the API can, because the CLI has no other route in.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/assistant"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/auth"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/config"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/vad"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version"
)

// Deps are everything the transport layer needs.
type Deps struct {
	Config    config.Config
	Engine    asr.Engine
	VAD       *vad.Detector
	Assistant *assistant.Assistant
	Router    *intent.Router
	Registry  *tools.Registry
	DB        *storage.DB
	Keys      *auth.Store
	Tickets   *auth.TicketStore
	Metrics   *observability.Metrics
	LLM       llm.Provider
	UserID    string
}

// Server routes HTTP requests.
type Server struct {
	deps       Deps
	mux        *http.ServeMux
	h          http.Handler
	transcoder *audio.Transcoder
	// registered records every pattern passed to the mux, in registration
	// order. ServeMux does not expose what it holds, and the alternative to
	// recording it here is a documentation file nobody can check.
	// TestOpenAPIMatchesTheRouter reads this.
	registered []string
}

// Routes returns the registered patterns, as "METHOD /path".
func (s *Server) Routes() []string {
	out := make([]string, len(s.registered))
	copy(out, s.registered)
	return out
}

// New builds the router and middleware chain.
func New(d Deps) *Server {
	s := &Server{
		deps: d,
		mux:  http.NewServeMux(),
		// One transcoder for the process: it probes for ffmpeg once, so a
		// missing binary produces a single clear log line rather than a
		// surprise on each upload.
		transcoder: audio.NewTranscoder(60 * time.Second),
	}
	s.routes()

	// Order matters. recoverer is outermost so it catches a panic anywhere
	// inside; requestID precedes logging so every line is correlated; CORS
	// precedes auth so a browser preflight is answered without credentials.
	s.h = chain(s.mux,
		recoverer(),
		requestID(),
		logging(),
		cors(d.Config.CORSOrigins),
		timeout(d.Config.RequestTimeout),
		bodyLimit(d.Config.MaxUploadBytes),
	)
	return s
}

// Handler returns the configured handler.
func (s *Server) Handler() http.Handler { return s.h }

func (s *Server) routes() {
	asrLimit := newRateLimiter(60, 15)
	turnLimit := newRateLimiter(120, 30)
	readLimit := newRateLimiter(600, 200)

	// open registers a route with metrics but no authentication.
	open := func(pattern string, h http.HandlerFunc) {
		s.registered = append(s.registered, pattern)
		s.mux.Handle(pattern, chain(h, instrument(s.deps.Metrics, pattern)))
	}
	// authed registers a route behind authentication and a rate limit.
	authed := func(pattern string, h http.HandlerFunc, l *rateLimiter) {
		s.registered = append(s.registered, pattern)
		s.mux.Handle(pattern, chain(h,
			instrument(s.deps.Metrics, pattern),
			authenticate(s.deps.Keys, s.auditAuth),
			rateLimit(l),
		))
	}

	// Operational endpoints are unauthenticated: a liveness probe that needs a
	// credential is a liveness probe that fails during a credential problem.
	open("GET /healthz", s.handleHealthz)
	open("GET /readyz", s.handleReadyz)
	open("GET /version", s.handleVersion)

	if s.deps.Config.MetricsPublic {
		open("GET /metrics", s.handleMetrics)
	} else {
		authed("GET /metrics", s.handleMetrics, readLimit)
	}

	authed("POST /v1/audio/transcriptions", s.handleTranscribe, asrLimit)
	authed("POST /v1/assistant/turns", s.handleTurn, turnLimit)
	authed("POST /v1/assistant/approvals", s.handleApproval, turnLimit)

	authed("GET /v1/conversations", s.handleListConversations, readLimit)
	authed("GET /v1/conversations/{id}/messages", s.handleListMessages, readLimit)
	authed("DELETE /v1/conversations/{id}", s.handleDeleteConversation, readLimit)
	authed("GET /v1/transcripts/search", s.handleSearch, readLimit)
	authed("GET /v1/audit", s.handleAudit, readLimit)

	authed("GET /v1/tools", s.handleTools, readLimit)
	authed("GET /v1/models", s.handleModels, readLimit)

	authed("POST /v1/realtime/tickets", s.handleTicket, readLimit)
	// The WebSocket route authenticates with a single-use ticket rather than a
	// header, because a browser cannot set headers on an upgrade.
	open("GET /v1/realtime", s.handleRealtime)
}

// ------------------------------------------------------------- responses

// errorBody is the single error shape every endpoint returns.
type errorBody struct {
	Error struct {
		Type      string         `json:"type"`
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id,omitempty"`
		Details   map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, kind, code, msg string, details map[string]any) {
	var b errorBody
	b.Error.Type = kind
	b.Error.Code = code
	b.Error.Message = msg
	b.Error.Details = details
	if r != nil {
		b.Error.RequestID = observability.RequestID(r.Context())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads a JSON body, rejecting unknown fields.
//
// Strictness is a kindness here: a typo'd field name that is silently ignored
// produces a request that succeeds while doing the wrong thing.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// mapError turns a domain error into an HTTP status and code.
func mapError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "not_found", "no such resource", nil)
	case errors.Is(err, tools.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "unknown_tool", err.Error(), nil)
	case errors.Is(err, tools.ErrInvalidArgs):
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid_arguments", err.Error(), nil)
	case errors.Is(err, tools.ErrDenied), errors.Is(err, tools.ErrOutsideWorkspace):
		writeError(w, r, http.StatusForbidden, "permission_denied", "denied", err.Error(), nil)
	case errors.Is(err, assistant.ErrNoSuchCall):
		writeError(w, r, http.StatusConflict, "invalid_request", "already_resolved",
			"that call has already been approved, denied or has expired", nil)
	case errors.Is(err, assistant.ErrApprovalExpired):
		writeError(w, r, http.StatusConflict, "invalid_request", "approval_expired",
			"the approval window for that call has closed", nil)
	case errors.Is(err, asr.ErrOverloaded):
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "overloaded", "overloaded",
			"every decoder worker is busy; retry shortly", nil)
	case errors.Is(err, asr.ErrAudioTooLong):
		writeError(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "audio_too_long", err.Error(), nil)
	case errors.Is(err, asr.ErrClosed):
		writeError(w, r, http.StatusServiceUnavailable, "internal", "shutting_down",
			"the server is shutting down", nil)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusGatewayTimeout, "internal", "timeout", "the request took too long", nil)
	case errors.Is(err, context.Canceled):
		// The client hung up; there is nobody to answer.
	default:
		observability.Logger(r.Context()).Error("unhandled error", "error", err, "path", r.URL.Path)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal_error",
			"something went wrong", nil)
	}
}

func (s *Server) auditAuth(ctx context.Context, action, outcome, detail string) {
	if s.deps.DB == nil {
		return
	}
	_ = s.deps.DB.Audit(ctx, storage.AuditEvent{
		RequestID: observability.RequestID(ctx),
		Actor:     "system", Action: action, Outcome: outcome,
		DetailJSON: fmt.Sprintf(`{"detail":%q}`, detail),
	})
}

// ------------------------------------------------------------ operations

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Liveness answers one question: is this process running. Checking a
	// dependency here would make a slow database restart the server in a loop.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	if s.deps.Engine != nil {
		info := s.deps.Engine.Info()
		if info.Ready {
			checks["asr"] = "ok"
		} else {
			checks["asr"] = "not loaded"
			ready = false
		}
	} else {
		checks["asr"] = "disabled"
	}

	if s.deps.DB != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.deps.DB.SQL().PingContext(ctx); err != nil {
			checks["database"] = "unreachable"
			ready = false
		} else {
			checks["database"] = "ok"
		}
	}

	if s.deps.VAD != nil {
		checks["vad"] = "ok"
	} else {
		checks["vad"] = "not loaded"
	}

	// The language model is explicitly not part of readiness: the grammar tier
	// works without it, so an unreachable Ollama must not take the server out
	// of rotation.
	if s.deps.LLM != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.deps.LLM.Health(ctx); err != nil {
			checks["llm"] = "unreachable (grammar commands still work)"
		} else {
			checks["llm"] = "ok"
		}
	} else {
		checks["llm"] = "disabled"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.deps.Metrics == nil {
		http.Error(w, "metrics disabled", http.StatusNotFound)
		return
	}
	if s.deps.Engine != nil {
		s.deps.Metrics.ASRQueueFree.Set(float64(s.deps.Engine.Info().Workers))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	s.deps.Metrics.Registry.WritePrometheus(w)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	info := asr.Info{Model: "none"}
	if s.deps.Engine != nil {
		info = s.deps.Engine.Info()
	}
	llmName, llmModel := "none", ""
	if s.deps.LLM != nil {
		llmName, llmModel = s.deps.LLM.Name(), s.deps.LLM.Model()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asr": info,
		"llm": map[string]string{"provider": llmName, "model": llmModel},
	})
}
