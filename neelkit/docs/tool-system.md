# NeelKIT — Tool / Plugin Architecture

The goal: **adding a capability must never require editing the assistant.**

If adding a `kubectl` tool means touching `internal/assistant/`, the design
failed. This document shows why it does not.

---

## 1. The contract

```go
// internal/tools/tool.go

type RiskLevel string

const (
    RiskRead    RiskLevel = "read"    // observes only: cat, ls, git log, grep
    RiskWrite   RiskLevel = "write"   // mutates local state: write file, git branch
    RiskExecute RiskLevel = "execute" // runs a program: go test, make, npm run
    RiskNetwork RiskLevel = "network" // leaves the machine: HTTP, GitHub API
)

type Descriptor struct {
    Name        string          // "git.branch_create" — namespace.verb, stable, an API
    Title       string          // "Create git branch"  — shown in the UI
    Description string          // one sentence; this is the LLM's entire understanding
    Schema      json.RawMessage // JSON Schema draft 2020-12 for Input
    Risk        RiskLevel
    Idempotent  bool            // safe to retry after an ambiguous failure?
    Examples    []Example       // few-shot examples for the planner AND doc examples
}

type Input struct {
    Args      json.RawMessage
    Workspace string          // absolute, already validated as the sandbox root
    Turn      TurnContext     // conversation id, turn id, request id, cwd hint
}

type Result struct {
    Summary  string          // one line, spoken back to the user
    Data     any             // structured, rendered in the UI
    Stdout   string          // capped
    ExitCode int
    Artifacts []Artifact     // files produced, for the download panel
}

type Tool interface {
    Descriptor() Descriptor
    Execute(ctx context.Context, in Input) (Result, error)
}
```

Four design points, each defensible:

1. **`Schema` is JSON Schema, not a Go struct tag convention.** It is served to
   three consumers that cannot share Go types: the LLM planner, the
   `GET /v1/tools` endpoint, and the web UI's argument form. One source of
   truth, generated from the struct with `invopop/jsonschema` in a test that
   fails if the checked-in schema drifts.

2. **`Risk` is declared by the tool, not inferred by the caller.** The policy
   engine must never have to guess. A tool author declaring `RiskRead` on
   something that writes is a review failure — and a lint test asserts that no
   tool importing `os/exec` declares below `RiskExecute`.

3. **`Workspace` arrives pre-validated.** Tools never resolve paths themselves.
   They receive an `*os.Root` via the sandbox (see §5). A tool *cannot* escape
   the workspace because it is never handed anything that could.

4. **`Idempotent` drives retry.** The runner will retry a failed idempotent
   tool once on a transient error (timeout, EAGAIN). It will never retry
   `git.commit`.

---

## 2. Registration — how a tool is added without touching the core

```go
// internal/tools/builtin/git/branch_create.go
package git

import "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/registry"

func init() { registry.MustRegister(&branchCreate{}) }

type branchCreate struct{}

func (t *branchCreate) Descriptor() tools.Descriptor { /* … */ }
func (t *branchCreate) Execute(ctx context.Context, in tools.Input) (tools.Result, error) {
    var a struct{ Name string `json:"name"` }
    if err := tools.Decode(in.Args, &a); err != nil { return tools.Result{}, err }
    return in.Sandbox.Run(ctx, "git", "switch", "-c", a.Name)
}
```

```go
// cmd/neelkitd/plugins.go — the ONLY file that changes when shipping a new tool
package main

import (
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fs"
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/http"
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
    _ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
)
```

That blank-import list *is* the plugin manifest. It is the same pattern as
`database/sql` drivers and `image/png` decoders — a Go reviewer recognises it
without explanation, and it keeps `internal/assistant` free of any `switch` over
tool names.

**What happens automatically once a tool registers:**

| Consumer | Effect | Mechanism |
|---|---|---|
| LLM planner | The tool becomes callable | Planner builds its function schema from `registry.Descriptors()` at request time |
| `GET /v1/tools` | Appears in the list | Same call |
| Web UI | Gets an argument form + a permission card | Renders from the JSON Schema |
| Policy engine | Gets gated | Reads `Descriptor().Risk` |
| Audit log | Gets logged | The runner logs every call uniformly |
| `nk tools list` | Appears | Hits the same endpoint |
| Metrics | Gets timed | `neelkit_tool_execution_seconds{tool=…}` from the runner |

