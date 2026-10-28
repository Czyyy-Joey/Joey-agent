# Joey-agent

A learning-oriented AI agent harness in Go, modelled on the architecture of OpenAI Codex (`codex-rs/core`).

**Status: Phase 3 (context management)**, built after Phases 4 and 5 — streaming model calls through OneAPI, six built-in tools
(`read_file`, `list_directory`, `search_files`, `write_file`, `apply_patch`, `exec_command`), approvals in
the web UI and the terminal, multi-step loop, interrupt, SSE event stream, CLI and web UI.
Sessions, turns, history and events are stored in SQLite (`~/.joey/joey.db`) and survive restarts.
Each session picks its own working directory when it is created.

Restarting restores *history*, not execution: a turn that was running when the process stopped is
marked `interrupted`, its unfinished tool calls get an `aborted` output, and the model is told the
previous turn was cut off.

Phase 4 adds the command-execution layer (`internal/sandbox`): sandbox policies, a mandatory path guard
for file tools, execution inside macOS Seatbelt (the same mechanism Codex uses on macOS) and an
approval decision. Phase 5 gives the agent those capabilities as tools: shell commands run in the session's
sandbox with live output, file edits go through the path guard, and anything that needs approval pauses
the turn until the user answers ("allow once", "allow for this session", "deny", "deny and stop").
Phase 3 keeps long tasks inside the context window: a layered system prompt (base instructions,
environment, `AGENTS.md`/`JOEY.md`), token accounting, tool outputs truncated in the middle, and automatic
compaction into a handoff summary that is checkpointed in SQLite.

## Quick start

Requirements: macOS (for the sandbox), Go 1.26+, Node 20+.

```bash
# Offline demo with a scripted model (no API key needed)
make demo                      # builds the web UI, serves http://127.0.0.1:8080 (data in ~/.joey/demo.db)

# Real model through OneAPI
export ONEAPI_API_KEY=<token>  # or set provider.api_key in joey.toml (gitignored)
make run                       # or: go run ./cmd/joey serve
go run ./cmd/joey chat "读取 README.md 并告诉我项目名"
go run ./cmd/joey chat -cwd ~/code/other-project "这个项目是做什么的？"
go run ./cmd/joey chat -session sess_xxx "接着上次的问题"   # continue a stored session
```

UI development with hot reload: run `joey serve` in one terminal and `cd web && npm run dev` in another,
then open http://localhost:5173 (Vite proxies `/api` to `:8080`).

Configuration: copy `joey.example.toml` to `joey.toml`. Environment overrides: `ONEAPI_BASE_URL`,
`JOEY_MODEL`, `JOEY_ADDR`, `JOEY_WORKSPACE`, `JOEY_ALLOWED_ROOTS`, `JOEY_DB`, `JOEY_MAX_STEPS`,
`JOEY_CONTEXT_WINDOW`, `JOEY_SANDBOX_MODE`, `JOEY_APPROVAL`.

Working directories: in the web UI, "新会话" opens the native macOS folder dialog (the browser must run on the
same machine as joey; otherwise, or with `joey serve -native-dialog=false`, a built-in directory browser is used). A session's directory must lie under
`workspace.allowed_roots` (default `~`) or be `workspace.root` itself (the default for new sessions,
`-workspace` on the command line). The directory is fixed for the life of the session; file tools
cannot see anything outside it. Use `-db :memory:` to run without persistence.

## Tests

| Command | What it runs |
|---|---|
| `make test` | All Go unit, integration and e2e tests with `-race`; offline, scripted model |
| `make web-test` | Frontend reducer tests (Vitest) |
| `make lint` | gofmt, go vet, and the `agent` → `session`/`server` dependency check |
| `make smoke` | Real requests against OneAPI: a tool round trip and a summary request (needs `ONEAPI_API_KEY` or `provider.api_key`); `JOEY_SMOKE_OVERFLOW=1` adds the ~1.1M-token overflow probe |

