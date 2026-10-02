// Package sandbox is the single place where containment is implemented.
//
// Every filesystem read, every subprocess and every outbound request from a
// tool goes through here, so the rules are written once and tested once,
// adversarially, instead of being re-derived — and re-broken — in each tool.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools"
)

// Config configures a sandbox.
type Config struct {
	// Root is the workspace directory. Nothing outside it is reachable.
	Root string
	// ReadOnly disables write and execute tools entirely.
	ReadOnly bool
	// AllowedBinaries may be executed. Empty uses DefaultBinaries.
	AllowedBinaries []string
	// MaxFileSize caps a single read or write.
	MaxFileSize int64
	// MaxOutput caps captured stdout/stderr.
	MaxOutput int
	// ExecTimeout bounds one subprocess.
	ExecTimeout time.Duration
	// Concurrency caps simultaneous subprocesses.
	Concurrency int
}

// DefaultBinaries is the allowlist. It is intentionally short: each entry is a
// tool a developer actually asks for by voice, and nothing on it can delete
// files on its own.
var DefaultBinaries = []string{"git", "go", "npm", "pnpm", "yarn", "make", "ls", "cat", "rg", "grep", "jq", "docker", "python3", "node"}

// deniedArgs are argument patterns refused for every binary.
//
// The shell metacharacters are belt and braces: exec.Command never invokes a
// shell, so they are inert. Their presence means the plan tried to write shell,
// which means the plan is wrong — so it is rejected and audited rather than
// quietly executed with the characters taken literally.
var deniedArgs = []string{
	"$(", "`", "&&", "||", ";", "|", ">", ">>", "<",
	"rm -rf", "mkfs", "dd if=", "dd of=", ":(){", "chmod -R 777",
}

// deniedGitArgs are git operations that destroy history or remote state.
var deniedGitArgs = [][]string{
	{"push", "--force"},
	{"push", "-f"},
	{"push", "--delete"},
	{"reset", "--hard"},
	{"clean", "-fdx"},
	{"clean", "-xfd"},
	{"branch", "-D"},
	{"filter-branch"},
	{"reflog", "expire"},
}

// Sandbox is a confined view of one workspace.
type Sandbox struct {
	cfg  Config
	root *os.Root
	abs  string
	sem  chan struct{}

	binOnce sync.Once
	binPath map[string]string
	binErr  error
}

// New opens a sandbox rooted at cfg.Root.
func New(cfg Config) (*Sandbox, error) {
	if cfg.Root == "" {
		return nil, errors.New("sandbox: no workspace root configured")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: workspace %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox: workspace %s is not a directory", abs)
	}

	// os.Root resolves every path component relative to a directory handle and
	// refuses to escape it, including through a symlink created after an
	// earlier check passed. That last case is the time-of-check/time-of-use
	// race that defeats prefix-matching sandboxes.
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: open root %s: %w", abs, err)
	}

	if len(cfg.AllowedBinaries) == 0 {
		cfg.AllowedBinaries = DefaultBinaries
	}
	if cfg.MaxFileSize <= 0 {
		cfg.MaxFileSize = 10 << 20
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 64 << 10
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}

	return &Sandbox{
		cfg:  cfg,
		root: root,
		abs:  abs,
		sem:  make(chan struct{}, cfg.Concurrency),
	}, nil
}

// Close releases the root handle.
func (s *Sandbox) Close() error {
	if s.root != nil {
		return s.root.Close()
	}
	return nil
}

// Root implements tools.Sandbox.
func (s *Sandbox) Root() string { return s.abs }

// ReadOnly implements tools.Sandbox.
func (s *Sandbox) ReadOnly() bool { return s.cfg.ReadOnly }

// sensitive names are never readable, even inside the workspace. A read-class
// tool that can print .env would let any summary exfiltrate every secret the
// project has.
var sensitive = []string{
	".env", ".env.local", ".env.production", ".npmrc", ".netrc",
	"id_rsa", "id_ed25519", "id_ecdsa", ".pypirc", "credentials",
}

var sensitiveSuffixes = []string{".pem", ".key", ".p12", ".pfx", ".keystore"}
var sensitiveDirs = []string{".ssh", ".aws", ".gnupg", ".docker", ".kube"}

// checkPath normalises a tool-supplied path and rejects obviously sensitive
// targets before os.Root is even consulted.
func (s *Sandbox) checkPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty path", tools.ErrInvalidArgs)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: path contains a NUL byte", tools.ErrInvalidArgs)
	}
	// Normalise separators unconditionally, not via filepath.ToSlash, which is
	// a no-op on Unix. A Windows-style path arriving here is either a confused
	// client or a traversal attempt, and on Linux "..\..\etc\passwd" would
	// otherwise be one long legal filename that escapes nothing but litters
	// the workspace. The cost is that a Unix file whose name genuinely
	// contains a backslash is unreachable, which is a trade worth making.
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." {
		clean = "."
	}
	// os.Root rejects these anyway; failing here produces a clearer error and
	// keeps the audit log readable.
	if filepath.IsAbs(name) || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%w: %q", tools.ErrOutsideWorkspace, name)
	}

	base := strings.ToLower(path.Base(clean))
	for _, n := range sensitive {
		if base == n {
			return "", fmt.Errorf("%w: %q holds credentials", tools.ErrDenied, name)
		}
	}
	for _, suf := range sensitiveSuffixes {
		if strings.HasSuffix(base, suf) {
			return "", fmt.Errorf("%w: %q looks like a private key", tools.ErrDenied, name)
		}
	}
	for _, seg := range strings.Split(clean, "/") {
		for _, d := range sensitiveDirs {
			if strings.EqualFold(seg, d) {
				return "", fmt.Errorf("%w: %q is inside %s", tools.ErrDenied, name, d)
			}
		}
	}
	return clean, nil
}

