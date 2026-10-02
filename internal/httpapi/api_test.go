package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	asrfake "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr/fake"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/assistant"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/auth"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/config"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/runner"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/sandbox"

	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fsx"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
)

type harness struct {
	srv   *Server
	token string
	db    *storage.DB
	root  string
}

// newHarness builds the whole API over a fake ASR engine.
//
// Nothing here needs ONNX Runtime or a 670 MB model download, which is exactly
// why asr.Engine is an interface: the transport, the assistant and the tool
// system are all testable on a bare CI runner in under a second.
func newHarness(t *testing.T) *harness {
	t.Helper()

	root := t.TempDir()
	sb, err := sandbox.New(sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	uid, err := db.EnsureUser(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	token, key, err := auth.Generate(uid, "test")
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewStore()
	keys.Add(key)

	engine := asrfake.New("create a git branch called feature auth")
	router := intent.New(intent.Config{})
	run := runner.New(runner.Config{Sandbox: sb, Policy: policy.DefaultSettings()})

	cfg := config.Default()
	cfg.WorkspaceRoot = root
	cfg.DataDir = t.TempDir()
	cfg.CORSOrigins = []string{"http://localhost:3000"}

	return &harness{
		srv: New(Deps{
			Config: cfg, Engine: engine, Registry: tools.Default, DB: db,
			Keys: keys, Tickets: auth.NewTicketStore(0),
			Metrics: observability.NewMetrics(), Router: router,
			Assistant: assistant.New(assistant.Config{
				Router: router, Runner: run, DB: db, UserID: uid,
			}),
			UserID: uid,
		}),
		token: token, db: db, root: root,
	}
}

func (h *harness) do(t *testing.T, method, path string, body io.Reader, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if authed {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if body != nil && !strings.Contains(path, "transcriptions") {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) postJSON(t *testing.T, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return h.do(t, http.MethodPost, path, bytes.NewReader(raw), true)
}

// ------------------------------------------------------------------ auth

// This service can run programs. Every route that touches data or the machine
// must refuse an unauthenticated caller.
func TestEveryDataRouteRequiresAuth(t *testing.T) {
	h := newHarness(t)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/assistant/turns"},
		{http.MethodPost, "/v1/assistant/approvals"},
		{http.MethodGet, "/v1/conversations"},
		{http.MethodGet, "/v1/transcripts/search?q=x"},
		{http.MethodGet, "/v1/tools"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1/audit"},
		{http.MethodPost, "/v1/realtime/tickets"},
		{http.MethodPost, "/v1/audio/transcriptions"},
	}
	for _, r := range routes {
		rec := h.do(t, r.method, r.path, strings.NewReader("{}"), false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token -> %d, want 401", r.method, r.path, rec.Code)
		}
	}
}

func TestInvalidAndMalformedTokensAreRejected(t *testing.T) {
	h := newHarness(t)
	for _, header := range []string{
		"", "Bearer", "Bearer ", "Basic abc", "Bearer not-a-key",
		"Bearer nk_deadbeef_wrong", "Bearer " + h.token + "x",
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q -> %d, want 401", header, rec.Code)
		}
	}
}

// Operational probes must work without a credential: a liveness check that
// needs a key fails exactly when a key problem is what you are diagnosing.
func TestOperationalRoutesAreUnauthenticated(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		if rec := h.do(t, http.MethodGet, path, nil, false); rec.Code >= 500 && path != "/readyz" {
			t.Errorf("%s -> %d", path, rec.Code)
		}
	}
	if rec := h.do(t, http.MethodGet, "/healthz", nil, false); rec.Code != http.StatusOK {
		t.Errorf("/healthz -> %d, want 200", rec.Code)
	}
}

// Liveness must not consult dependencies. A liveness probe that fails during a
// slow database restart puts the process into a restart loop.
func TestHealthzIgnoresDependencies(t *testing.T) {
	h := newHarness(t)
	_ = h.db.Close() // simulate the database being gone

	if rec := h.do(t, http.MethodGet, "/healthz", nil, false); rec.Code != http.StatusOK {
		t.Fatalf("/healthz -> %d with a dead database, want 200", rec.Code)
	}
	rec := h.do(t, http.MethodGet, "/readyz", nil, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz -> %d with a dead database, want 503", rec.Code)
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Ready || body.Checks["database"] == "ok" {
		t.Fatalf("readyz body = %+v, want a failing database check", body)
	}
}

// --------------------------------------------------------------- requests

func TestRequestIDIsEchoedAndGenerated(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodGet, "/healthz", nil, false)
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("no request id generated")
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "my-trace-1")
	rec = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "my-trace-1" {
		t.Fatalf("request id = %q, want the client's value echoed", got)
	}

	// A client-supplied id lands in logs, so a hostile one must be replaced,
	// not trusted.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "bad\nvalue\rinjected")
	rec = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); strings.ContainsAny(got, "\n\r") {
		t.Fatalf("request id %q was not sanitised", got)
	}
}

