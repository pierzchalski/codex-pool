# codex-pool

A reverse proxy that pools **Codex CLI**, **Claude Code**, **Gemini CLI**, **Kimi**, and **MiniMax** accounts behind a single endpoint. Incoming requests are dispatched to a healthy backend account; conversations pin to the same account for cache reuse; tokens refresh automatically.

## Build and Test

```bash
go build ./...        # build the binary
go test ./...         # run all tests
go run . --help       # show CLI flags
```

No code generation or pre-build steps required.

## Configuration

Config file: `config.toml` (or `$CONFIG_PATH`). See `config.toml.example`.

**Priority: env var > config file > default.** Implemented by `getConfigString` / `getConfigInt` in `config.go`. A handful of settings also accept CLI flags (defined in `main.go:buildConfig`).

Secrets can live in files (`admin_token_file`, `friend_code_file`, `jwt_secret_file`) to avoid putting them inline.

Key settings:
- `pool_dir` — directory containing provider subdirectories (`pool/codex/`, `pool/claude/`, etc.)
- `admin_token` — required to enable `/admin/*` endpoints
- `friend_code` — optional; enables a self-serve landing page for non-admin users
- `[pool_users].jwt_secret` — required to enable pool-user (client credential) management

## Pool Directory Layout

```
pool/
  codex/    *.json    Codex auth.json files
  claude/   *.json    Claude API key or OAuth JSON files
  gemini/   *.json    Gemini OAuth credential files
  kimi/     *.json    Kimi credentials
  minimax/  *.json    MiniMax credentials
```

The filename (minus `.json`) becomes the account ID shown in `/status` and `/admin/accounts`.

## Key Architecture

### Provider interface (`provider.go`)

`Provider` defines how to load, authenticate, and route for one backend type. The `ProviderRegistry` holds all providers and matches incoming request paths in order: **Gemini first, Claude second, Codex third** (order matters — Claude's `/v1/messages` must not be swallowed by Codex's broader `/v1/` prefix). Kimi/MiniMax are model-routed extras and never win path matching.

Provider implementations: `provider_codex.go`, `provider_claude.go`, `provider_gemini.go`, `provider_kimi.go`, `provider_minimax.go`.

### Pool state (`pool.go`)

`poolState` holds the live account list. `candidate()` implements tiered selection:

- **Tier 1** (preferred): Claude max, Codex pro, Gemini ultra
- **Tier 2** (standard): other first-party/OAuth accounts
- **Tier 3** (fallback): OpenRouter-backed Codex accounts

Within a tier, accounts are ranked by a score (headroom, drain urgency, inflight count, penalty). Conversations are pinned to an account via `convPin` map (conversation ID → account ID) for cache reuse; tier-3 pins yield to any better-tier account.

`loadPool` reads JSON files from provider subdirectories and delegates parsing to each provider's `LoadAccount`.

### Hot-reload watcher (`watcher.go`)

`poolWatcher` uses `fsnotify` to watch the pool directory tree and config file. Changes are debounced at 500 ms, then trigger a full pool reload (new `loadPool` call + `pool.replace()`). The server never needs to restart.

### Config (`config.go`)

`ConfigFile` mirrors `config.toml`. `loadConfigFile` parses it; `resolveConfigSecrets` substitutes `*_file` fields. `globalConfigFile` is a package-level pointer used by landing-page helpers (`getFriendName`, etc.).

### Pool users (`pool_users.go`)

`PoolUser` represents an end-user credential. The admin creates users via `POST /admin/pool-users`; the user downloads a generated config from `/setup/<tool>/<token>`. Requires `admin_token` (or `friend_code`) **and** `[pool_users].jwt_secret`.

### Frontend / landing pages (`frontend.go`)

HTML templates are embedded via `//go:embed`. Two modes: local (no `friend_code` set) and friend (shows branded landing).

### Storage

- `storage.go` / BoltDB (`proxy.db`): per-account usage totals, persisted across restarts.
- `analytics_store.go` / SQLite (`data/analytics.db`): per-request analytics with daily rollup.

### Format translation (`format_translate*.go`)

Translates between OpenAI-style request/response format (used by Codex/Kimi/MiniMax) and Anthropic-style (Claude). SSE variants handled in `*_sse.go` files.

## Conventions

- **Locking**: `Account.mu` is a `sync.Mutex`; always lock before reading or writing account fields. `poolState.mu` is a `sync.RWMutex`; use `RLock` for reads, `Lock` for writes.
- **Error handling**: `log.Fatalf` for startup errors; `log.Printf` for non-fatal runtime errors. HTTP errors via `http.Error`.
- **Atomic fields**: `config.debug` is `atomic.Bool` so it can be toggled live without a lock.
- **File layout**: one file per logical concern. Provider-specific logic lives in its own `provider_*.go` file; admin UI logic in `admin_*.go` files.
- **Naming**: exported types use clear domain nouns (`Account`, `PoolUser`, `Provider`). Internal helpers use lowercase.
