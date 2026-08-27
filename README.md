# opencode-go-proxy

A local reverse proxy that owns **one or more** OpenCode Go subscriptions. Models
configured in `free_model_map` first try a mapped ID on OpenCode's Zen free endpoint;
any non-200 falls through to the normal Go upstream. That Go path reads authenticated
quota windows, chooses the account with the most headroom, preserves free Go usage as
long as possible, and round-robins when every subscription is exhausted.

Clients point at `http://localhost:8082/v1` and use any non-empty placeholder API key.
The proxy injects the chosen account's real key. Request paths are preserved on both
routes; only a configured free-tier attempt rewrites the JSON `model` field.

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
| **Proactive** | authenticated `GET /zen/go/v1/usage` every 60s | headroom per window → stickiness + balancing | preserve last good windows, mark usage stale, keep reactive protection |
| **Reactive** | top-level `cost` field in each 200 response | ground truth tier: `0` = Go free, `>0` = PAYG kicked in | always available; catches usage API lag |

The API returns rolling, weekly, and monthly percentages, statuses, and absolute reset
timestamps. The `status` field (`ok`/`rate-limited`) is authoritative for exhaustion.
The reactive cost signal protects against a delayed poll: a paid response immediately
demotes that account before the next request is routed.

## Routing

Per request:
1. If the request model appears in `free_model_map`, rewrite it to the mapped ID and try
   `free_upstream` with a sticky non-avoided account. A 429 rotates the free sticky
   account; every non-200 or transport failure falls through to the Go path.
2. On the Go path, exclude `avoided` keys (401 cooldown). All excluded → `503`.
3. Prefer `go_free` accounts over `payg`.
4. Among the same tier, pick lower load. **Hysteresis (default 8 pts)** avoids switching
   the sticky account unless another is at least 8 points lower.
5. A 200 response with `cost>0` on a `go_free` account demotes it to `payg`
   immediately; the next request recomputes.
6. All PAYG → round-robin. The usage API does not expose Zen balances.

Go-upstream non-200 handling is conservative pass-through with no tier/state mutation,
except a self-healing **401 cooldown**: a 401 avoids that key for two minutes, and the
next 200 or cooldown expiry restores it.

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
  "free_upstream": "https://opencode.ai/zen",
  "free_model_map": {
    "deepseek-v4-flash": "deepseek-v4-flash-free",
    "mimo-v2.5": "mimo-v2.5-free"
  },
  "disable_payg": false,
  "poll_interval": "60s",
  "hysteresis_points": 8,
  "tier_safe_pct": 95,
  "avoid_401_cooldown": "2m",
  "request_timeout": "10m",
  "accounts": [
    {"name": "primary", "api_key": "sk-…"},
    {"name": "secondary", "api_key": "sk-…"}
  ]
}
```

`disable_payg` (default `false`) — when `true`, the proxy refuses to route to any
account whose Go usage is exhausted. Returns `503` instead of spending Zen balance.
Useful when you want to stay strictly within free Go quota.

Each account needs only its name and OpenCode Go API key. The proxy uses that same key
for inference and `GET /zen/go/v1/usage`; no workspace ID, browser cookie, or dashboard
session is required.

## Health

```
curl http://localhost:8082/health
```
```json
{
  "status": "ok",
  "active_key": "primary",
  "accounts": [
    {
      "name": "primary",
      "tier": "go_free",
      "rolling": {"pct": 0, "reset_in": "5h", "status": "ok", "present": true},
      "weekly": {"pct": 17, "reset_in": "5d", "status": "ok", "present": true},
      "monthly": {"pct": 42, "reset_in": "21d", "status": "ok", "present": true},
      "last_cost": "0",
      "last_error": "",
      "usage_fresh": true,
      "last_usage_at": "2026-08-25T00:00:00Z",
      "avoided": false
    }
  ],
  "aggregate": {
    "total_accounts": 1,
    "active_accounts": 1,
    "max_rolling_pct": 0,
    "max_weekly_pct": 17,
    "max_monthly_pct": 42,
    "any_avoided": false,
    "any_usage_stale": false
  },
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
   `http://localhost:8082` (apps that append `/v1` themselves). Either works when the
   final path is `/v1/chat/completions` or `/v1/messages`; mapped free-tier requests also
   rewrite the body model ID before preserving that path.
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

Unit tests cover authenticated usage API requests and response validation, the cost-field
distinguisher (string/number/missing, SSE trailing event), proactive and reactive tier
transitions, stale-data preservation, sticky+hysteresis routing, PAYG round-robin,
`disable_payg`, 401 cooldown + self-heal, auth header swapping, and end-to-end proxy
flows via `httptest`.

```bash
go test -v ./...
```
