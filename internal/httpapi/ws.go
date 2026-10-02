package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/assistant"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/realtime"
)

// WebSocket limits.
const (
	// wsReadLimit caps one inbound message. Audio arrives in small frames; a
	// megabyte is far more than any legitimate one and stops a client from
	// forcing an unbounded allocation.
	wsReadLimit = 1 << 20
	// wsOutboundBuffer is how many events may queue for a slow client before
	// previews start being dropped.
	wsOutboundBuffer = 64
	// wsIdleTimeout closes a session that has gone quiet.
	wsIdleTimeout = 10 * time.Minute
	// wsPingEvery keeps NAT and proxy state alive and detects a dead peer.
	wsPingEvery = 30 * time.Second
)

// clientMessage is the inbound control protocol.
type clientMessage struct {
	Type string `json:"type"`

	ConversationID string `json:"conversation_id,omitempty"`
	Mode           string `json:"mode,omitempty"`
	Partials       *bool  `json:"partials,omitempty"`
	HangoverMS     int    `json:"hangover_ms,omitempty"`
	Language       string `json:"language,omitempty"`

	CallID string `json:"call_id,omitempty"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
}

// serverMessage is anything sent to the client. Realtime events and assistant
// turns share one envelope so the client has a single switch.
type serverMessage struct {
	Type string `json:"type"`

	*realtime.Event `json:",inline"`
	Turn            *assistant.Turn       `json:"turn,omitempty"`
	Call            *assistant.CallResult `json:"call,omitempty"`
	Extra           map[string]any        `json:"extra,omitempty"`
}

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	// A browser cannot set an Authorization header on an upgrade, so the
	// credential is a single-use ticket fetched over an authenticated POST.
	// Putting the API key in the query string would write it into access logs,
	// proxy logs and browser history.
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		writeError(w, r, http.StatusUnauthorized, "authentication", "missing_ticket",
			"fetch a ticket from POST /v1/realtime/tickets first", nil)
		return
	}
	userID, err := s.deps.Tickets.Redeem(ticket)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "authentication", "invalid_ticket",
			"that ticket is unknown, expired or already used", nil)
		return
	}

	// A WebSocket handshake is not subject to CORS preflight, so the Origin
	// check has to happen here explicitly. A server that relies on CORS to
	// protect its WebSocket is not protected at all.
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
		writeError(w, r, http.StatusForbidden, "permission_denied", "bad_origin",
			"this origin is not allowed to open a session", nil)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin was already checked above against the configured allowlist.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled, // PCM does not compress
	})
	if err != nil {
		observability.Logger(r.Context()).Warn("websocket upgrade failed", "error", err)
		return
	}
	conn.SetReadLimit(wsReadLimit)
	defer conn.CloseNow()

	s.runSession(r.Context(), conn, userID)
}

func (s *Server) originAllowed(origin string) bool {
	origin = strings.TrimRight(origin, "/")
	for _, o := range s.deps.Config.CORSOrigins {
		if strings.TrimRight(strings.TrimSpace(o), "/") == origin {
			return true
		}
	}
	return false
}

// runSession owns one connection for its lifetime.
//
// Exactly one goroutine writes to the socket. Every producer — the audio
// pipeline, the assistant, the ping timer — sends on a channel instead. This is
// the idiomatic answer to "a WebSocket connection does not support concurrent
// writers", and it also provides the single place where a slow client's backlog
// can be shed without blocking the decoder.
func (s *Server) runSession(parent context.Context, conn *websocket.Conn, userID string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	outbound := make(chan serverMessage, wsOutboundBuffer)
	// Written by whichever goroutine produced a preview, read by the writer.
	var dropped atomic.Int64

	// send never blocks. Previews are expendable; anything else is not, and if
	// the queue is full for a non-preview the client is beyond saving.
	send := func(m serverMessage) {
		select {
		case outbound <- m:
		default:
			if m.Event != nil && m.Event.Type == realtime.EventPartial {
				dropped.Add(1)
				return
			}
			cancel()
		}
	}
	sendEvent := func(ev realtime.Event) {
		send(serverMessage{Type: string(ev.Type), Event: &ev})
	}

	// The conversation is read by the reader loop and updated by assistant
	// goroutines, so it is guarded rather than shared bare.
	var convMu sync.Mutex
	convID := ""
	getConv := func() string { convMu.Lock(); defer convMu.Unlock(); return convID }
	setConv := func(id string) { convMu.Lock(); convID = id; convMu.Unlock() }
	var sess *realtime.Session

	onFinal := func(ctx context.Context, ev realtime.Event) {
		// The assistant runs on its own goroutine: a tool call can take
		// seconds, and blocking here would stall audio intake and make the
		// next utterance unrecognisable.
		text := ev.Text
		go func() {
			turn, err := s.deps.Assistant.Process(ctx, assistant.Input{
				ConversationID: getConv(),
				Text:           text,
				Source:         "voice",
			})
			if err != nil {
				sendEvent(realtime.Event{Type: realtime.EventError,
					Code: "assistant_failed", Message: err.Error()})
				return
			}
			setConv(turn.ConversationID)
			send(serverMessage{Type: "assistant.turn", Turn: &turn})

			for i := range turn.Calls {
				if turn.Calls[i].Status == "awaiting_approval" {
					c := turn.Calls[i]
					send(serverMessage{Type: "tool.permission_required", Call: &c})
				}
			}
		}()
	}

	// The writer goroutine.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ping := time.NewTicker(wsPingEvery)
		defer ping.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case m := <-outbound:
				if m.Event != nil && m.Event.Type == realtime.EventFinal {
					if n := dropped.Swap(0); n > 0 {
						// Tell the client its previews were incomplete, once,
						// when the next stable result lands.
						ev := realtime.Event{Type: realtime.EventLagged, Dropped: int(n)}
						_ = writeMessage(ctx, conn, serverMessage{Type: string(ev.Type), Event: &ev})
					}
				}
				if err := writeMessage(ctx, conn, m); err != nil {
					cancel()
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	defer func() {
		cancel()
		<-writerDone
		if sess != nil {
			sess.Close()
		}
	}()

	idle := time.Now()
	buf := make([]float32, 0, 4096)

	for {
		if time.Since(idle) > wsIdleTimeout {
			_ = conn.Close(websocket.StatusNormalClosure, "idle")
			return
		}
		readCtx, readCancel := context.WithTimeout(ctx, wsIdleTimeout)
		typ, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				observability.Logger(ctx).Debug("websocket closed", "error", err)
			}
			return
		}
		idle = time.Now()

		if typ == websocket.MessageBinary {
			if sess == nil {
				sendEvent(realtime.Event{Type: realtime.EventError, Code: "not_configured",
					Message: "send session.configure before any audio"})
				continue
			}
			buf = audio.PCM16ToFloat32(data, buf)
			sess.Write(ctx, buf)
			continue
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			sendEvent(realtime.Event{Type: realtime.EventError, Code: "bad_json", Message: err.Error()})
			continue
		}

		switch msg.Type {
		case "session.configure":
			if sess != nil {
				sess.Close()
			}
			setConv(msg.ConversationID)
			sess, err = s.newSession(msg, sendEvent, onFinal)
			if err != nil {
				sendEvent(realtime.Event{Type: realtime.EventError,
					Code: "configure_failed", Message: err.Error()})
				continue
			}
			sess.Start()

		case "session.commit":
			if sess != nil {
				sess.Commit(ctx)
			}

		case "session.cancel":
			if sess != nil {
				sess.Cancel()
			}

		case "text.input":
			// Typing must take exactly the same path as speaking, or the two
			// can diverge in behaviour and only one gets tested.
			onFinal(ctx, realtime.Event{Type: realtime.EventFinal, Text: msg.Text, Stable: true})

		case "tool.approve":
			res, err := s.deps.Assistant.Approve(ctx, msg.CallID)
			if err != nil {
				sendEvent(realtime.Event{Type: realtime.EventError,
					Code: "approve_failed", Message: err.Error()})
				continue
			}
			send(serverMessage{Type: "tool.completed", Call: &res})

		case "tool.deny":
			if err := s.deps.Assistant.Deny(ctx, msg.CallID, msg.Reason); err != nil {
				sendEvent(realtime.Event{Type: realtime.EventError,
					Code: "deny_failed", Message: err.Error()})
			}

		case "ping":
			send(serverMessage{Type: "pong"})

		default:
			sendEvent(realtime.Event{Type: realtime.EventError, Code: "unknown_message",
				Message: "unrecognised message type " + msg.Type})
		}
	}
}

func (s *Server) newSession(msg clientMessage, send func(realtime.Event), onFinal realtime.OnFinal) (*realtime.Session, error) {
	cfg := realtime.Config{
		Engine:       s.deps.Engine,
		VAD:          s.deps.VAD,
		Metrics:      s.deps.Metrics,
		Mode:         realtime.Mode(msg.Mode),
		Partials:     s.deps.Config.Partials,
		PartialEvery: s.deps.Config.PartialEvery,
		HangoverMS:   s.deps.Config.HangoverMS,
		MaxUtterance: s.deps.Config.MaxUtterance,
		Language:     msg.Language,
	}
	if msg.Partials != nil {
		cfg.Partials = *msg.Partials
	}
	if msg.HangoverMS > 0 {
		cfg.HangoverMS = msg.HangoverMS
	}
	return realtime.NewSession(cfg, send, onFinal)
}

// writeMessage serialises one event with a bounded deadline.
//
// The deadline is what makes a stalled reader recoverable: without it, a client
// that stops draining its receive window blocks this goroutine indefinitely
// while the rest of the session piles up behind it.
func writeMessage(ctx context.Context, conn *websocket.Conn, m serverMessage) error {
	payload, err := marshalMessage(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, payload)
}

// marshalMessage flattens the event into the envelope.
//
// encoding/json has no inline support for embedded pointers, so the two objects
// are merged by hand rather than nesting the event under a key the client would
// have to reach through.
func marshalMessage(m serverMessage) ([]byte, error) {
	out := map[string]any{"type": m.Type}
	if m.Event != nil {
		raw, err := json.Marshal(m.Event)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		for k, v := range fields {
			if k == "type" {
				continue
			}
			out[k] = v
		}
	}
	if m.Turn != nil {
		out["turn"] = m.Turn
	}
	if m.Call != nil {
		out["call"] = m.Call
	}
	for k, v := range m.Extra {
		out[k] = v
	}
	return json.Marshal(out)
}
