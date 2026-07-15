# opencode-go-proxy

A local reverse proxy that owns **one or more** OpenCode Go subscriptions and intelligently
routes each request to the account with the most headroom. Preserves free Go usage as
long as possible, routes to the account with the highest remaining Zen balance when Go
is exhausted, and degrades safely when cookies/cost signals are unavailable.

Clients (pi, future agents) point at `http://localhost:8082/v1` and use any non-empty
placeholder API key — the proxy injects the real key for the chosen account. The proxy is
**path-transparent**: it forwards `/v1/chat/completions` (OpenAI-compat) and `/v1/messages`
(anthropic) to `https://opencode.ai/zen/go` unchanged except for the auth header.

## Quick start

```bash
go build -o opencode-go-proxy .
cp config.example.json config.json
# edit config.json with your account(s)
./opencode-go-proxy -config config.json
# point client at http://localhost:8082/v1
```

## The two detection signals (layered)

| Layer | Source | Role | Fallback if missing |
|---|---|---|---|
| **Proactive** | cookie scrape of `/workspace/<id>/go` + `/billing` every 60s | headroom per window → stickiness + balancing | cookie expiry → "unknown"; reactive still drives correctness |
| **Reactive** | top-level `cost` field in each 200 response | ground truth tier: `0` = Go free, `>0` = PAYG kicked in | always available; proxy routes on this alone if scrape is down |