// ReadFile implements tools.Sandbox.
func (s *Sandbox) ReadFile(name string) ([]byte, error) {
	clean, err := s.checkPath(name)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(clean)
	if err != nil {
		return nil, translate(err, name)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %q is a directory", tools.ErrInvalidArgs, name)
	}
	if info.Size() > s.cfg.MaxFileSize {
		return nil, fmt.Errorf("%w: %q is %d bytes, limit is %d",
			tools.ErrInvalidArgs, name, info.Size(), s.cfg.MaxFileSize)
	}

	// LimitReader guards against the file growing between Stat and Read.
	return io.ReadAll(io.LimitReader(f, s.cfg.MaxFileSize))
}

// WriteFile implements tools.Sandbox.
func (s *Sandbox) WriteFile(name string, data []byte) error {
	if s.cfg.ReadOnly {
		return fmt.Errorf("%w: workspace is read-only", tools.ErrDenied)
	}
	clean, err := s.checkPath(name)
	if err != nil {
		return err
	}
	if int64(len(data)) > s.cfg.MaxFileSize {
		return fmt.Errorf("%w: %d bytes exceeds the %d limit",
			tools.ErrInvalidArgs, len(data), s.cfg.MaxFileSize)
	}
	// Parent directories are not created implicitly: a typo in a path should
	// fail loudly, not scatter empty directories through the workspace.
	f, err := s.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return translate(err, name)
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// ReadDir implements tools.Sandbox.
func (s *Sandbox) ReadDir(name string) ([]fs.DirEntry, error) {
	clean, err := s.checkPath(name)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(clean)
	if err != nil {
		return nil, translate(err, name)
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// Stat implements tools.Sandbox.
func (s *Sandbox) Stat(name string) (fs.FileInfo, error) {
	clean, err := s.checkPath(name)
	if err != nil {
		return nil, err
	}
	info, err := s.root.Stat(clean)
	if err != nil {
		return nil, translate(err, name)
	}
	return info, nil
}

// resolveBinaries pins each allowlisted binary to an absolute path once, so a
// later PATH change cannot swap the executable under a running server.
func (s *Sandbox) resolveBinaries() {
	s.binOnce.Do(func() {
		s.binPath = map[string]string{}
		for _, b := range s.cfg.AllowedBinaries {
			if p, err := exec.LookPath(b); err == nil {
				s.binPath[b] = p
			}
		}
	})
}

// Run implements tools.Sandbox.
func (s *Sandbox) Run(ctx context.Context, name string, args ...string) (tools.ExecResult, error) {
	if s.cfg.ReadOnly {
		return tools.ExecResult{}, fmt.Errorf("%w: workspace is read-only", tools.ErrDenied)
	}
	s.resolveBinaries()

	bin, ok := s.binPath[name]
	if !ok {
		return tools.ExecResult{}, fmt.Errorf("%w: %q is not in the allowed binary list", tools.ErrDenied, name)
	}
	if err := checkArgs(name, args); err != nil {
		return tools.ExecResult{}, err
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return tools.ExecResult{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ExecTimeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = s.abs
	// The environment is constructed, not inherited: GITHUB_TOKEN, AWS_* and
	// every NEELKIT_* secret in the server's environment must never reach a
	// subprocess.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", // never block waiting for credentials
	}
	applyLimits(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := tools.ExecResult{
		Duration: time.Since(start),
		ExitCode: cmd.ProcessState.ExitCode(),
	}
	res.Stdout, res.Truncated = truncate(stdout.String(), s.cfg.MaxOutput)
	res.Stderr, _ = truncate(stderr.String(), s.cfg.MaxOutput)

	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("command timed out after %s", s.cfg.ExecTimeout)
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return res, fmt.Errorf("run %s: %w", name, err)
	}
	// A non-zero exit is data, not an error: "go test" failing is exactly the
	// answer the user asked for.
	return res, nil
}

// checkArgs rejects arguments that are destructive or that betray an attempt
// to write shell.
func checkArgs(bin string, args []string) error {
	joined := strings.ToLower(strings.Join(args, " "))
	for _, bad := range deniedArgs {
		if strings.Contains(joined, strings.ToLower(bad)) {
			return fmt.Errorf("%w: argument contains %q, which is never allowed", tools.ErrDenied, bad)
		}
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%w: argument contains a NUL byte", tools.ErrInvalidArgs)
		}
	}
	if bin == "git" {
		for _, seq := range deniedGitArgs {
			if containsSeq(args, seq) {
				return fmt.Errorf("%w: `git %s` destroys history or remote state",
					tools.ErrDenied, strings.Join(seq, " "))
			}
		}
	}
	return nil
}

// containsSeq reports whether every element of seq appears in args, in order.
// Checking the sequence rather than the joined string avoids both false
// negatives from flag reordering and false positives from a filename that
// happens to contain the words.
func containsSeq(args, seq []string) bool {
	i := 0
	for _, a := range args {
		if strings.EqualFold(a, seq[i]) {
			i++
			if i == len(seq) {
				return true
			}
		}
	}
	return false
}

func truncate(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return s[:max] + "\n... [truncated]", true
}

func translate(err error, name string) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %q does not exist", tools.ErrInvalidArgs, name)
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w: %q", tools.ErrOutsideWorkspace, name)
	}
	return fmt.Errorf("%w: %q: %v", tools.ErrOutsideWorkspace, name, err)
}

var _ tools.Sandbox = (*Sandbox)(nil)
