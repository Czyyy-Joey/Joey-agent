# Joey-agent

A learning-oriented AI agent harness in Go, modelled on the architecture of OpenAI Codex (`codex-rs/core`).

**Status: Phase 2 (Sessions and persistence)** — streaming model calls through OneAPI, tool calling with
`read_file` / `list_directory`, multi-step loop, interrupt, SSE event stream, CLI and web UI.
Sessions, turns, history and events are stored in SQLite (`~/.joey/joey.db`) and survive restarts.
Each session picks its own working directory when it is created.

Restarting restores *history*, not execution: a turn that was running when the process stopped is
marked `interrupted`, its unfinished tool calls get an `aborted` output, and the model is told the
previous turn was cut off.

## Quick start

Requirements: Go 1.26+, Node 20+.

```bash
# Offline demo with a scripted model (no API key needed)
make demo                      # builds the web UI, serves http://127.0.0.1:8080 (data in ~/.joey/demo.db)

# Real model through OneAPI
export ONEAPI_API_KEY=<token>  # never put the key in a config file
make run                       # or: go run ./cmd/joey serve
go run ./cmd/joey chat "读取 README.md 并告诉我项目名"
go run ./cmd/joey chat -cwd ~/code/other-project "这个项目是做什么的？"
go run ./cmd/joey chat -session sess_xxx "接着上次的问题"   # continue a stored session
```

UI development with hot reload: run `joey serve` in one terminal and `cd web && npm run dev` in another,
then open http://localhost:5173 (Vite proxies `/api` to `:8080`).

Configuration: copy `joey.example.toml` to `joey.toml`. Environment overrides: `ONEAPI_BASE_URL`,
`JOEY_MODEL`, `JOEY_ADDR`, `JOEY_WORKSPACE`, `JOEY_ALLOWED_ROOTS`, `JOEY_DB`, `JOEY_MAX_STEPS`.

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
| `make smoke` | One real tool round trip against OneAPI (needs `ONEAPI_API_KEY`) |

## Layout

```
cmd/joey/                 serve and chat commands
internal/protocol/        Item and Event types shared with the frontend
internal/llm/             Provider interface, errors, model capabilities
  openaicompat/           Chat Completions streaming client (tool_calls assembly, retries)
  llmtest/                Scripted provider and fake OneAPI server for tests
internal/tools/           Tool interface and router (validation, timeouts, panics, aborts)
  builtin/                read_file, list_directory, workspace path guard
internal/agent/           Turn loop (RunTurn); no dependency on storage or HTTP
internal/session/         Sessions, one active turn each, cancellation, event bus, restart recovery
internal/store/           SQLite: sessions, turns, items, events
internal/history/         History invariants (call/output pairing, abort markers)
internal/workspace/       Which directories a session may use; directory listing for the picker
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

Differences worth knowing: Codex speaks the Responses API and dispatches tools while the stream is
still running; Joey speaks Chat Completions (what OneAPI exposes) and runs tools after the stream
finishes, because a call is only complete once its arguments stop arriving.

## Security notes

- The server listens on `127.0.0.1` by default and refuses other addresses unless `JOEY_AUTH_TOKEN` is set.
- Browser requests from non-local origins are rejected.
- File tools resolve symlinks and refuse paths outside the session's directory. This is a path guard, not a
  sandbox; tools are read-only so far. Command execution and real isolation arrive in Phase 4.
- `GET /api/workspace/dirs` lets any API client list directory names under the allowed roots. Keep
  `allowed_roots` as narrow as you need, especially if you set `JOEY_AUTH_TOKEN` and expose the server.
- The database stores full conversations, including file contents the tools returned. It is created
  with `0700` directory permissions.