Not redundant: proactive sees the *whole picture* ("weekly 30%, monthly 85% — resets in 2
weeks"); reactive sees *ground truth per request* ("scrape said 84% but cost just slipped
to PAYG"). The `status` field (`ok`/`rate-limited`) is a cleaner exhaustion signal than
inferring from 100%. Scrape = steering, cost = odometer. Scrape keeps you sticky; cost catches you
when the scrape is stale or wrong.

## Routing

Per request:
1. Exclude `avoided` keys (401 cooldown). Both excluded → `503`.
2. Prefer `go_free` accounts over `payg`.
3. Among same-tier, pick lower load. **Hysteresis (default 8 pts)** — don't switch the
   sticky active key unless the other is ≥8 pts lower. This is the stickiness.
4. **Reactive override**: a 200 response with `cost>0` on a `go_free` account demotes it
   to `payg` immediately; next request recomputes. Stale scrape can't cost you free tokens.
5. Both on PAYG → prefer the account with the highest Zen balance.

Non-200 handling is conservative pass-through: **no tier/state mutation** from stray 5xx /
429 / 402, protecting against transient corruption. The single exception is a self-healing
**401 cooldown** — a 401 marks that key avoided for 2 min, auto-recovered on the next 200 or
on cooldown expiry. This covers the revoked-key gap without lasting state corruption.

## Auth

The proxy swaps the auth header to match the upstream endpoint: OpenAI-compat
(`/v1/chat/completions`) accepts `Authorization: Bearer` or `x-api-key`; anthropic
(`/v1/messages`) requires `x-api-key`. The proxy sends whichever form the endpoint expects,
using the chosen account's real API key.

## Config

`~/.config/opencode-go-proxy/config.json` (600), see `config.example.json`:

```json
{
  "listen_addr": "127.0.0.1:8082",
  "upstream": "https://opencode.ai/zen/go",
  "disable_payg": false,
  "poll_interval": "60s",
  "scrape_cache_ttl": "90s",
  "hysteresis_points": 8,
  "tier_safe_pct": 95,
  "alert_email": "you@example.com",
  "smtp_config_path": "/path/to/.smtp_config",
  "stale_realert_hours": 24,
  "avoid_401_cooldown": "2m",
  "request_timeout": "10m",
  "accounts": [ { "name": "...", "api_key": "sk-…", "workspace_id": "wrk_…", "auth_cookie": "Fe26.2**…" } ]
}
```

`disable_payg` (default `false`) — when `true`, the proxy refuses to route to any
account whose Go usage is exhausted. Returns `503` instead of spending Zen balance.
Useful when you want to stay strictly within free Go quota.

Each account needs: its OpenCode Go API key, its workspace ID, and the `auth` cookie from a
logged-in dashboard session (scrape auth — *not* the API key). **Cookie-stale email alerting
is optional — leave both `alert_email` and `smtp_config_path` empty to disable.** When
enabled, SMTP credentials come from the shared `.smtp_config` file.

## Health

```
curl http://localhost:8082/health
```
```json
{
  "status": "ok",
  "active_key": "primary",
  "accounts": [
    { "name": "primary", "tier": "go_free",
      "rolling":  {"pct": 0,  "reset_in": "5h", "status": "ok", "present": true},
      "weekly":   {"pct": 0,  "reset_in": "2d", "status": "ok", "present": true},
      "monthly":  {"pct": 0,  "reset_in": "31d","status": "ok", "present": true},
      "payg": {"balance_usd": 8.44, "monthly_usage_usd": 11.56, "monthly_limit_usd": 50, "present": true},
      "last_cost": "0", "last_error": null, "cookie_fresh": true, ... },
    { "name": "secondary", "tier": "payg",
      "rolling":  {"pct": 100,"reset_in": "36m","status": "rate-limited", "present": true},
      "weekly":   {"pct": 53, "reset_in": "2d", "status": "ok", "present": true},
      "monthly":  {"pct": 26, "reset_in": "28d","status": "ok", "present": true},
      "payg": {"balance_usd": 19.14, "monthly_usage_usd": 0.86, "monthly_limit_usd": 50, "present": true},
      "last_cost": "0.00003450", "last_error": null, "cookie_fresh": true, ... }
  ],
  "upstream": "https://opencode.ai/zen/go",
  "disable_payg": false
}
```
`tier` is **runtime state**, not an account property — it flips as each account's rolling/weekly/monthly quota rolls over (the accounts above have swapped these roles since the proxy was first built). `name` is the only stable account identifier.

## Build / deploy

Three install paths:

**(a) Go binary** (see Quick start above):
```bash
go build -o opencode-go-proxy .
go test ./...
```

**(b) Docker Compose:**
```bash
docker compose up -d
```
Set `listen_addr` to `0.0.0.0:8082` in `config.json` when running in Docker so the port
is reachable from outside the container. See the `Dockerfile` and `docker-compose.yml`.

**(c) Systemd user unit** (example):
```bash
bash release.sh                     # build → install binary+unit → start
systemctl --user status opencode-go-proxy
journalctl --user -u opencode-go-proxy -f
```

**(d) GitHub Releases** — pre-built binaries for Linux and macOS (amd64, arm64) are attached
to each [release](https://github.com/carter2099/opencode-go-proxy/releases). Download, verify
with the included SHA-256 checksums, and place on `$PATH`.

## Pointing a client at the proxy

1. **Base URL** → `http://localhost:8082/v1` (apps that want the full base) or
   `http://localhost:8082` (apps that append `/v1` themselves). Either works; the proxy is
   path-transparent so long as the final URL is `…/v1/chat/completions` or `…/v1/messages`.
2. **API key** → any non-empty placeholder (e.g. `proxy`). The proxy overwrites it with the
   chosen account's real key. The app just needs *something* so its HTTP client sends an
   auth header.
3. That's it — no `/login` flow with the app; the proxy owns the real keys.

## Docker container clients

For a container client (e.g. Open WebUI) connecting to the proxy on the host, use
`host.docker.internal` (with `extra_hosts: host-gateway` in the client's compose file) or
`network_mode: host`. Configure your own firewall to restrict the exposed port.

Example: Open WebUI Admin Settings → Connections → OpenAI API: Base URL
`http://host.docker.internal:8082/v1`, API key `proxy` (placeholder). The model dropdown
stays identical (same models from the same upstream).

## Tests

Unit tests cover the cost-field distinguisher (string/number/missing, SSE trailing event),
the reactive demote override, proactive tier transitions, the scrape parsers (ported from
and cross-checked against the pi-go-bars TypeScript regexes), cookie-stale re-alert
suppression, sticky+hysteresis routing, highest-balance PAYG, disable_payg, 401 cooldown + self-heal, the auth
header swap, and end-to-end proxy flows via `httptest`.

```bash
go test -v ./...
```

## Out of v1 (deferred)

- Auto-retry of failed non-stream requests on another key.
- Failover to local llm-proxy (free qwen) when all Go subs fully exhausted.
- Self-healing expired cookies (manual refresh; email alert only).