## Layout

```
cmd/joey/                 serve and chat commands
internal/protocol/        Item and Event types shared with the frontend
internal/llm/             Provider interface, errors, model capabilities
  openaicompat/           Chat Completions streaming client (tool_calls assembly, retries)
  llmtest/                Scripted provider and fake OneAPI server for tests
internal/tools/           Tool interface and router (validation, timeouts, panics, aborts)
  builtin/                read_file, list_directory, search_files, write_file, apply_patch, exec_command
internal/patch/           Parser and applier for the Codex apply_patch format
internal/agent/           Turn loop (RunTurn); no dependency on storage or HTTP
internal/session/         Sessions, one active turn each, cancellation, event bus, restart recovery
internal/store/           SQLite: sessions, turns, items, events
internal/history/         History invariants, token estimates, truncation, compaction and checkpoint replay
internal/prompt/          Layered system prompt: base, <environment_context>, AGENTS.md / JOEY.md
internal/workspace/       Which directories a session may use; directory listing for the picker
internal/sandbox/         Policy, path guard, Seatbelt profile and process control, approval decision
internal/dialog/          Native macOS folder dialog
internal/server/          REST + SSE API, origin and token checks
web/                      React + Vite + Tailwind UI
e2e/                      Black-box tests that run the joey binary
```

## How the loop maps to Codex

| Joey (Go) | Codex (Rust) |
|---|---|
| `agent.RunTurn` | `run_turn` in `core/src/session/turn.rs` |
| `turnRunner.sample` | `run_sampling_request` / `try_run_sampling_request` |
| `llm.StreamEvent` | `ResponseEvent` in `codex-api/src/common.rs` |
| `tools.Router.Execute` | `ToolRouter` + `ToolCallRuntime::handle_tool_call` |
| `session.Session.Interrupt` (context cancel) | `abort_all_tasks` / `CancellationToken` |
| `history.Normalize` | `ensure_call_outputs_present` in `context_manager/normalize.rs` |
| `history.AbortMarker` | the `<turn_aborted>` marker written by `handle_task_abort` |
| `store.DB` (SQLite) | `RolloutRecorder` (JSONL) / `ThreadStore` |
| `turnRunner.compact` | `run_compact_task_inner_impl` in `core/src/compact.rs` |
| `history.BuildCompacted` | `build_compacted_history` (user messages within 20k tokens + summary) |
| `history.TruncateMiddle` | `truncate_middle_with_token_budget` in `utils/string/src/truncate.rs` |
| `prompt.LoadDocs` | `agents_md.rs` (AGENTS.md from the project root down to the cwd) |

Differences worth knowing: Codex speaks the Responses API and dispatches tools while the stream is
still running; Joey speaks Chat Completions (what OneAPI exposes) and runs tools after the stream
finishes, because a call is only complete once its arguments stop arriving.

## Sandbox

macOS only. Commands run under `/usr/bin/sandbox-exec` with a profile generated from the policy, so
the kernel enforces the limits — the same approach as Codex (`codex-rs/sandboxing/src/seatbelt.rs`).

```bash
joey sandbox info -profile                         # policy and the generated Seatbelt profile
joey sandbox exec -- ls -la
joey sandbox exec -timeout 1s -- sleep 30          # killed after 1s, exit 124
joey sandbox exec -- 'echo x > /etc/x'             # Operation not permitted
joey sandbox exec -- 'cat ~/.ssh/config'           # Operation not permitted (deny_read)
joey sandbox exec -network -- 'curl -sI https://example.com'
```

| | `read-only` | `workspace-write` (default) | `danger-full-access` |
|---|---|---|---|
| Reads | whole disk except `deny_read` | whole disk except `deny_read` | everything |
| Writes | none | workspace, `writable_roots`, `$TMPDIR`, `/tmp`; not `.git` / `.joey` | everything |
| Network (`network = false`) | blocked, including unix sockets | blocked, including unix sockets | allowed |
| Sandboxed | yes | yes | **no** |

