// Package assistant orchestrates one turn: text in, actions and a reply out.
//
// A turn is a small state machine. Normalise the transcript, resolve an intent,
// plan each proposed tool call, gate it, run what is allowed, suspend on what
// needs a human, and compose a reply. Keeping that sequence in one place is
// what makes it identical whether the input arrived by voice, by the REST API
// or from the CLI.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/id"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/intent"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/llm"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/normalize"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/policy"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/runner"
)

// Errors callers distinguish.
var (
	// ErrNoSuchCall means the approval refers to a call that does not exist or
	// has already been resolved.
	ErrNoSuchCall = errors.New("assistant: unknown or already-resolved call")
	// ErrApprovalExpired means the user took too long.
	ErrApprovalExpired = errors.New("assistant: approval window expired")
)

// Status is where a turn ended up.
type Status string

const (
	StatusExecuted         Status = "executed"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusDenied           Status = "denied"
	StatusFailed           Status = "failed"
	StatusChat             Status = "chat"
)

// CallResult is one tool call's outcome within a turn.
type CallResult struct {
	CallID   string          `json:"call_id"`
	Tool     string          `json:"tool"`
	Args     map[string]any  `json:"args"`
	Risk     tools.RiskLevel `json:"risk"`
	Decision policy.Decision `json:"decision"`
	Reason   string          `json:"reason"`
	Preview  string          `json:"preview"`
	Status   string          `json:"status"`
	Summary  string          `json:"summary,omitempty"`
	Stdout   string          `json:"stdout,omitempty"`
	Data     any             `json:"data,omitempty"`
	Error    string          `json:"error,omitempty"`
	Duration int64           `json:"duration_ms"`
}

// Turn is the result of processing one utterance.
type Turn struct {
	TurnID         string        `json:"turn_id"`
	ConversationID string        `json:"conversation_id"`
	Text           string        `json:"text"`
	RawText        string        `json:"raw_text"`
	Intent         intent.Intent `json:"intent"`
	Calls          []CallResult  `json:"calls"`
	Status         Status        `json:"status"`
	Message        string        `json:"message"`
	Latency        int64         `json:"latency_ms"`
}

// MarshalJSON always writes calls as a list.
//
// A turn that runs no tool — "hello", or anything the model only replies to —
// has a nil slice, which encoding/json writes as null. The web client is
// entitled to treat a field documented as an array as an array, and when it
// called .reduce on null the whole page went down. Fixing it here, once,
// covers every path that produces a turn.
func (t Turn) MarshalJSON() ([]byte, error) {
	type plain Turn // drops this method, so the call below does not recurse
	if t.Calls == nil {
		t.Calls = []CallResult{}
	}
	return json.Marshal(plain(t))
}

// Config configures an Assistant.
type Config struct {
	Router  *intent.Router
	Runner  *runner.Runner
	DB      *storage.DB
	Metrics *observability.Metrics
	UserID  string
	// ApprovalTTL is how long a suspended call waits before it is treated as
	// denied. An approval prompt that never expires is a call that can fire
	// hours later, long after the user has forgotten the context.
	ApprovalTTL time.Duration
	// CustomVocabulary augments the normaliser with project-specific words.
	CustomVocabulary map[string]string
}

// Assistant processes turns.
type Assistant struct {
	cfg Config

	mu      sync.Mutex
	pending map[string]*pendingCall
}

type pendingCall struct {
	plan    runner.Plan
	turnID  string
	convID  string
	expires time.Time
}

// New returns an assistant.
func New(cfg Config) *Assistant {
	if cfg.ApprovalTTL <= 0 {
		cfg.ApprovalTTL = 2 * time.Minute
	}
	return &Assistant{cfg: cfg, pending: map[string]*pendingCall{}}
}

// Input is one utterance to process.
type Input struct {
	ConversationID string
	Text           string
	// Source is "voice" or "text"; it is recorded, not branched on, because
	// the pipeline must behave identically either way.
	Source string
	// TranscriptID links this turn to the stored recognition result.
	TranscriptID string
	// DryRun plans and gates without running anything.
	DryRun bool
}