Zero of those are per-tool code.

**Registration is strict:** `MustRegister` panics at init on a duplicate name or
an invalid schema. A broken tool crashes the process at startup, not at 2am on
the first invocation.

---

## 3. The v1 tool set

Chosen because each one removes a real interruption from a real day, not to hit
a count.

| Namespace | Tools | Risk | The problem it removes |
|---|---|---|---|
| `fs` | `read`, `write`, `list`, `search` | read / **write** | "what's in that config" without alt-tabbing |
| `git` | `status`, `diff`, `log`, `branch_create`, `branch_list`, `commit_message` | read / **write** | branch naming and commit messages, hands-free |
| `shell` | `run` (allowlisted argv) | **execute** | `go test ./...` / `make build` while reading the code |
| `http` | `request` | **network** | poke an endpoint without writing the curl by hand |
| `search` | `code`, `docs` | read | ripgrep over the workspace, by voice |
| `analyze` | `explain_error`, `explain_sql`, `json_to_types`, `summarize_log` | read | paste-and-explain, spoken |
| `notes` | `create`, `list`, `todo_from_turn` | **write** | capture a TODO mid-thought without losing the thought |

Mapping the user's example commands onto it:

| Spoken | Resolves to |
|---|---|
| "create a git branch called feature auth" | `git.branch_create{name:"feature-auth"}` — *grammar* |
| "write a commit message from these changes" | `git.diff` → `git.commit_message` — *grammar chain* |
| "run the test suite" | `shell.run{argv:["go","test","./..."]}` — *grammar*, **confirm** |
| "search my project for authentication logic" | `search.code{pattern:"auth"}` — *grammar* |
| "explain this error" | `analyze.explain_error{text:<clipboard\|last stderr>}` — *LLM* |
| "convert this JSON into a TypeScript interface" | `analyze.json_to_types` — *LLM* |
| "generate a curl request for this endpoint" | `http.request` with `dry_run:true` → prints, never sends — *LLM* |
| "summarize this log" | `analyze.summarize_log` — *LLM* |

Note `http.request` with `dry_run` — "generate a curl request" is a **read**
operation that happens to be about a network tool. Conflating "produce the
command" with "run the command" is exactly the class of mistake the risk model
exists to prevent.

---

## 4. Execution path

```
ToolPlan ─► registry.Lookup(name) ─► schema.Validate(args)
         ─► policy.Decide(risk, settings, args) ─► allow│confirm│deny
         ─► [confirm] ─► WS tool.permission_required ─► wait ─► approve│deny│timeout
         ─► audit.Write(status=pending)            ← BEFORE anything runs
         ─► sandbox.Enter(workspaceRoot)
         ─► ctx, cancel := WithTimeout(perToolTimeout)
         ─► tool.Execute(ctx, in)
         ─► truncate stdout/stderr to 64 KiB
         ─► audit.Update(status, exit, duration)
         ─► metrics.Observe(tool, outcome, duration)
         ─► Result
```

Two ordering decisions that matter:

- **Schema validation before policy.** Malformed arguments are rejected before
  a human is asked to approve them — you cannot meaningfully approve a call you
  cannot read.
- **Audit row written before execution.** If the process is killed mid-command,
  a `pending` row with full arguments survives. An audit log that only records
  successes is not an audit log.

---

## 5. The sandbox

`internal/tools/sandbox` is the single place containment is implemented, so it
is tested once, adversarially, instead of re-implemented per tool.

### Filesystem

```go
root, err := os.OpenRoot(cfg.WorkspaceRoot)   // Go 1.24+
f, err := root.Open("src/main.go")            // traversal refused by the runtime
```

`os.Root` resolves every path component relative to a directory handle and
refuses to escape it — including through symlinks, including through a symlink
created *after* a prior check passed. That last case is the TOCTOU race that
defeats `strings.HasPrefix(filepath.Clean(p), root)`, which is how most
hand-rolled sandboxes fail.

Additionally: a denylist of names that are never readable even inside the
workspace (`.env`, `.env.*`, `id_rsa`, `*.pem`, `.aws/`, `.ssh/`,
`.git/config`), and a 10 MB per-file read cap.

### Process execution

- `exec.CommandContext` with an **explicit argv**. There is no `sh -c` anywhere
  in this codebase, so there is no shell to inject into. Command substitution,
  pipes, redirection and `;` are not parsed because nothing parses them.
