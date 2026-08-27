## Overview

A local reverse proxy that tries configured model mappings on the Zen free endpoint, then falls through to quota-aware routing across one or more OpenCode Go API keys at `https://opencode.ai/zen/go`.

## Build & test

```bash
go build -o opencode-go-proxy .
go test -v ./...  # no network
```

No formatters/linters configured.

Releases: push a `v*` tag (e.g. `v1.0.0`); `.github/workflows/release.yml` cross-compiles
linux/darwin amd64/arm64 binaries and attaches them + SHA-256 checksums to the GitHub Release.

## Repo structure

Single `package main`, no subpackages.

|File|Role|
|---|---|
|`main.go`|entrypoint, `usageLoop`, `pollUsage`, `/health` handler|
|`proxy.go`|`proxyCore`, free-tier mapped-model attempt/fallthrough, Go `handleProxy`, 200-cost extraction, `swapAuth`|
|`routing.go`|`account` runtime state, tier transitions, `applyUsage`/`applyCost`, 401 cooldown, `snapshot`|
|`picker.go`|`picker.choose`: tier preference → sticky+hysteresis → PAYG round-robin|
|`usage.go`|authenticated `/zen/go/v1/usage` client and strict response parser|
|`config.go`|`Config`/`AccountCfg`, `free_upstream`/`free_model_map`, `loadConfig`, JSON `duration` helper|
|`proxy_test.go` / `usage_test.go`|unit and `httptest` coverage|

## Invariants (do not violate)

- On the Go-upstream path, non-200 responses are pass-through with **no tier/state mutation** — the only exception is the 401 cooldown (`mark401`/`clear401On200`). The separate free-tier path falls through on non-200 and rotates its sticky account on 429.
- The top-level `cost` field in a 200 body is ground truth: `cost>0` on a `go_free` account demotes it to `payg` immediately (`applyCost`).
- `tier` is runtime state, not an account property; `name` is the only stable account identifier.
- The authenticated usage API steers; `cost` verifies. A stale poll must never cost free tokens — the reactive override catches it.
- A configured free-tier model tries its mapped Zen ID first. Any non-200 falls through to the Go picker; a free-tier 429 also rotates the free sticky account.

## Conventions

- Mutex per account (`account.mu`) and a picker mutex (`picker.mu`); `snapshot` is the only `/health` read path.
- Config is JSON with `time.Duration` strings (the `duration` UnmarshalJSON helper).
- API keys live only in `config.json` (gitignored).
