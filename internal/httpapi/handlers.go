package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/assistant"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/normalize"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
)

// transcriptionResponse is the batch transcription result.
//
// The shape is OpenAI-compatible so existing tooling works unchanged, with
// NeelKIT's additions (word timings, real confidence, measured metrics) as
// extra fields that default to absent.
type transcriptionResponse struct {
	Text       string     `json:"text"`
	Language   string     `json:"language"`
	Duration   float64    `json:"duration"`
	Model      string     `json:"model"`
	Confidence float64    `json:"confidence,omitempty"`
	Words      []asr.Word `json:"words,omitempty"`
	Metrics    *txMetrics `json:"metrics,omitempty"`
}

type txMetrics struct {
	RTF         float64 `json:"rtf"`
	InferenceMS int64   `json:"inference_ms"`
	QueueWaitMS int64   `json:"queue_wait_ms"`
}

func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.Engine == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "asr_disabled",
			"no speech model is loaded", nil)
		return
	}

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if errors.Is(err, errBodyTooLarge) || strings.Contains(err.Error(), "too large") {
			writeError(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "body_too_large",
				"the upload exceeds the configured limit", nil)
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid_request", "bad_multipart", err.Error(), nil)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "missing_field",
			"a multipart field named \"file\" is required", nil)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, s.deps.Config.MaxUploadBytes))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "unreadable_upload", err.Error(), nil)
		return
	}

	samples, err := s.decodeAudio(r, data)
	if err != nil {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media", "undecodable_audio",
			err.Error(), map[string]any{"filename": header.Filename})
		return
	}

	wantWords := r.FormValue("timestamps") == "word"
	res, err := s.deps.Engine.Transcribe(r.Context(), asr.Request{
		Samples:   samples,
		Language:  formOr(r, "language", "en"),
		Priority:  asr.PriorityBatch,
		WantWords: wantWords,
	})
	if err != nil {
		mapError(w, r, err)
		return
	}

	text := res.Text
	if parseBool(r.FormValue("normalize")) {
		text = normalize.Text(text, normalize.DictationOptions())
	}

	if s.deps.Metrics != nil {
		s.deps.Metrics.ASRRTF.Observe(res.RTF)
		s.deps.Metrics.ASRQueueWait.Observe(res.QueueWait.Seconds())
		s.deps.Metrics.ASRDuration.Observe(res.Inference.Seconds(), "batch", "batch")
	}
	s.persistTranscript(r, res, text)

	info := s.deps.Engine.Info()
	switch r.FormValue("response_format") {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case "srt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, buildSRT(res, text))
	case "vtt":
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		_, _ = io.WriteString(w, buildVTT(res, text))
	default:
		out := transcriptionResponse{
			Text: text, Language: formOr(r, "language", "en"),
			// Duration comes from the decoded waveform, not from the byte
			// count: dividing upload bytes by a hardcoded bitrate (what the
			// reference implementation does) is meaningless for a compressed
			// file and poisons every timestamp derived from it.
			Duration: res.Duration.Seconds(),
			Model:    info.Model + "-" + info.Precision,
		}
		if parseBool(r.FormValue("confidence")) {
			out.Confidence = res.Confidence
		}
		if wantWords {
			out.Words = res.Words
		}
		out.Metrics = &txMetrics{
			RTF:         round3(res.RTF),
			InferenceMS: res.Inference.Milliseconds(),
			QueueWaitMS: res.QueueWait.Milliseconds(),
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// decodeAudio turns an upload into samples, transcoding through ffmpeg when the
// payload is not WAV.
//
// Format is decided by content, never by filename: a client may upload a file
// with no extension or a misleading one, and the server only ever sees bytes.
func (s *Server) decodeAudio(r *http.Request, data []byte) ([]float32, error) {
	if audio.IsWAV(data) {
		return audio.DecodeWAV(data)
	}
	wav, err := s.transcoder.Convert(r.Context(), data)
	if err != nil {
		return nil, err
	}
	return audio.DecodeWAV(wav)
}

func (s *Server) persistTranscript(r *http.Request, res asr.Result, text string) {
	if s.deps.DB == nil {
		return
	}
	info := s.deps.Engine.Info()
	words, _ := json.Marshal(res.Words)
	_, _ = s.deps.DB.SaveTranscript(r.Context(), storage.Transcript{
		UserID: UserID(r.Context()), RawText: res.Text, Text: text,
		Language: formOr(r, "language", "en"), DurationMS: res.Duration.Milliseconds(),
		Confidence: res.Confidence, Model: info.Model, Device: info.Device,
		RTF: res.RTF, WordsJSON: string(words),
	})
}

// --------------------------------------------------------------- assistant

type turnRequest struct {
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
	Execute        *bool  `json:"execute"`
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	var req turnRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "bad_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "missing_field",
			"\"text\" is required", nil)
		return
	}

	// execute defaults to true, so the common case needs no flag, but a client
	// can ask for a plan without side effects.
	dryRun := req.Execute != nil && !*req.Execute

	turn, err := s.deps.Assistant.Process(r.Context(), assistant.Input{
		ConversationID: req.ConversationID,
		Text:           req.Text,
		Source:         "text",
		DryRun:         dryRun,
	})
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, turn)
}