// Process runs one turn.
func (a *Assistant) Process(ctx context.Context, in Input) (Turn, error) {
	start := time.Now()
	log := observability.Logger(ctx)

	raw := strings.TrimSpace(in.Text)
	opts := normalize.CommandOptions()
	opts.Custom = a.cfg.CustomVocabulary
	text := normalize.Text(raw, opts)

	t := Turn{
		TurnID:         id.New(id.PrefixTurn),
		ConversationID: in.ConversationID,
		Text:           text,
		RawText:        raw,
		Status:         StatusChat,
	}
	if text == "" {
		t.Message = "I didn't catch that."
		t.Latency = time.Since(start).Milliseconds()
		return t, nil
	}

	if err := a.ensureConversation(ctx, &t, text); err != nil {
		return t, err
	}
	a.record(ctx, t.ConversationID, t.TurnID, "user", text, in.Source, in.TranscriptID)

	history := a.history(ctx, t.ConversationID)

	res, err := a.cfg.Router.Route(ctx, text, history)
	if err != nil {
		t.Status = StatusFailed
		t.Message = "I couldn't work out what to do with that."
		log.Error("intent routing failed", "error", err)
		t.Latency = time.Since(start).Milliseconds()
		return t, err
	}
	t.Intent = res
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.IntentResolved.Inc(string(res.Source), res.Name)
	}

	if len(res.Calls) == 0 {
		t.Message = res.Reply
		if t.Message == "" {
			t.Message = "I'm not sure what to do with that."
		}
		a.record(ctx, t.ConversationID, t.TurnID, "assistant", t.Message, "text", "")
		t.Latency = time.Since(start).Milliseconds()
		return t, nil
	}

	for _, c := range res.Calls {
		cr := a.planAndRun(ctx, &t, c, in.DryRun)
		t.Calls = append(t.Calls, cr)
	}

	t.Status = statusFor(t.Calls, in.DryRun)
	t.Message = compose(res, t.Calls)
	if t.Message != "" {
		a.record(ctx, t.ConversationID, t.TurnID, "assistant", t.Message, "text", "")
	}
	t.Latency = time.Since(start).Milliseconds()
	return t, nil
}

// planAndRun gates one proposed call and executes it when allowed.
func (a *Assistant) planAndRun(ctx context.Context, t *Turn, c intent.Call, dryRun bool) CallResult {
	callID := id.New(id.PrefixCall)

	plan, err := a.cfg.Runner.Plan(ctx, runner.Call{ID: callID, Tool: c.Tool, Args: c.Args})
	if err != nil {
		return CallResult{
			CallID: callID, Tool: c.Tool, Args: c.Args,
			Status: "failed", Error: err.Error(),
		}
	}

	cr := CallResult{
		CallID:   callID,
		Tool:     plan.Tool,
		Args:     plan.Args,
		Risk:     plan.Risk,
		Decision: plan.Decision.Decision,
		Reason:   plan.Decision.Reason,
		Preview:  plan.Preview,
	}
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.ToolDecision.Inc(plan.Tool, string(plan.Risk), string(plan.Decision.Decision))
	}
	a.audit(ctx, "tool.plan", plan, "planned")

	switch {
	case plan.Denied():
		cr.Status = "denied"
		a.audit(ctx, "tool.deny", plan, "denied")
		return cr
	case dryRun:
		cr.Status = "planned"
		return cr
	case plan.NeedsApproval():
		cr.Status = "awaiting_approval"
		a.suspend(plan, t.TurnID, t.ConversationID)
		a.persistPending(ctx, t.TurnID, plan)
		return cr
	}

	return a.execute(ctx, t.TurnID, plan, cr)
}

// execute runs an approved plan and records the outcome.
func (a *Assistant) execute(ctx context.Context, turnID string, plan runner.Plan, cr CallResult) CallResult {
	start := time.Now()
	a.persistStart(ctx, turnID, plan)

	res, err := a.cfg.Runner.Execute(ctx, plan)
	cr.Duration = time.Since(start).Milliseconds()

	if err != nil {
		cr.Status = "failed"
		cr.Error = err.Error()
		a.audit(ctx, "tool.invoke", plan, "failed")
		a.persistFinish(ctx, plan, cr)
		return cr
	}

	cr.Status = "succeeded"
	cr.Summary = res.Summary
	cr.Stdout = res.Stdout
	cr.Data = res.Data
	a.audit(ctx, "tool.invoke", plan, "allowed")
	a.persistFinish(ctx, plan, cr)

	// Tool output re-enters the conversation as a tool-role message. It is
	// data the model may reason about, never instructions it may follow; the
	// provider delimits and labels it on the way in.
	if res.Summary != "" {
		a.record(ctx, "", turnID, "tool", plan.Tool+": "+res.Summary, "tool", "")
	}
	return cr
}

