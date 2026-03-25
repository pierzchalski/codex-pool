<p align="center">
  <img src="logo.png" alt="codex-pool" width="400">
</p>

<h1 align="center">codex-pool</h1>

<p align="center">
  <strong>Pool your accounts. Share with friends. Never swap credentials again.</strong>
</p>

---

`codex-pool` is a reverse proxy that pools **Codex CLI**, **Claude Code**, and **Gemini CLI** accounts behind one endpoint.

You put provider credentials in `pool/`, point your local CLI at the proxy, and `codex-pool` picks a healthy backend account automatically. Conversations stay sticky to the same account for cache reuse, tokens refresh automatically where possible, and the status/admin pages show what is loaded.

<p align="center">
  <img src="screenshots/analytics-dashboard.png" alt="Pool Analytics" width="700">
</p>

---

## Read This First

For real CLI usage, the supported path is:

1. Run the proxy with `admin_token` and `[pool_users].jwt_secret` configured.
2. Add real provider accounts under `pool/`.
3. Create a pool user with `POST /admin/pool-users`.
4. Save the returned pool-user `token`.
5. Run the generated `/setup/<tool>/<token>` script on each client machine.

The exact pool-user creation command lives in [Pointing Your Local CLI At The Pool](#pointing-your-local-cli-at-the-pool).

That works for a single-user install too. It is also the easiest way to keep Codex, Claude, and Gemini all pointed at the same pool.

A few important details:

- `/admin/*` is **disabled** until top-level `admin_token` is set.
- Pool-user credentials are only available when `[pool_users].jwt_secret` is set.
- The filename under `pool/` becomes the account ID shown on `/status` and `/admin/accounts`.
- **OpenRouter is not a top-level provider directory.** If you want OpenRouter as a Codex fallback, it lives under `pool/codex/`.

---

## Quick Start

### 1. Create directories

```bash
mkdir -p pool/codex pool/claude pool/gemini data
mkdir -p container-config
```

If you run the binary directly, the default config path is `./config.toml`.

If you use the provided `docker-compose.yml`, the mounted config file path is:

```text
container-config/config.toml
```

### 2. Write a config file

Use `config.toml` for bare-metal runs, or `container-config/config.toml` for the compose setup.

```toml
listen_addr = "127.0.0.1:8989"
pool_dir = "pool"
db_path = "./data/proxy.db"
public_url = "http://127.0.0.1:8989"

# Enables /admin/*.
admin_token = "replace-with-your-admin_token"

# Secondary-usage threshold for tier preference.
tier_threshold = 0.50

# Optional self-serve landing page for friends.
# friend_code = "choose-a-friend_code-if-you-want-friends-mode"
# friend_name = "YourName"
# friend_tagline = "Optional landing-page tagline"

[pool_users]
# Required for generated pool-user credentials and /setup/* scripts.
jwt_secret = "replace-with-your-pool_users.jwt_secret"
storage_path = "./data/pool_users.json"
```

In the examples below, `<your top-level admin_token>` always means the exact `admin_token` value from this config file. It is different from the per-user pool token returned by `POST /admin/pool-users`.

### 3. Start the server

Bare metal:

```bash
go build && ./codex-pool
```

Docker Compose:

```bash
docker compose up --build
```

Podman Compose:

```bash
podman-compose -f docker-compose.yml -f docker-compose.podman.yml up --build
```

If you run under Podman without the provided override, use an equivalent `keep-id` user namespace mapping. Otherwise `/app/pool` or `/app/data` will usually hit permission errors.

### 4. Add provider accounts to `pool/`

The proxy hot-reloads `pool/` automatically. After you drop in a new JSON file, it should appear on `/status` within about a second.

Recommended check:

```bash
curl -fsS "http://127.0.0.1:8989/admin/accounts?admin_token=replace-with-your-admin_token" | jq
```

---

## Adding Accounts To The Pool

### Codex accounts

The simplest way to add an existing Codex login is to copy its `auth.json` into `pool/codex/`:

```bash
cp ~/.codex/auth.json pool/codex/codex-work.json
cp ~/other-home/.codex/auth.json pool/codex/codex-personal.json
```

Accepted format:

```json
{
  "tokens": {
    "access_token": "...",
    "refresh_token": "...",
    "id_token": "...",
    "account_id": "acct_..."
  },
  "last_refresh": "2026-03-25T00:00:00Z"
}
```

The proxy preserves unknown fields when it rewrites these files after refreshes.

#### Codex OAuth flow through the admin API

If you would rather log in through `codex-pool` directly:

```bash
ADD_JSON=$(curl -fsS -X POST \
  -H 'X-Admin-Token: <your top-level admin_token>' \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://127.0.0.1:8989/admin/codex/add)

printf '%s\n' "$ADD_JSON" | jq
VERIFIER=$(printf '%s' "$ADD_JSON" | jq -r .verifier)
printf '%s\n' "$ADD_JSON" | jq -r .oauth_url
```

Open the returned `oauth_url` in a browser. OpenAI will redirect to `http://localhost:1455/auth/callback`; if nothing is listening there, copy the final URL from the browser address bar and take the `code=` value from it, then exchange it:

```bash
CODE='paste-the-code-query-value-here'

curl -fsS -X POST \
  -H 'X-Admin-Token: <your top-level admin_token>' \
  -H 'Content-Type: application/json' \
  -d "{\"code\":\"$CODE\",\"verifier\":\"$VERIFIER\"}" \
  http://127.0.0.1:8989/admin/codex/exchange | jq
```

That saves a new file in `pool/codex/` and reloads the pool.

### Claude accounts

The recommended Claude path is the built-in OAuth flow. `codex-pool` will fetch the account plan from Anthropic and store it in the pool file.

```bash
ADD_JSON=$(curl -fsS -X POST \
  -H 'X-Admin-Token: <your top-level admin_token>' \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://127.0.0.1:8989/admin/claude/add)

printf '%s\n' "$ADD_JSON" | jq
VERIFIER=$(printf '%s' "$ADD_JSON" | jq -r .verifier)
printf '%s\n' "$ADD_JSON" | jq -r .oauth_url
```

Open the returned `oauth_url` in a browser. Anthropic will eventually land on a callback URL that contains `code=`; copy that final URL from the browser and exchange the callback `code`:

```bash
CODE='paste-the-code-query-value-here'

curl -fsS -X POST \
  -H 'X-Admin-Token: <your top-level admin_token>' \
  -H 'Content-Type: application/json' \
  -d "{\"code\":\"$CODE\",\"verifier\":\"$VERIFIER\"}" \
  http://127.0.0.1:8989/admin/claude/exchange | jq
```

Accepted manual OAuth-file format:

```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "...",
    "expiresAt": 1760000000000,
    "subscriptionType": "max",
    "rateLimitTier": "default_claude_max_20x"
  }
}
```

Accepted API-key format:

```json
{
  "api_key": "sk-ant-api03-...",
  "plan_type": "max"
}
```

`codex-pool` does **not** automatically scrape a live Claude credential out of `~/.claude/credentials.json`. If you want to add Claude manually, write a file in one of the JSON shapes above into `pool/claude/`.

### Gemini accounts

Gemini accounts are added by dropping an OAuth credential file into `pool/gemini/`:

```bash
cp ~/.gemini/oauth_creds.json pool/gemini/gemini-main.json
```

Accepted format:

```json
{
  "access_token": "ya29....",
  "refresh_token": "1//....",
  "token_type": "Bearer",
  "scope": "...",
  "expiry_date": 1760000000000,
  "plan_type": "ultra"
}
```

Notes:

- `plan_type` is optional. Use `"ultra"` if you want that account treated as the preferred Gemini tier.
- There is currently no `/admin/gemini/add` OAuth helper. Gemini accounts are file-based today.

### OpenRouter as a Codex fallback

Put the OpenRouter credential in **`pool/codex/`**, not in its own top-level directory.

Recommended filename:

```text
pool/codex/codex-openrouter.json
```

That makes the account show up as `codex-openrouter` on the status/admin pages.

File format:

```json
{
  "backend": "openrouter",
  "base_url": "https://openrouter.ai/api/v1",
  "plan_type": "api",
  "OPENAI_API_KEY": "sk-or-v1-..."
}
```

Notes:

- `OPENAI_API_KEY` is the field name the loader expects.
- `plan_type = "api"` is what makes the status page show `api` in the plan column.
- OpenRouter Codex accounts are treated as a **tier-3 fallback**.
- They are only used when no better first-party Codex account is eligible.
- They still satisfy Codex `pro`-required model routing.
- If a conversation is pinned to OpenRouter and a better first-party Codex account becomes available later, the conversation is unpinned from the fallback so it can move back.

---

## Pointing Your Local CLI At The Pool

Once you have real provider accounts in `pool/`, create a pool user and run the generated setup script for each local CLI.

Create the pool user:

```bash
POOL_USER_JSON=$(curl -fsS -X POST \
  -H 'X-Admin-Token: <your top-level admin_token>' \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","plan_type":"pro"}' \
  http://127.0.0.1:8989/admin/pool-users)

printf '%s\n' "$POOL_USER_JSON" | jq
TOKEN=$(printf '%s' "$POOL_USER_JSON" | jq -r .token)
```

The `X-Admin-Token` header above uses your top-level `admin_token` from config.toml. The returned `token` is a different credential: it is the pool-user token used by both `/config/*/<token>` and `/setup/*/<token>`.

### Codex CLI

Run the setup script:

```bash
curl -fsSL "http://127.0.0.1:8989/setup/codex/$TOKEN" | bash
```

Windows / PowerShell:

```powershell
irm "http://127.0.0.1:8989/setup/codex/$TOKEN?shell=powershell" | iex
```

What the Codex setup script does:

- writes `~/.codex/auth.json`
- writes or updates `~/.codex/config.toml`
- writes `~/.codex/model_catalog.json`
- installs a small `model_sync` MCP sidecar so the model catalog gets refreshed
- configures Codex to use:
  - `chatgpt_base_url = ".../backend-api"`
  - provider `base_url = ".../v1"`
  - `wire_api = "responses"`

The script is the recommended path. If you configure Codex manually, make sure your config matches those values.

Manual equivalent:

```bash
mkdir -p ~/.codex
curl -fsSL "http://127.0.0.1:8989/config/codex/$TOKEN" -o ~/.codex/auth.json
curl -fsSL -H "Authorization: Bearer $(jq -r '.tokens.access_token' ~/.codex/auth.json)" \
  "http://127.0.0.1:8989/backend-api/codex/models?client_version=0.106.0" \
  -o ~/.codex/model_catalog.json

cat > ~/.codex/config.toml <<'EOF'
model_provider = "codex-pool"
chatgpt_base_url = "http://127.0.0.1:8989/backend-api"
model_catalog_json = "~/.codex/model_catalog.json"

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "http://127.0.0.1:8989/v1"
wire_api = "responses"
requires_openai_auth = true
EOF
```

That manual path writes the same auth file and equivalent Codex config, but it skips the helper MCP sidecar that keeps `model_catalog.json` fresh.

### Claude Code

Run the setup script:

```bash
curl -fsSL "http://127.0.0.1:8989/setup/claude/$TOKEN" | bash
```

Windows / PowerShell:

```powershell
irm "http://127.0.0.1:8989/setup/claude/$TOKEN?shell=powershell" | iex
```

What the Claude setup script does:

- exports `ANTHROPIC_BASE_URL`
- exports `CLAUDE_CODE_OAUTH_TOKEN`
- updates `~/.claude/settings.json`
- marks `~/.claude.json` as having completed onboarding

Important: for pooled Claude usage, the client credential is **`CLAUDE_CODE_OAUTH_TOKEN`**, not `ANTHROPIC_API_KEY`.

Manual equivalent:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8989"
export CLAUDE_CODE_OAUTH_TOKEN="<token from /setup/claude/... or generated config>"
```

### Gemini CLI

Run the setup script:

```bash
curl -fsSL "http://127.0.0.1:8989/setup/gemini/$TOKEN" | bash
```

Windows / PowerShell:

```powershell
irm "http://127.0.0.1:8989/setup/gemini/$TOKEN?shell=powershell" | iex
```

What the Gemini setup script does:

- exports `CODE_ASSIST_ENDPOINT`
- exports `GOOGLE_GENAI_USE_GCA=1`
- exports `GOOGLE_CLOUD_ACCESS_TOKEN=<pool token>`

Important: the pool-user Gemini path does **not** rely on writing `~/.gemini/oauth_creds.json`. The setup script uses environment variables instead.

Manual equivalent:

```bash
export CODE_ASSIST_ENDPOINT="http://127.0.0.1:8989"
export GOOGLE_GENAI_USE_GCA=1
export GOOGLE_CLOUD_ACCESS_TOKEN="<token from /setup/gemini/...>"
```

---

## Status, Admin, And Operations

Useful endpoints:

- `/status`: HTML status page
- `/healthz`: health check
- `/admin/accounts`: loaded accounts
- `/admin/pool-users`: create/list pool users
- `/admin/reload`: force a pool reload

Admin auth rules:

- API clients should send `X-Admin-Token: ...`.
- If you are opening an admin page in a browser, use `?admin_token=...`.
- If `admin_token` is blank, `/admin/*` returns `admin access disabled`.

Examples:

```bash
curl -fsS -H 'X-Admin-Token: <your top-level admin_token>' \
  http://127.0.0.1:8989/admin/accounts | jq

curl -fsS -X POST -H 'X-Admin-Token: <your top-level admin_token>' \
  http://127.0.0.1:8989/admin/reload
```

Hot reload behavior:

- Changes under `pool/` are watched and reloaded automatically.
- The file watcher also reloads some non-sensitive config fields.
- Sensitive config such as `admin_token`, `friend_code`, and `[pool_users].jwt_secret` is effectively startup config. If you change those, restart the server/container.

Compose restart examples:

```bash
docker compose restart codex-pool
podman-compose restart codex-pool
```

---

## Friends Mode

If you want a self-serve landing page for other people, add a `friend_code` and keep `[pool_users].jwt_secret` configured:

```toml
friend_code = "choose-a-friend_code-to-share"
friend_name = "YourName"

[pool_users]
jwt_secret = "replace-with-your-pool_users.jwt_secret"
```

That uses the same pool-user machinery described above. The landing page hands out the same style of `/setup/*/<token>` instructions.

---

## Notes

- `PROXY_MAX_INMEM_BODY_BYTES` controls how large a request body can be before the proxy streams it directly instead of buffering it for retries. The default is `16777216` (16 MiB).
- Unknown JSON fields are preserved when account files are rewritten after refreshes.
- Using multiple accounts or sharing pooled access may violate provider terms. Make your own call.

---

## License

MIT