`deny_read` defaults to `~/.ssh`, `~/.gnupg`, `~/.aws` and `~/.netrc`. Everything else on disk is
readable by commands, as in Codex; file tools are stricter and only see the session's directory.
There are no memory/CPU limits. In every mode the working directory is pinned inside the workspace,
the environment is reduced to an allow-list (API keys and tokens are never passed), timeouts kill
the whole process group (SIGTERM, then SIGKILL) and output is capped (head and tail kept).

Approval (`approval = "on-request"`, default): commands run inside the sandbox without asking;
obviously destructive ones (`sudo`, `rm -rf ~`, `curl … | sh`, …) always need approval, because
`rm -rf .` inside the workspace is still allowed by the sandbox. `untrusted` asks for everything
except known read-only commands; `never` never asks and refuses the destructive ones.

## Tools and approvals

| Tool | Parallel | Approval |
|---|---|---|
| `read_file`, `list_directory`, `search_files` | yes (run together) | never |
| `write_file`, `apply_patch` | no | only with `approval = "untrusted"`; paths outside the writable roots are refused, not asked |
| `exec_command` | no | dangerous commands always; everything not known read-only with `untrusted`; `sandbox_permissions = "require_escalated"` (run outside Seatbelt) always |

Consecutive read-only calls run in parallel; other calls run alone and in order. Arguments are checked
against each tool's JSON Schema and errors are returned to the model with the expected parameters.
A pending approval is answered with `POST /api/sessions/{id}/approvals/{approval_id}` and
`{"decision": "approved" | "approved_for_session" | "denied" | "abort"}`; after
`agent.approval_timeout_seconds` (default 600) it counts as denied. "Allow for this session" is kept in
memory only. `joey chat` asks on the terminal (`y` / `a` / `n` / `q`).

`make demo-coding` runs a scripted coding task on `testdata/demo/goapp` with `approval = "untrusted"`.

## Context management

| Setting | Default | Meaning |
|---|---|---|
| `model.context_window` | 900000 | Measured on 2026-10-28: 900k input tokens accepted, 1M rejected with HTTP 400 |
| `model.auto_compact_token_limit` | 200000 | Compact when the context passes this, or 90% of the window if that is smaller |
| `model.tool_output_tokens` | 10000 | Kept per tool output (head and tail), at most a quarter of the compaction limit |
| `agent.project_docs` | true | Add `AGENTS.md` / `JOEY.md` from the git root down to the workspace (32 KiB total) |

The context size is the last usage the gateway reported plus a bytes/4 estimate of everything appended
since; without usage it is estimated entirely. Compaction runs before a turn (history plus the new input
would pass the limit), between steps (tool outputs passed it), and after the model reports a context
overflow (compact, then retry once). The model writes a handoff summary; the new model history is the
most recent user messages (up to 20k tokens) plus that summary. The full history stays in SQLite, and a
`compaction` item records the replacement, so a restarted session continues from the checkpoint.
Type `/compact` in the web UI or `joey chat` to compact by hand.

`make demo-long` runs a scripted task in an 8000-token window that compacts after the third large file.

## Security notes

- The server listens on `127.0.0.1` by default and refuses other addresses unless `JOEY_AUTH_TOKEN` is set.
- Browser requests from non-local origins are rejected.
- File tools always go through the sandbox path guard (`internal/sandbox.Guard`): symlinks are resolved and
  paths outside the session's directory and inside `deny_read` are refused; `.git` and `.joey` are read-only. The guard checks
  before use, so a file swapped between the check and the use (TOCTOU) is not caught.
- `GET /api/workspace/dirs` lets any API client list directory names under the allowed roots. Keep
  `allowed_roots` as narrow as you need, especially if you set `JOEY_AUTH_TOKEN` and expose the server.
- The database stores full conversations, including file contents the tools returned. It is created
  with `0700` directory permissions.