- **Binary allowlist**, configurable, defaulting to
  `git, go, npm, pnpm, make, ls, cat, rg, jq, docker`. Resolved once via
  `exec.LookPath` at startup and pinned to the absolute path, so a later `PATH`
  change cannot swap the binary.
- **Argument denylist** applied per binary — this is where the destructive
  operations live:

  | Blocked outright | Requires explicit confirmation every time |
  |---|---|
  | `rm -rf`, `rm -r /`, `mkfs`, `dd of=/dev/*` | `git push --force*` |
  | `git push --delete`, `git branch -D` on the default branch | `git reset --hard` |
  | `chmod -R 777`, `chown -R` outside workspace | `git clean -fdx` |
  | `curl … \| sh`, `wget … \| bash` | `docker rm`, `docker rmi` |
  | anything containing `$(`, backticks, `;`, `&&`, `\|` | `npm publish` |

  The last row is belt-and-braces: those characters are meaningless to
  `exec.Command`, but their *presence* means the model tried to write shell,
  which means the plan is wrong. Reject and log it.
- **Resource bounds:** `RLIMIT_CPU`, `RLIMIT_AS`, `RLIMIT_NOFILE` and
  `RLIMIT_FSIZE` set on the child via `SysProcAttr` (Linux/macOS); output capped
  at 64 KiB streamed and discarded past that; hard timeout via context with
  `SIGTERM` then `SIGKILL` after a 3 s grace; `Setpgid` so a timeout kills the
  whole process group, not just the shell's first child.
- **Environment is constructed, not inherited.** The child gets
  `PATH`, `HOME`, `LANG`, and nothing else. `NEELKIT_*`, `AWS_*`, `GITHUB_TOKEN`
  and everything else in the server's environment never reaches a tool.

### Network (`http.request`)

SSRF is the realistic attack here: an LLM that reads a malicious file can be
told to fetch `http://169.254.169.254/latest/meta-data/`.

- Custom `net.Dialer.Control` inspects the **resolved IP** — not the hostname —
  and rejects loopback, private (RFC 1918), link-local, CGNAT, multicast,
  unspecified, and the cloud metadata addresses. Checking the hostname is
  useless: DNS resolves `evil.com` to `127.0.0.1` for free.
- The check runs **on every connection**, so DNS rebinding between the check and
  the dial cannot win.
- Redirects are followed at most 3 times and **re-checked at each hop**; scheme
  downgrades to non-HTTP(S) are refused.
- Response body capped at 5 MB, total request deadline 30 s.
- An optional host allowlist (`NEELKIT_HTTP_ALLOWED_HOSTS`) turns the tool from
  deny-known-bad into allow-known-good for anyone who wants it.

---

## 6. Adding a new tool — the whole checklist

1. Create `internal/tools/builtin/<ns>/<verb>.go`.
2. Implement `Descriptor()` and `Execute()`.
3. `func init() { registry.MustRegister(&t{}) }`.
4. Add the blank import to `cmd/neelkitd/plugins.go`.
5. Write a table test for `Execute` against a temp workspace.
6. Optionally add a grammar rule in `internal/intent/grammar/rules.go` if it is
   a command worth making deterministic.

**Nothing in `internal/assistant`, `internal/intent/planner`, `internal/httpapi`
or `web/` changes.** That is the whole claim, and step 4 being the only file
outside the tool's own package is the evidence.

---

## 7. Out-of-process plugins (roadmap, designed for now)

`registry` accepts a `ToolProvider`, not only concrete `Tool`s:

```go
type ToolProvider interface {
    Descriptors(ctx context.Context) ([]Descriptor, error)
    Invoke(ctx context.Context, name string, in Input) (Result, error)
}
```

`builtinProvider` wraps the in-process registry. A second implementation,
`mcpProvider`, speaks **MCP over stdio** — which makes every existing MCP
server a NeelKIT plugin, in any language, without a NeelKIT SDK.

It is deliberately *not* in v1: it adds process supervision, a second trust
boundary, and a schema-translation layer, for zero benefit until there is a
tool worth writing outside Go. The interface exists so that work is additive.

`RemoteRunner` (AD-008) reuses the same shape for the "server in the cloud,
tools on my laptop" topology.