// Approve runs a suspended call.
func (a *Assistant) Approve(ctx context.Context, callID string) (CallResult, error) {
	a.mu.Lock()
	p, ok := a.pending[callID]
	if ok {
		delete(a.pending, callID)
	}
	a.mu.Unlock()

	if !ok {
		return CallResult{}, ErrNoSuchCall
	}
	if time.Now().After(p.expires) {
		a.audit(ctx, "tool.expire", p.plan, "denied")
		return CallResult{}, ErrApprovalExpired
	}

	base := CallResult{
		CallID: callID, Tool: p.plan.Tool, Args: p.plan.Args,
		Risk: p.plan.Risk, Decision: policy.Allow,
		Reason: "approved by you", Preview: p.plan.Preview,
	}
	return a.execute(ctx, p.turnID, runner.Approve(p.plan), base), nil
}

// Deny discards a suspended call.
func (a *Assistant) Deny(ctx context.Context, callID, reason string) error {
	a.mu.Lock()
	p, ok := a.pending[callID]
	if ok {
		delete(a.pending, callID)
	}
	a.mu.Unlock()
	if !ok {
		return ErrNoSuchCall
	}
	a.audit(ctx, "tool.deny", p.plan, "denied")

	cr := CallResult{CallID: callID, Tool: p.plan.Tool, Status: "denied", Error: reason}
	a.persistFinish(ctx, p.plan, cr)
	return nil
}

// Pending returns the calls currently awaiting approval, dropping any that
// have expired. Called by the API and by the session loop.
func (a *Assistant) Pending() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	var out []string
	for id, p := range a.pending {
		if now.After(p.expires) {
			delete(a.pending, id)
			continue
		}
		out = append(out, id)
	}
	return out
}

func (a *Assistant) suspend(plan runner.Plan, turnID, convID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[plan.ID] = &pendingCall{
		plan: plan, turnID: turnID, convID: convID,
		expires: time.Now().Add(a.cfg.ApprovalTTL),
	}
}

// ------------------------------------------------------------ persistence

func (a *Assistant) ensureConversation(ctx context.Context, t *Turn, title string) error {
	if a.cfg.DB == nil {
		if t.ConversationID == "" {
			t.ConversationID = id.New(id.PrefixConversation)
		}
		return nil
	}
	if t.ConversationID == "" {
		c, err := a.cfg.DB.CreateConversation(ctx, a.cfg.UserID, truncate(title, 80))
		if err != nil {
			return fmt.Errorf("create conversation: %w", err)
		}
		t.ConversationID = c.ID
		return nil
	}
	return a.cfg.DB.TouchConversation(ctx, t.ConversationID, truncate(title, 80))
}

func (a *Assistant) record(ctx context.Context, convID, turnID, role, content, source, transcriptID string) {
	if a.cfg.DB == nil || content == "" {
		return
	}
	if convID == "" {
		return
	}
	if _, err := a.cfg.DB.AddMessage(ctx, storage.Message{
		ConversationID: convID, TurnID: turnID, Role: role,
		Content: content, Source: source, TranscriptID: transcriptID,
	}); err != nil {
		observability.Logger(ctx).Warn("could not record message", "error", err, "role", role)
	}
}

func (a *Assistant) history(ctx context.Context, convID string) []llm.Message {
	if a.cfg.DB == nil || convID == "" {
		return nil
	}
	msgs, err := a.cfg.DB.ListMessages(ctx, convID, 40)
	if err != nil {
		return nil
	}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		var role llm.Role
		switch m.Role {
		case "user":
			role = llm.RoleUser
		case "assistant":
			role = llm.RoleAssistant
		case "tool":
			role = llm.RoleTool
		default:
			continue
		}
		out = append(out, llm.Message{Role: role, Content: m.Content})
	}
	return out
}

