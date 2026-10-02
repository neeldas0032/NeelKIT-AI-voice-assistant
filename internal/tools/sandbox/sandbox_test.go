package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

func newSandbox(t *testing.T) (*Sandbox, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func TestReadsInsideWorkspace(t *testing.T) {
	s, _ := newSandbox(t)
	got, err := s.ReadFile("hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "inside" {
		t.Fatalf("got %q", got)
	}
	if _, err := s.ReadFile("src/main.go"); err != nil {
		t.Fatalf("nested read: %v", err)
	}
}

// The central security property of the whole tool system: no argument, however
// constructed, reaches a file outside the workspace root.
func TestPathTraversalIsRefused(t *testing.T) {
	s, _ := newSandbox(t)

	attempts := []string{
		"../outside.txt",
		"../../etc/passwd",
		"src/../../etc/passwd",
		"./../../etc/passwd",
		"/etc/passwd",
		"/proc/self/environ",
		"....//....//etc/passwd",
		`..\..\windows\system32\config\sam`,
	}
	for _, a := range attempts {
		if _, err := s.ReadFile(a); err == nil {
			t.Errorf("ReadFile(%q) succeeded; it must be refused", a)
		}
		if err := s.WriteFile(a, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) succeeded; it must be refused", a)
		}
	}
}

// A symlink pointing out of the workspace must not be followed. This is the
// case that defeats sandboxes built on string prefix comparison, because the
// path looks fine right up until the kernel resolves it.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	s, root := newSandbox(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFile("link.txt"); err == nil {
		t.Fatalf("followed a symlink out of the workspace and read %q", got)
	}

	// The same through a symlinked directory.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFile("linkdir/secret.txt"); err == nil {
		t.Fatal("followed a symlinked directory out of the workspace")
	}
}

