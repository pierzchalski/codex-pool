# AGENTS.md — codex-pool

## What this is

A single-binary Go reverse proxy that pools Codex, Claude, Gemini, Kimi, and MiniMax accounts. Incoming LLM API requests are dispatched to a healthy backend account chosen from `pool/`. See `CLAUDE.md` for full architecture details.

## Build / Test

```bash
go build ./...   # must pass before committing
go test ./...    # run all tests
```

No external tools, code generators, or pre-build steps.

## Config precedence

`env var > config file (config.toml) > default`

Implemented by `getConfigString` / `getConfigInt` in `config.go`. Do not hardcode defaults into callers; add them to those helpers.

## Key principles

**Don't break hot-reload.** The `poolWatcher` (`watcher.go`) watches `pool/` and `config.toml` for filesystem changes and reloads accounts without restarting the server. Any code that reads accounts must go through `poolState` (with its `RWMutex`), not stash account pointers across requests.

**Lock discipline.** `Account.mu` guards all `Account` fields — lock before read, unlock promptly. `poolState.mu` is a `RWMutex`; use `RLock` for queries, `Lock` only for `replace()`. Never hold both locks simultaneously.

**Provider path matching order is load-bearing.** `ProviderRegistry` matches request paths in registration order (Gemini → Claude → Codex). Claude's `/v1/messages` prefix must be registered before Codex's `/v1/` or Codex will claim Claude requests. See `provider.go:NewProviderRegistry`.

**Admin endpoints are opt-in.** `/admin/*` is disabled unless `admin_token` is set. Pool-user management additionally requires `[pool_users].jwt_secret`. Do not add new admin routes without the auth guard (`checkAdminAuth`).

**One provider file per backend.** New providers belong in a new `provider_<name>.go`. Register them in `NewProviderRegistry` (in `provider.go`) and add their subdirectory to `loadPool` (in `pool.go`).

**Secrets via files.** Prefer `*_file` config fields over inline secrets. `resolveConfigSecrets` in `config.go` handles the substitution.

## Gotchas

- The `poolState.convPin` map is cleared on every reload (`pool.replace`). Clients will be re-pinned on their next request.
- `config.debug` is an `atomic.Bool` — toggled at runtime via `/admin/debug`. Do not cache it.
- `Account.Inflight` is updated with `sync/atomic` ops (not under `Account.mu`). Use `atomic.LoadInt64` / `atomic.AddInt64`.
- OpenRouter is not a top-level provider directory. It lives under `pool/codex/` with a `backend: "openrouter"` field and is treated as Tier 3 (last resort).
- Token refresh uses a separate HTTP transport (`refreshTransport`) that may be configured to use a proxy (`refresh_proxy_url`). Refresh code should not use the standard transport.
- The Gemini provider has two base URLs: `geminiBase` (OAuth/Code Assist) and `geminiAPIBase` (API key). `UpstreamURL` in `provider_gemini.go` routes between them based on path.
