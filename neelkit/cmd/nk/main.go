// Command nk is the NeelKIT command-line client.
//
// It talks to the server over HTTP and nothing else. It does not import the
// assistant, the tool registry or the database, which is the point: if nk can
// do something, the API can do it, because nk has no other way in. A CLI that
// reaches into internal packages proves nothing about the API's completeness.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version"
)

const usage = `nk — NeelKIT command line

Usage:
  nk ask <text>            Send a command; anything risky asks before running
  nk plan <text>           Show what would happen, without running it
  nk transcribe <file>     Transcribe an audio file
  nk history [query]       List conversations, or search everything you said
  nk tools                 List what the assistant can do
  nk audit [hours]         Show what has run on this machine
  nk doctor                Check that the server is reachable and ready
  nk version               Print version information

Environment:
  NEELKIT_URL       Server URL (default http://127.0.0.1:8420)
  NEELKIT_API_KEY   API key printed by neelkitd on first run
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error

	switch cmd {
	case "ask":
		err = ask(args, true)
	case "plan":
		err = ask(args, false)
	case "transcribe":
		err = transcribe(args)
	case "history":
		err = history(args)
	case "tools":
		err = listTools()
	case "audit":
		err = audit(args)
	case "doctor":
		err = doctor(args)
	case "version":
		v := version.Get()
		fmt.Printf("nk %s (%s, built %s, %s)\n", v.Version, v.Commit, v.BuildDate, v.GoVersion)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "nk: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "nk:", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------------ client

func baseURL() string {
	if u := os.Getenv("NEELKIT_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:8420"
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func call(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, baseURL()+path, reader)
	if err != nil {
		return err
	}
	if key := os.Getenv("NEELKIT_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s — is neelkitd running?", baseURL())
	}
	defer res.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 400 {
		return apiError(res.StatusCode, payload)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, out)
}

func apiError(status int, payload []byte) error {
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && body.Error.Message != "" {
		if status == http.StatusUnauthorized {
			return fmt.Errorf("%s\n\nSet NEELKIT_API_KEY to the key neelkitd printed on first run",
				body.Error.Message)
		}
		return fmt.Errorf("%s (%s, request %s)", body.Error.Message, body.Error.Code, body.Error.RequestID)
	}
	return fmt.Errorf("server returned %d: %s", status, strings.TrimSpace(string(payload)))
}

// -------------------------------------------------------------------- types

type callResult struct {
	CallID   string         `json:"call_id"`
	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	Risk     string         `json:"risk"`
	Decision string         `json:"decision"`
	Reason   string         `json:"reason"`
	Preview  string         `json:"preview"`
	Status   string         `json:"status"`
	Summary  string         `json:"summary"`
	Stdout   string         `json:"stdout"`
	Error    string         `json:"error"`
	Duration int64          `json:"duration_ms"`
}

type turn struct {
	TurnID         string `json:"turn_id"`
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
	Intent         struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"intent"`
	Calls   []callResult `json:"calls"`
	Status  string       `json:"status"`
	Message string       `json:"message"`
	Latency int64        `json:"latency_ms"`
}

// ---------------------------------------------------------------- commands

func ask(args []string, execute bool) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "approve every prompt without asking (use with care)")
	jsonOut := fs.Bool("json", false, "print the raw turn as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to say; try: nk ask \"run the tests\"")
	}

	var t turn
	if err := call(http.MethodPost, "/v1/assistant/turns",
		map[string]any{"text": text, "execute": execute}, &t); err != nil {
		return err
	}

	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(t)
	}

	fmt.Printf("  %s  %s  (%dms)\n", dim(t.Intent.Source), t.Intent.Name, t.Latency)
	if t.Message != "" {
		fmt.Printf("\n%s\n", t.Message)
	}

	for _, c := range t.Calls {
		fmt.Println()
		printCall(c)

		if c.Status != "awaiting_approval" {
			continue
		}
		approve := *yes
		if !approve {
			var err error
			approve, err = confirm("  Run this?")
			if err != nil {
				return err
			}
		}

		if !approve {
			_ = call(http.MethodPost, "/v1/assistant/approvals",
				map[string]any{"call_id": c.CallID, "decision": "deny"}, nil)
			fmt.Println("  denied.")
			continue
		}

		var done callResult
		if err := call(http.MethodPost, "/v1/assistant/approvals",
			map[string]any{"call_id": c.CallID, "decision": "approve"}, &done); err != nil {
			return err
		}
		fmt.Println()
		printCall(done)
	}
	return nil
}

func printCall(c callResult) {
	marker := "·"
	switch c.Status {
	case "succeeded":
		marker = "✓"
	case "failed", "denied":
		marker = "✗"
	case "awaiting_approval":
		marker = "?"
	}

	fmt.Printf("  %s %s  %s\n", marker, c.Tool, dim(c.Risk))
	if c.Preview != "" {
		fmt.Printf("    %s\n", c.Preview)
	}
	if c.Status == "awaiting_approval" && c.Reason != "" {
		fmt.Printf("    %s\n", dim(c.Reason))
	}
	if c.Summary != "" {
		fmt.Printf("    %s\n", c.Summary)
	}
	if c.Error != "" {
		fmt.Printf("    %s\n", c.Error)
	}
	if c.Stdout != "" {
		for _, line := range strings.Split(strings.TrimRight(c.Stdout, "\n"), "\n") {
			fmt.Printf("    │ %s\n", line)
		}
	}
}