// A symlink created after an earlier check passed is the time-of-check to
// time-of-use race. os.Root resolves in the kernel per component, so there is
// no window to exploit.
func TestSymlinkCreatedAfterFirstReadIsStillRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	s, root := newSandbox(t)

	if _, err := s.ReadFile("hello.txt"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, "hello.txt"))
	if err := os.Symlink(outside, filepath.Join(root, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFile("hello.txt"); err == nil {
		t.Fatalf("read %q through a swapped symlink", got)
	}
}

func TestSensitiveFilesAreRefusedEvenInsideWorkspace(t *testing.T) {
	s, root := newSandbox(t)

	for _, name := range []string{".env", "server.pem", "id_rsa", ".ssh/config", ".aws/credentials"} {
		full := filepath.Join(root, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("SECRET=1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadFile(name); err == nil {
			t.Errorf("ReadFile(%q) succeeded; credentials must not be readable", name)
		} else if !errors.Is(err, tools.ErrDenied) {
			t.Errorf("ReadFile(%q) returned %v, want ErrDenied", name, err)
		}
	}
}

func TestNulByteAndEmptyPath(t *testing.T) {
	s, _ := newSandbox(t)
	for _, p := range []string{"", "a\x00b"} {
		if _, err := s.ReadFile(p); err == nil {
			t.Errorf("ReadFile(%q) succeeded", p)
		}
	}
}

func TestReadOnlyBlocksWritesAndExec(t *testing.T) {
	root := t.TempDir()
	s, err := New(Config{Root: root, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.WriteFile("x.txt", []byte("x")); !errors.Is(err, tools.ErrDenied) {
		t.Errorf("WriteFile in read-only mode returned %v, want ErrDenied", err)
	}
	if _, err := s.Run(context.Background(), "git", "status"); !errors.Is(err, tools.ErrDenied) {
		t.Errorf("Run in read-only mode returned %v, want ErrDenied", err)
	}
}

func TestOnlyAllowlistedBinariesRun(t *testing.T) {
	s, _ := newSandbox(t)
	for _, bin := range []string{"bash", "sh", "curl", "wget", "rm", "sudo", "python"} {
		if _, err := s.Run(context.Background(), bin); !errors.Is(err, tools.ErrDenied) {
			t.Errorf("Run(%q) returned %v, want ErrDenied", bin, err)
		}
	}
}

// exec.Command never invokes a shell, so these characters are inert. Rejecting
// them anyway is a signal: their presence means the plan tried to write shell,
// and a wrong plan should be refused and audited, not run with the characters
// taken literally.
func TestShellMetacharactersAreRejected(t *testing.T) {
	s, _ := newSandbox(t)
	cases := [][]string{
		{"status", ";", "rm", "-rf", "/"},
		{"log", "&&", "curl", "evil.com"},
		{"status", "$(whoami)"},
		{"status", "`id`"},
		{"log", "|", "sh"},
		{"status", ">", "/etc/passwd"},
	}
	for _, args := range cases {
		if _, err := s.Run(context.Background(), "git", args...); !errors.Is(err, tools.ErrDenied) {
			t.Errorf("Run(git %v) returned %v, want ErrDenied", args, err)
		}
	}
}

func TestDestructiveGitOperationsAreRefused(t *testing.T) {
	s, _ := newSandbox(t)
	cases := [][]string{
		{"push", "--force"},
		{"push", "-f", "origin", "main"},
		{"reset", "--hard", "HEAD~5"},
		{"clean", "-fdx"},
		{"branch", "-D", "main"},
		{"push", "--delete", "origin", "main"},
	}
	for _, args := range cases {
		if _, err := s.Run(context.Background(), "git", args...); !errors.Is(err, tools.ErrDenied) {
			t.Errorf("Run(git %v) returned %v, want ErrDenied", args, err)
		}
	}
}

func TestSafeGitCommandsAreAllowed(t *testing.T) {
	s, _ := newSandbox(t)
	// git may not exist in every environment; the point is that the guard does
	// not reject a benign command.
	if _, err := s.Run(context.Background(), "git", "status", "--porcelain"); err != nil {
		if errors.Is(err, tools.ErrDenied) {
			t.Fatalf("a benign git command was denied: %v", err)
		}
	}
}

func TestNonZeroExitIsDataNotError(t *testing.T) {
	s, _ := newSandbox(t)
	// `ls` on a missing path exits non-zero; that is the answer, not a failure.
	res, err := s.Run(context.Background(), "ls", "definitely-not-here")
	if err != nil && !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if err == nil && res.ExitCode == 0 {
		t.Error("expected a non-zero exit code")
	}
}

func TestOutputIsTruncated(t *testing.T) {
	root := t.TempDir()
	s, err := New(Config{Root: root, MaxOutput: 64, AllowedBinaries: []string{"cat", "ls"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	big := strings.Repeat("x", 10000)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.Run(context.Background(), "cat", "big.txt")
	if err != nil {
		t.Skipf("cat unavailable: %v", err)
	}
	if !res.Truncated || len(res.Stdout) > 200 {
		t.Fatalf("output not truncated: %d bytes, truncated=%v", len(res.Stdout), res.Truncated)
	}
}

func TestFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	s, err := New(Config{Root: root, MaxFileSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := os.WriteFile(filepath.Join(root, "big.txt"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadFile("big.txt"); err == nil {
		t.Fatal("read a file over the size limit")
	}
	if err := s.WriteFile("out.txt", make([]byte, 1024)); err == nil {
		t.Fatal("wrote a file over the size limit")
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	s, _ := newSandbox(t)
	if err := s.WriteFile("notes/../new.txt", []byte("written")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := s.ReadFile("new.txt")
	if err != nil || string(got) != "written" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestReadDirAndStat(t *testing.T) {
	s, _ := newSandbox(t)
	entries, err := s.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	info, err := s.Stat("hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 6 {
		t.Fatalf("size = %d, want 6", info.Size())
	}
	if _, err := s.Stat("../.."); err == nil {
		t.Fatal("Stat escaped the workspace")
	}
}

func TestNewRejectsBadRoot(t *testing.T) {
	if _, err := New(Config{Root: ""}); err == nil {
		t.Error("empty root accepted")
	}
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("missing root accepted")
	}
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := New(Config{Root: f}); err == nil {
		t.Error("a file was accepted as a workspace root")
	}
}
