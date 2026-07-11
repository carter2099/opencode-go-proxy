# opencode-go-proxy

A local reverse proxy that owns **two** OpenCode Go subscriptions and intelligently
routes each request to the account with the most headroom. Preserves free Go usage as
long as possible, balances Zen pay-as-you-go ($25 cap each) across both accounts when Go
is exhausted, and degrades safely when cookies/cost signals are unavailable.

Clients (pi, future agents) point at `http://localhost:8082/v1` and use any non-empty
placeholder API key — the proxy injects the real key for the chosen account. The proxy is
**path-transparent**: it forwards `/v1/chat/completions` (OpenAI-compat) and `/v1/messages`
(anthropic) to `https://opencode.ai/zen/go` unchanged except for the auth header.

## The two detection signals (layered)

| Layer | Source | Role | Fallback if missing |
|---|---|---|---|
| **Proactive** | cookie scrape of `/workspace/<id>/go` + `/billing` every 60s | headroom per window → stickiness + balancing | cookie expiry → "unknown"; reactive still drives correctness |
| **Reactive** | top-level `cost` field in each 200 response | ground truth tier: `0` = Go free, `>0` = PAYG kicked in | always available; proxy routes on this alone if scrape is down |

Not redundant: proactive sees the *whole picture* ("weekly 30%, monthly 85% — resets in 2
weeks"); reactive sees *ground truth per request* ("scrape said 84% but cost just slipped
to PAYG"). Scrape = steering, cost = odometer. Scrape keeps you sticky; cost catches you
when the scrape is stale or wrong.

## Routing

Per request:
1. Exclude `avoided` keys (401 cooldown). Both excluded → `503`.
2. Prefer `go_free` accounts over `payg`.
3. Among same-tier, pick lower load. **Hysteresis (default 8 pts)** — don't switch the
   sticky active key unless the other is ≥8 pts lower. This is the stickiness.
4. **Reactive override**: a 200 response with `cost>0` on a `go_free` account demotes it
   to `payg` immediately; next request recomputes. Stale scrape can't cost you free tokens.
5. Both on PAYG → round-robin to spread the two $25 caps.

Non-200 handling is conservative pass-through: **no tier/state mutation** from stray 5xx /
429 / 402, protecting against transient corruption. The single exception is a self-healing
**401 cooldown** — a 401 marks that key avoided for 2 min, auto-recovered on the next 200 or
on cooldown expiry. This covers the revoked-key gap without lasting state corruption.

## Config

`~/.config/opencode-go-proxy/config.json` (600), see `config.example.json`:

```json
{
  "listen_addr": "127.0.0.1:8082",
  "upstream": "https://opencode.ai/zen/go",
  "poll_interval": "60s",
  "scrape_cache_ttl": "90s",
  "hysteresis_points": 8,
  "tier_safe_pct": 95,
  "alert_email": "carter2099@pm.me",
  "smtp_config_path": "/home/carter/scripts/.smtp_config",
  "stale_realert_hours": 24,
  "avoid_401_cooldown": "2m",
  "request_timeout": "10m",
  "accounts": [ { "name": "...", "api_key": "sk-…", "workspace_id": "wrk_…", "auth_cookie": "Fe26.2**…" } ]
}
```

Each account needs: its OpenCode Go API key, its workspace ID, and the `auth` cookie from a
logged-in dashboard session (scrape auth — *not* the API key). SMTP credentials for the
cookie-stale alert come from the shared `.smtp_config` file.

## Health

```
curl http://localhost:8082/health
```
```json
{
  "status": "ok",
  "active_key": "carter2099",
  "accounts": [
    { "name": "carter2099", "tier": "go_free",
      "rolling":  {"pct": 0,  "reset_in": "5h", "status": "ok", "present": true},
      "weekly":   {"pct": 0,  "reset_in": "2d", "status": "ok", "present": true},
      "monthly":  {"pct": 0,  "reset_in": "31d","status": "ok", "present": true},
      "payg": {"balance_usd": 8.44, "monthly_usage_usd": 11.56, "monthly_limit_usd": 25, "present": true},
      "last_cost": "0", "last_error": null, "cookie_fresh": true, ... },
    { "name": "carterbrwn2", "tier": "payg",
      "rolling":  {"pct": 100,"reset_in": "36m","status": "rate-limited", "present": true},
      "weekly":   {"pct": 53, "reset_in": "2d", "status": "ok", "present": true},
      "monthly":  {"pct": 26, "reset_in": "28d","status": "ok", "present": true},
      "payg": {"balance_usd": 19.14, "monthly_usage_usd": 0.86, "monthly_limit_usd": 25, "present": true},
      "last_cost": "0.00003450", "last_error": null, "cookie_fresh": true, ... }
  ]
}
```
`tier` is **runtime state**, not an account property — it flips as each account's rolling/weekly/monthly quota rolls over (the two accounts above have swapped these roles since the proxy was first built). `name` is the only stable account identifier.

## Build / deploy / manage

```bash
cd ~/dev/opencode-go-proxy
bash release.sh                                    # build → install binary+unit → start
systemctl --user status opencode-go-proxy
journalctl --user -u opencode-go-proxy -f
go test ./...                                      # unit tests (no network)
```

## Pointing a client at the proxy

1. **Base URL** → `http://localhost:8082/v1` (apps that want the full base) or
   `http://localhost:8082` (apps that append `/v1` themselves). Either works; the proxy is
   path-transparent so long as the final URL is `…/v1/chat/completions` or `…/v1/messages`.
   **For a Docker container** (e.g. Open WebUI), do NOT use `localhost` — that's the
   container's own loopback. Use `http://host.docker.internal:8082/v1` (with `extra_hosts:
   - host.docker.internal:host-gateway` in compose), and the host must allow the container's
   docker-bridge interface in ufw (see "Docker container clients" below).
2. **API key** → any non-empty placeholder (e.g. `proxy`). The proxy overwrites it with the
   chosen account's real key. The app just needs *something* so its HTTP client sends an
   auth header.
3. That's it — no `/login` flow with the app; the proxy owns the real keys.

## Tests

Unit tests cover the cost-field distinguisher (string/number/missing, SSE trailing event),
the reactive demote override, proactive tier transitions, the scrape parsers (ported from
and cross-checked against the pi-go-bars TypeScript regexes), cookie-stale re-alert
suppression, sticky+hysteresis routing, PAYG round-robin, 401 cooldown + self-heal, the auth
header swap, and end-to-end proxy flows via `httptest`.

```bash
go test -v ./...
```

## Verified empirically (pre-build, 2026-07-07)

- Port 8082 free (8081 = llm-proxy).
- `cost` field signal confirmed across verified-distinct keys (sha256 differ): healthy key
  → `cost:"0"` (stream + non-stream); maxed key → `cost:"0.00002260"` non-stream /
  `cost:"0.00003140"` stream. Lives only in the top-level `cost` field
  (`usage.estimated_cost` is always 0).
- `status` field (`ok`/`rate-limited`) emitted alongside `usagePercent`/`resetInSec` — a
  cleaner exhaustion signal than inferring from 100%.
- Auth: OpenAI-compat path accepts `Authorization: Bearer` or `x-api-key`; anthropic path
  requires `x-api-key`. Proxy swaps whichever the request uses.

## Docker container clients (e.g. Open WebUI)

The proxy binds `0.0.0.0:8082` (not loopback) so Docker containers can reach it, but
**ufw gates it to the docker bridges only** — the LAN still can't reach it (default deny),
matching the posture of the sibling `llm-proxy` on `:8081`. Two requirements for a container
client:

1. `host.docker.internal:host-gateway` in the container's compose `extra_hosts` (so the
   hostname resolves to the host).
2. A ufw allow rule for the container's actual docker-bridge interface, e.g.
   `sudo ufw allow in on br-<id> to any port 8082 proto tcp`. The bridge interface is
   `br-<first 12 of the docker network id>`; find it with `docker network inspect <name>`.
   **Do not** assume the packet arrives on `docker0` or from `172.23.0.0/16` — those are
   stale defaults; a container's traffic arrives on whatever bridge its network uses
   (`172.22`/`172.18`/…) and ufw's `on docker0` rules won't match. The k3s `cni0`/`flannel.1`
   rules elsewhere in this homelab are the same pattern.

Open WebUI Admin Settings → Connections → OpenAI API: Base URL
`http://host.docker.internal:8082/v1`, API key `proxy` (placeholder). The model dropdown
stays identical (same 20 models from the same upstream).

## Out of v1 (deferred)

- Auto-retry of failed non-stream requests on the other key.
- Failover to local llm-proxy (free qwen) when both Go subs fully exhausted.
- Self-healing expired cookies (manual refresh; email alert only).