func confirm(prompt string) (bool, error) {
	// A non-interactive shell must never be treated as consent: piping into nk
	// would silently approve everything.
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("  not a terminal; skipping (pass --yes to approve)")
		return false, nil
	}
	fmt.Printf("%s [y/N] ", prompt)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func transcribe(args []string) error {
	fs := flag.NewFlagSet("transcribe", flag.ContinueOnError)
	words := fs.Bool("words", false, "include per-word timings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: nk transcribe <file>")
	}
	path := fs.Arg(0)

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	_ = mw.WriteField("confidence", "true")
	if *words {
		_ = mw.WriteField("timestamps", "word")
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, baseURL()+"/v1/audio/transcriptions", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key := os.Getenv("NEELKIT_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s — is neelkitd running?", baseURL())
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return apiError(res.StatusCode, payload)
	}

	var out struct {
		Text       string  `json:"text"`
		Duration   float64 `json:"duration"`
		Confidence float64 `json:"confidence"`
		Words      []struct {
			Word  string  `json:"word"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"words"`
		Metrics struct {
			RTF         float64 `json:"rtf"`
			InferenceMS int64   `json:"inference_ms"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return err
	}

	fmt.Println(out.Text)
	if *words {
		fmt.Println()
		for _, w := range out.Words {
			fmt.Printf("  %6.2f  %6.2f  %s\n", w.Start, w.End, w.Word)
		}
	}
	fmt.Fprintf(os.Stderr, "\n  %.1fs audio · %d ms inference · %.2fx real time · %.0f%% confident\n",
		out.Duration, out.Metrics.InferenceMS, out.Metrics.RTF, out.Confidence*100)
	return nil
}

func history(args []string) error {
	if len(args) > 0 {
		query := strings.Join(args, " ")
		var out struct {
			Results []struct {
				Text      string `json:"text"`
				Snippet   string `json:"snippet"`
				CreatedAt int64  `json:"created_at"`
			} `json:"results"`
		}
		if err := call(http.MethodGet, "/v1/transcripts/search?q="+urlEncode(query), nil, &out); err != nil {
			return err
		}
		if len(out.Results) == 0 {
			fmt.Println("nothing matched")
			return nil
		}
		for _, r := range out.Results {
			fmt.Printf("  %s  %s\n", dim(when(r.CreatedAt)), strings.ReplaceAll(r.Snippet, "\n", " "))
		}
		return nil
	}

	var out struct {
		Conversations []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			UpdatedAt int64  `json:"updated_at"`
			Count     int    `json:"message_count"`
		} `json:"conversations"`
	}
	if err := call(http.MethodGet, "/v1/conversations", nil, &out); err != nil {
		return err
	}
	if len(out.Conversations) == 0 {
		fmt.Println("no conversations yet")
		return nil
	}
	for _, c := range out.Conversations {
		fmt.Printf("  %s  %-40s %s\n", dim(when(c.UpdatedAt)), truncate(c.Title, 40),
			dim(fmt.Sprintf("%d messages", c.Count)))
	}
	return nil
}

func listTools() error {
	var out struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Risk        string `json:"risk"`
		} `json:"tools"`
		Workspace string `json:"workspace"`
		ReadOnly  bool   `json:"read_only"`
	}
	if err := call(http.MethodGet, "/v1/tools", nil, &out); err != nil {
		return err
	}

	fmt.Printf("  workspace %s%s\n\n", out.Workspace, map[bool]string{true: " (read-only)"}[out.ReadOnly])
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	for _, t := range out.Tools {
		fmt.Printf("  %-22s %-9s %s\n", t.Name, dim(t.Risk), truncate(t.Description, 60))
	}
	return nil
}

func audit(args []string) error {
	hours := "24"
	if len(args) > 0 {
		hours = args[0]
	}
	var out struct {
		Events []struct {
			Action    string `json:"action"`
			Subject   string `json:"subject"`
			Risk      string `json:"risk"`
			Outcome   string `json:"outcome"`
			CreatedAt int64  `json:"created_at"`
		} `json:"events"`
	}
	if err := call(http.MethodGet, "/v1/audit?hours="+hours, nil, &out); err != nil {
		return err
	}
	if len(out.Events) == 0 {
		fmt.Printf("nothing in the last %s hours\n", hours)
		return nil
	}
	for _, e := range out.Events {
		fmt.Printf("  %s  %-14s %-22s %-8s %s\n",
			dim(when(e.CreatedAt)), e.Action, e.Subject, e.Risk, e.Outcome)
	}
	return nil
}

// doctor is what the container healthcheck runs, and what a user runs first
// when something is wrong. It answers one question per line.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "exit status only, for healthchecks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var ready struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := call(http.MethodGet, "/readyz", nil, &ready); err != nil {
		if *quiet {
			os.Exit(1)
		}
		return err
	}

	if *quiet {
		if !ready.Ready {
			os.Exit(1)
		}
		return nil
	}

	fmt.Printf("  server    %s\n", baseURL())
	keys := make([]string, 0, len(ready.Checks))
	for k := range ready.Checks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mark := "✓"
		if ready.Checks[k] != "ok" && !strings.HasPrefix(ready.Checks[k], "disabled") {
			mark = "✗"
		}
		fmt.Printf("  %s %-9s %s\n", mark, k, ready.Checks[k])
	}

	if os.Getenv("NEELKIT_API_KEY") == "" {
		fmt.Println("\n  NEELKIT_API_KEY is not set; authenticated commands will fail")
	}
	if !ready.Ready {
		return errors.New("the server is not ready")
	}
	return nil
}

// ---------------------------------------------------------------- helpers

func dim(s string) string {
	// Only colour a real terminal: piping into a file or a log should not
	// collect escape sequences.
	if os.Getenv("NO_COLOR") != "" {
		return s
	}
	if info, err := os.Stdout.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

func when(ms int64) string {
	return time.UnixMilli(ms).Format("02 Jan 15:04")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func urlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