func TestErrorsCarryTheRequestID(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodGet, "/v1/tools", nil, false)

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.RequestID == "" || body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error body is missing fields: %+v", body.Error)
	}
	if body.Error.RequestID != rec.Header().Get("X-Request-Id") {
		t.Fatal("the id in the body does not match the header")
	}
}

// ------------------------------------------------------------------ CORS

func TestCORSAllowlist(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/tools", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allowed origin -> %q", got)
	}
	if rec.Header().Get("Vary") == "" {
		t.Error("no Vary: Origin header; a shared cache could cross origins")
	}

	// An origin that is not on the list gets no CORS headers at all, so the
	// browser refuses the response.
	req = httptest.NewRequest(http.MethodOptions, "/v1/tools", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("an unlisted origin received CORS headers")
	}
}

// ------------------------------------------------------------ transcribe

func TestTranscriptionEndpoint(t *testing.T) {
	h := newHarness(t)

	samples := make([]float32, audio.SampleRate) // one second
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(audio.EncodeWAV(samples, audio.SampleRate)); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("timestamps", "word")
	_ = mw.WriteField("confidence", "true")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out transcriptionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Text == "" || out.Confidence == 0 || len(out.Words) == 0 {
		t.Fatalf("response = %+v", out)
	}
	// Duration comes from the decoded waveform, not from a byte count.
	if out.Duration < 0.9 || out.Duration > 1.1 {
		t.Fatalf("duration = %v, want ~1.0s from the decoded audio", out.Duration)
	}
	if out.Metrics == nil {
		t.Error("no measured metrics returned")
	}
}

func TestTranscriptionRejectsMissingFileAndGarbage(t *testing.T) {
	h := newHarness(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("language", "en")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing file -> %d, want 400", rec.Code)
	}

	body.Reset()
	mw = multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "not-audio.bin")
	_, _ = part.Write([]byte("this is definitely not audio"))
	mw.Close()

	req = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("garbage audio -> %d, want 415", rec.Code)
	}
}

// --------------------------------------------------------------- assistant