type approvalRequest struct {
	CallID   string `json:"call_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "bad_json", err.Error(), nil)
		return
	}
	if req.CallID == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "missing_field",
			"\"call_id\" is required", nil)
		return
	}

	switch req.Decision {
	case "approve":
		res, err := s.deps.Assistant.Approve(r.Context(), req.CallID)
		if err != nil {
			mapError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "deny":
		if err := s.deps.Assistant.Deny(r.Context(), req.CallID, req.Reason); err != nil {
			mapError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"call_id": req.CallID, "status": "denied"})
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_request", "bad_decision",
			`"decision" must be "approve" or "deny"`, nil)
	}
}

// ----------------------------------------------------------- conversations

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusOK, map[string]any{"conversations": []any{}})
		return
	}
	list, err := s.deps.DB.ListConversations(r.Context(), UserID(r.Context()), intQuery(r, "limit", 50))
	if err != nil {
		mapError(w, r, err)
		return
	}
	if list == nil {
		list = []storage.Conversation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []any{}})
		return
	}
	convID := r.PathValue("id")
	if _, err := s.deps.DB.GetConversation(r.Context(), convID); err != nil {
		mapError(w, r, err)
		return
	}
	msgs, err := s.deps.DB.ListMessages(r.Context(), convID, intQuery(r, "limit", 200))
	if err != nil {
		mapError(w, r, err)
		return
	}
	if msgs == nil {
		msgs = []storage.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": convID, "messages": msgs})
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.deps.DB.DeleteConversation(r.Context(), r.PathValue("id")); err != nil {
		mapError(w, r, err)
		return
	}
	// The audit trail deliberately survives: deleting a conversation removes
	// what was said, not the record of what was run on the machine.
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true,
		"note":    "audit records of any commands that ran are retained",
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "missing_field",
			"\"q\" is required", nil)
		return
	}
	hits, err := s.deps.DB.SearchTranscripts(r.Context(), UserID(r.Context()), q, intQuery(r, "limit", 25))
	if err != nil {
		mapError(w, r, err)
		return
	}
	if hits == nil {
		hits = []storage.Transcript{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "results": hits})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.deps.DB == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		return
	}
	hours := intQuery(r, "hours", 24)
	events, err := s.deps.DB.ListAudit(r.Context(),
		time.Now().Add(-time.Duration(hours)*time.Hour), intQuery(r, "limit", 100))
	if err != nil {
		mapError(w, r, err)
		return
	}
	if events == nil {
		events = []storage.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// ------------------------------------------------------------------ tools

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	// This is the same data the language model is given. What the UI shows you
	// it can do is literally what the model was told it can do.
	writeJSON(w, http.StatusOK, map[string]any{
		"tools":     s.deps.Registry.Descriptors(),
		"workspace": s.deps.Config.WorkspaceRoot,
		"read_only": s.deps.Config.WorkspaceRO,
	})
}

// --------------------------------------------------------------- tickets

func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	ticket, exp, err := s.deps.Tickets.Issue(UserID(r.Context()))
	if err != nil {
		mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ticket":      ticket,
		"expires_at":  exp.UTC().Format(time.RFC3339),
		"ttl_seconds": int(time.Until(exp).Seconds()),
	})
}

// ---------------------------------------------------------------- helpers

func formOr(r *http.Request, key, fallback string) string {
	if v := strings.TrimSpace(r.FormValue(key)); v != "" {
		return v
	}
	return fallback
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func intQuery(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func round3(f float64) float64 {
	return float64(int(f*1000+0.5)) / 1000
}

func buildSRT(res asr.Result, text string) string {
	var b strings.Builder
	if len(res.Words) == 0 {
		b.WriteString("1\n00:00:00,000 --> " + srtTime(res.Duration.Seconds()) + "\n" + text + "\n")
		return b.String()
	}
	// Group words into cues of roughly one line each, so the output is
	// readable rather than one word per subtitle.
	for i, cue := range groupWords(res.Words, 10) {
		b.WriteString(strconv.Itoa(i+1) + "\n")
		b.WriteString(srtTime(cue.start) + " --> " + srtTime(cue.end) + "\n")
		b.WriteString(cue.text + "\n\n")
	}
	return b.String()
}

func buildVTT(res asr.Result, text string) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	if len(res.Words) == 0 {
		b.WriteString("00:00:00.000 --> " + vttTime(res.Duration.Seconds()) + "\n" + text + "\n")
		return b.String()
	}
	for _, cue := range groupWords(res.Words, 10) {
		b.WriteString(vttTime(cue.start) + " --> " + vttTime(cue.end) + "\n")
		b.WriteString(cue.text + "\n\n")
	}
	return b.String()
}

type cue struct {
	start, end float64
	text       string
}

func groupWords(words []asr.Word, per int) []cue {
	var out []cue
	for i := 0; i < len(words); i += per {
		end := i + per
		if end > len(words) {
			end = len(words)
		}
		chunk := words[i:end]
		var parts []string
		for _, w := range chunk {
			parts = append(parts, w.Word)
		}
		out = append(out, cue{
			start: chunk[0].Start,
			end:   chunk[len(chunk)-1].End,
			text:  strings.Join(parts, " "),
		})
	}
	return out
}

func srtTime(sec float64) string { return clockTime(sec, ",") }
func vttTime(sec float64) string { return clockTime(sec, ".") }

func clockTime(sec float64, sep string) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	ms := int((sec - float64(total)) * 1000)
	return pad2(total/3600) + ":" + pad2((total%3600)/60) + ":" + pad2(total%60) + sep + pad3(ms)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad3(n int) string {
	switch {
	case n < 10:
		return "00" + strconv.Itoa(n)
	case n < 100:
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