func (a *Assistant) persistPending(ctx context.Context, turnID string, p runner.Plan) {
	a.persistExec(ctx, turnID, p, "awaiting_approval")
}

func (a *Assistant) persistStart(ctx context.Context, turnID string, p runner.Plan) {
	a.persistExec(ctx, turnID, p, "running")
}

func (a *Assistant) persistExec(ctx context.Context, turnID string, p runner.Plan, status string) {
	if a.cfg.DB == nil {
		return
	}
	args, _ := json.Marshal(p.Args)
	err := a.cfg.DB.StartToolExecution(ctx, storage.ToolExecution{
		ID: p.ID, TurnID: turnID, UserID: a.cfg.UserID, Tool: p.Tool,
		ArgsJSON: string(args), Risk: string(p.Risk),
		Decision: string(p.Decision.Decision), Preview: p.Preview, Status: status,
	})
	if err != nil {
		observability.Logger(ctx).Warn("could not record tool execution", "error", err, "tool", p.Tool)
	}
}

func (a *Assistant) persistFinish(ctx context.Context, p runner.Plan, cr CallResult) {
	if a.cfg.DB == nil {
		return
	}
	_ = a.cfg.DB.FinishToolExecution(ctx, storage.ToolExecution{
		ID: p.ID, Status: cr.Status, Summary: cr.Summary,
		Stdout: truncate(cr.Stdout, 8000), Error: cr.Error, DurationMS: cr.Duration,
	})
}

func (a *Assistant) audit(ctx context.Context, action string, p runner.Plan, outcome string) {
	if a.cfg.DB == nil {
		return
	}
	detail, _ := json.Marshal(map[string]any{
		"args": p.Args, "preview": p.Preview, "reason": p.Decision.Reason,
	})
	_ = a.cfg.DB.Audit(ctx, storage.AuditEvent{
		UserID: a.cfg.UserID, RequestID: observability.RequestID(ctx),
		Actor: "assistant", Action: action, Subject: p.Tool,
		Risk: string(p.Risk), Outcome: outcome, DetailJSON: string(detail),
	})
}

// ---------------------------------------------------------------- helpers

func statusFor(calls []CallResult, dryRun bool) Status {
	if dryRun {
		return StatusAwaitingApproval
	}
	var awaiting, denied, failed, ok int
	for _, c := range calls {
		switch c.Status {
		case "awaiting_approval":
			awaiting++
		case "denied":
			denied++
		case "failed":
			failed++
		case "succeeded":
			ok++
		}
	}
	switch {
	case awaiting > 0:
		return StatusAwaitingApproval
	case failed > 0:
		return StatusFailed
	case denied > 0 && ok == 0:
		return StatusDenied
	default:
		return StatusExecuted
	}
}

// compose builds the spoken reply.
//
// It is assembled from the tool summaries rather than asking the model to
// narrate, because the summaries are facts and a second model round trip would
// add latency to say something the system already knows.
func compose(in intent.Intent, calls []CallResult) string {
	var parts []string
	for _, c := range calls {
		switch c.Status {
		case "succeeded":
			if c.Summary != "" {
				parts = append(parts, c.Summary)
			}
		case "awaiting_approval":
			// The preview is shown verbatim. Lower-casing it or cutting at the
			// first period mangles file names ("NOTES.md" became "notes"),
			// which is exactly the kind of quiet distortion an approval prompt
			// must never introduce.
			parts = append(parts, "Ready to run: "+c.Preview+". Approve when you're happy.")
		case "denied":
			parts = append(parts, "I can't do that: "+c.Reason+".")
		case "failed":
			parts = append(parts, "That failed: "+c.Error)
		case "planned":
			parts = append(parts, "Would run: "+c.Preview)
		}
	}
	if len(parts) == 0 {
		return in.Reply
	}
	msg := strings.Join(parts, " ")
	if in.Reply != "" && in.Source == intent.SourceLLM {
		msg = in.Reply + " " + msg
	}
	return strings.TrimSpace(msg)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