func TestTurnEndpointPlansAndGates(t *testing.T) {
	h := newHarness(t)

	rec := h.postJSON(t, "/v1/assistant/turns", map[string]any{
		"text": "make a note buy milk",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var turn assistant.Turn
	if err := json.Unmarshal(rec.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Status != assistant.StatusAwaitingApproval || len(turn.Calls) != 1 {
		t.Fatalf("turn = %+v", turn)
	}
	if turn.Calls[0].Preview == "" {
		t.Error("no preview returned for the approval card")
	}

	// Approving through the API must actually run it.
	rec = h.postJSON(t, "/v1/assistant/approvals", map[string]any{
		"call_id": turn.Calls[0].CallID, "decision": "approve",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approval -> %d: %s", rec.Code, rec.Body.String())
	}
	var res assistant.CallResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Status != "succeeded" {
		t.Fatalf("call result = %+v", res)
	}

	// The same approval twice is a conflict, not a second execution.
	rec = h.postJSON(t, "/v1/assistant/approvals", map[string]any{
		"call_id": turn.Calls[0].CallID, "decision": "approve",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("replayed approval -> %d, want 409", rec.Code)
	}
}

func TestTurnDryRunTouchesNothing(t *testing.T) {
	h := newHarness(t)
	execute := false

	rec := h.postJSON(t, "/v1/assistant/turns", map[string]any{
		"text": "make a note buy milk", "execute": &execute,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var turn assistant.Turn
	_ = json.Unmarshal(rec.Body.Bytes(), &turn)
	if turn.Calls[0].Status != "planned" {
		t.Fatalf("dry run produced %q", turn.Calls[0].Status)
	}
}

// A field name the client got wrong must be an error, not a silently ignored
// value that makes the request do something other than intended.
func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	h := newHarness(t)
	rec := h.postJSON(t, "/v1/assistant/turns", map[string]any{
		"text": "hello", "excute": true, // typo
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field -> %d, want 400", rec.Code)
	}
}

func TestTurnRequiresText(t *testing.T) {
	h := newHarness(t)
	for _, body := range []map[string]any{{}, {"text": ""}, {"text": "   "}} {
		if rec := h.postJSON(t, "/v1/assistant/turns", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%v -> %d, want 400", body, rec.Code)
		}
	}
}

// ------------------------------------------------------------- read paths

func TestConversationsAndSearch(t *testing.T) {
	h := newHarness(t)

	rec := h.postJSON(t, "/v1/assistant/turns", map[string]any{"text": "list the files"})
	if rec.Code != http.StatusOK {
		t.Fatalf("turn -> %d: %s", rec.Code, rec.Body.String())
	}
	var turn assistant.Turn
	_ = json.Unmarshal(rec.Body.Bytes(), &turn)

	rec = h.do(t, http.MethodGet, "/v1/conversations", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), turn.ConversationID) {
		t.Fatalf("conversations -> %d: %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodGet, "/v1/conversations/"+turn.ConversationID+"/messages", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages -> %d: %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodGet, "/v1/conversations/conv_nope/messages", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown conversation -> %d, want 404", rec.Code)
	}

	rec = h.do(t, http.MethodDelete, "/v1/conversations/"+turn.ConversationID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete -> %d", rec.Code)
	}
}

func TestSearchRequiresAQuery(t *testing.T) {
	h := newHarness(t)
	if rec := h.do(t, http.MethodGet, "/v1/transcripts/search", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("search without q -> %d, want 400", rec.Code)
	}
}

// The tools endpoint returns exactly what the model is told, so what the UI
// shows the user it can do is what the model was actually offered.
func TestToolsEndpointMatchesTheRegistry(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodGet, "/v1/tools", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Tools []tools.Descriptor `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != tools.Default.Len() {
		t.Fatalf("endpoint returned %d tools, registry has %d", len(body.Tools), tools.Default.Len())
	}
	for _, d := range body.Tools {
		if d.Schema == nil || d.Risk == "" {
			t.Errorf("tool %q exposed without a schema or risk level", d.Name)
		}
	}
}

// --------------------------------------------------------------- realtime

func TestRealtimeTicketFlow(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/realtime/tickets", nil, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ticket -> %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.HasPrefix(body.Ticket, "rt_") {
		t.Fatalf("ticket = %q", body.Ticket)
	}

	// The upgrade without a ticket must be refused before any socket work.
	rec = h.do(t, http.MethodGet, "/v1/realtime", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("upgrade without a ticket -> %d, want 401", rec.Code)
	}
	rec = h.do(t, http.MethodGet, "/v1/realtime?ticket=rt_fabricated", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("upgrade with a fake ticket -> %d, want 401", rec.Code)
	}
}

// A WebSocket handshake is not subject to CORS preflight, so the Origin check
// must happen in the handler or it does not happen at all.
func TestRealtimeChecksOrigin(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/realtime/tickets", nil, true)
	var body struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?ticket="+body.Ticket, nil)
	req.Header.Set("Origin", "https://evil.example")
	rec2 := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("upgrade from an unlisted origin -> %d, want 403", rec2.Code)
	}
}

// ---------------------------------------------------------------- metrics

func TestMetricsExposition(t *testing.T) {
	h := newHarness(t)
	_ = h.do(t, http.MethodGet, "/v1/tools", nil, true)

	rec := h.do(t, http.MethodGet, "/metrics", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics -> %d", rec.Code)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"# TYPE neelkit_http_requests_total counter",
		"neelkit_http_request_duration_seconds_bucket",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// Routes must be labelled by pattern, not by path, or every conversation
	// id becomes its own time series.
	if strings.Contains(out, "conv_01") {
		t.Error("a concrete id leaked into a metric label")
	}
}

// -------------------------------------------------------------- overload

func TestOverloadReturns503WithRetryAfter(t *testing.T) {
	h := newHarness(t)
	engine := h.srv.deps.Engine.(*asrfake.Engine)
	engine.Err = asr.ErrOverloaded

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "clip.wav")
	_, _ = part.Write(audio.EncodeWAV(make([]float32, audio.SampleRate), audio.SampleRate))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("overloaded -> %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header; a client cannot back off correctly")
	}
}

// A panic in one handler must not take down a server that is mid-transcription
// for somebody else.
func TestPanicIsContained(t *testing.T) {
	h := newHarness(t)
	h.srv.mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("deliberate")
	})
	rec := h.do(t, http.MethodGet, "/boom", nil, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler -> %d, want 500", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/healthz", nil, false); rec.Code != http.StatusOK {
		t.Fatal("the server stopped serving after a panic")
	}
}

// Metrics must be labelled by route pattern. Reading r.Pattern from an outer
// middleware silently labelled everything "unmatched", which looks like working
// metrics until someone tries to answer a question with them.
func TestMetricsAreLabelledByRoutePattern(t *testing.T) {
	h := newHarness(t)
	_ = h.do(t, http.MethodGet, "/v1/tools", nil, true)
	_ = h.do(t, http.MethodGet, "/v1/conversations", nil, true)

	out := h.do(t, http.MethodGet, "/metrics", nil, true).Body.String()
	for _, want := range []string{
		`route="GET /v1/tools"`,
		`route="GET /v1/conversations"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `route="unmatched"`) {
		t.Error("a routed request was labelled unmatched")
	}
}
