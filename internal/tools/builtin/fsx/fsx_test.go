package fsx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/sandbox"
)

func newSandbox(t *testing.T) (tools.Sandbox, string) {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"a.txt", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x\ny\nz"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "src"), 0o755)

	sb, err := sandbox.New(sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb, root
}

func run(t *testing.T, tool tools.Tool, sb tools.Sandbox, args string) tools.Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), tools.Input{
		Args: json.RawMessage(args), Sandbox: sb,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

// The summary is the sentence read aloud, so it has to be a sentence. Naive
// formatting of the path "." produced "2 entries in ..".
func TestListSummaryReadsAsASentence(t *testing.T) {
	sb, _ := newSandbox(t)
	res := run(t, listDir{}, sb, `{"path":"."}`)

	if strings.Contains(res.Summary, "..") {
		t.Fatalf("summary reads badly: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "project root") {
		t.Fatalf("summary = %q, want it to name the root in words", res.Summary)
	}
}

// "1 entries in src" is the kind of detail that is invisible on a screen and
// glaring when a voice says it out loud.
func TestListSummaryAgreesInNumber(t *testing.T) {
	sb, root := newSandbox(t)
	if err := os.WriteFile(filepath.Join(root, "src", "only.go"), []byte("package src"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, listDir{}, sb, `{"path":"src"}`)
	if !strings.Contains(res.Summary, "1 entry in") {
		t.Errorf("summary = %q, want the singular", res.Summary)
	}

	res = run(t, listDir{}, sb, `{"path":"."}`)
	if !strings.Contains(res.Summary, "entries in") {
		t.Errorf("summary = %q, want the plural", res.Summary)
	}
}

// Generated directories are skipped, and the user is told how many, rather than
// the listing silently differing from what they would see in a file manager.
func TestListSkipsGeneratedDirectoriesAndSaysSo(t *testing.T) {
	sb, _ := newSandbox(t)
	res := run(t, listDir{}, sb, `{"path":"."}`)

	data := res.Data.(map[string]any)
	raw, _ := json.Marshal(data["entries"])
	if strings.Contains(string(raw), "node_modules") {
		t.Error("node_modules appeared in the listing")
	}
	if !strings.Contains(res.Summary, "skipped") {
		t.Errorf("summary does not mention what was hidden: %q", res.Summary)
	}
}

func TestReadReportsLineCountAndTruncates(t *testing.T) {
	sb, _ := newSandbox(t)

	res := run(t, readFile{}, sb, `{"path":"a.txt"}`)
	data := res.Data.(map[string]any)
	if data["lines"] != 3 {
		t.Errorf("lines = %v, want 3", data["lines"])
	}
	if data["truncated"] != false {
		t.Error("a short file was reported as truncated")
	}

	res = run(t, readFile{}, sb, `{"path":"a.txt","max_lines":2}`)
	data = res.Data.(map[string]any)
	if data["truncated"] != true {
		t.Error("a truncated read did not say so")
	}
}

func TestWriteReportsWhetherItOverwrote(t *testing.T) {
	sb, _ := newSandbox(t)

	res := run(t, writeFile{}, sb, `{"path":"new.txt","content":"hello"}`)
	if !strings.HasPrefix(res.Summary, "Created") {
		t.Errorf("summary = %q, want it to say the file was created", res.Summary)
	}
	res = run(t, writeFile{}, sb, `{"path":"new.txt","content":"again"}`)
	if !strings.HasPrefix(res.Summary, "Overwrote") {
		t.Errorf("summary = %q, want it to warn that a file was overwritten", res.Summary)
	}
}

func TestToolsRefuseArgumentsOutsideTheWorkspace(t *testing.T) {
	sb, _ := newSandbox(t)
	for _, args := range []string{`{"path":"../escape.txt"}`, `{"path":"/etc/passwd"}`} {
		if _, err := (readFile{}).Execute(context.Background(),
			tools.Input{Args: json.RawMessage(args), Sandbox: sb}); err == nil {
			t.Errorf("read of %s succeeded", args)
		}
	}
}
