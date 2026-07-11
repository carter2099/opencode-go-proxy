package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ─── cost extraction: the verified "0000" PAYG distinguisher ────────────────

func TestExtractCostFromBody_String(t *testing.T) {
	// GO free → "cost":"0"  (verified 3/3 across stream/non-stream)
	cost, ok := extractCostFromBody([]byte(`{"id":"x","cost":"0","choices":[]}`))
	if !ok {
		t.Fatal("expected cost field present")
	}
	if cost != "0" {
		t.Errorf("cost = %q, want 0", cost)
	}
	// PAYG → "cost":"0.00002260"  (verified non-stream; maxed key)
	cost, ok = extractCostFromBody([]byte(`{"cost":"0.00002260"}`))
	if !ok {
		t.Fatal("missing cost")
	}
	if !costIsPayg(cost) {
		t.Errorf("cost=%s should be payg", cost)
	}
}

func TestExtractCostFromBody_Number(t *testing.T) {
	// Some gateways emit cost as a JSON number rather than a string.
	cost, ok := extractCostFromBody([]byte(`{"cost":0.0000226}`))
	if !ok {
		t.Fatal("missing cost")
	}
	if !costIsPayg(cost) {
		t.Errorf("cost=%s should be payg", cost)
	}
}

func TestExtractCostFromBody_Missing(t *testing.T) {
	if _, ok := extractCostFromBody([]byte(`{"choices":[]}`)); ok {
		t.Error("missing cost should return ok=false")
	}
	if _, ok := extractCostFromBody([]byte(`{not json`)); ok {
		t.Error("invalid json should return ok=false")
	}
}

func TestExtractCostFromSSELine(t *testing.T) {
	// SSE trailing usage event carries the cost (verified streaming).
	good := []byte(`data: {"id":"x","cost":"0.00003140","choices":[]}`)
	cost, ok := extractCostFromSSELine(good)
	if !ok {
		t.Fatal("expected cost from SSE data line")
	}
	if cost != "0.00003140" {
		t.Errorf("cost = %q, want 0.00003140", cost)
	}
	if _, ok := extractCostFromSSELine([]byte(`data: [DONE]`)); ok {
		t.Error("[DONE] should report no cost")
	}
	if _, ok := extractCostFromSSELine([]byte(`: keepalive`)); ok {
		t.Error("comment line should report no cost")
	}
	if _, ok := extractCostFromSSELine([]byte(`event: usage`)); ok {
		t.Error("non-data event header should report no cost")
	}
	// A chunk with no cost field is valid SSE but carries no signal: ok=false.
	if _, ok := extractCostFromSSELine([]byte(`data: {"delta":"hi"}`)); ok {
		t.Error("cost-less data line should report no cost")
	}
}

func TestExtractCostFromSSE_SkipsInferenceCostTelemetry(t *testing.T) {
	// Live observation (2026-07-11): OpenCode emits TWO cost-bearing events per
	// stream. The `inference-cost` telemetry event carries a normalised/
	// hypothetical cost (non-zero even on go_free) — it is NOT the actual
	// charge. The billing-truth event has `cost` but no `x-opencode-type` tag.
	// We must skip the telemetry; relying on event order would wrongly demote a
	// go_free account when the telemetry happens to land last.
	telemetry := []byte(`data: {"choices":[],"x-opencode-type":"inference-cost","cost":"0.00003450","normalizedUsage":{"inputTokens":7}}`)
	if cost, ok := extractCostFromSSELine(telemetry); ok {
		t.Fatalf("inference-cost telemetry must be skipped; got cost=%q", cost)
	}
	// The untagged summary event is billing truth — even with a non-zero cost
	// (PAYG case) it must be captured.
	payg := []byte(`data: {"choices":[],"cost":"0.00003140"}`)
	cost, ok := extractCostFromSSELine(payg)
	if !ok {
		t.Fatal("untagged summary event should report its cost")
	}
	if !costIsPayg(cost) {
		t.Errorf("summary cost %s should be payg", cost)
	}
	// go_free summary (cost 0) must still be captured and NOT demote.
	gofree := []byte(`data: {"choices":[],"cost":"0"}`)
	cost, ok = extractCostFromSSELine(gofree)
	if !ok {
		t.Fatal("go_free summary event should report its cost")
	}
	if costIsPayg(cost) {
		t.Error("cost 0 must not be payg")
	}
}

// ─── applyCost: the reactive demote override ────────────────────────────────

func newTestAccount(name string, t tier) *account {
	return &account{cfg: AccountCfg{Name: name, APIKey: "sk-test-" + name, WorkspaceID: "wrk_t_" + name, AuthCookie: "Fe26.2**test"}, tier: t}
}

func TestApplyCost_DemotesGoFreeOnPositiveCost(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	a.applyCost("0.00002260")
	if a.tier != tierPayg {
		t.Errorf("expected demote to payg, got %s", a.tier)
	}
	if a.lastCost != "0.00002260" {
		t.Errorf("lastCost = %q", a.lastCost)
	}
}

func TestApplyCost_KeepsGoFreeOnZeroCost(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	a.applyCost("0")
	if a.tier != tierGoFree {
		t.Errorf("zero cost must not demote, got %s", a.tier)
	}
}

func TestApplyCost_AlreadyPaygStays(t *testing.T) {
	a := newTestAccount("a", tierPayg)
	a.applyCost("0.0005")
	if a.tier != tierPayg {
		t.Errorf("payg stays payg, got %s", a.tier)
	}
}

// ─── applyScrape: proactive tier + stale transition ───────────────────────

func TestApplyScrape_StaleSetsCookieFalseAndError(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	// Make it "ever fresh" first.
	good := UsageData{Rolling: UsageWindow{UsagePercent: 12, ResetInSec: 3600, Present: true}, HasAny: true}
	a.applyScrape(good, PaygData{}, time.Now())
	if !a.cookieFresh || !a.everFresh {
		t.Fatal("precondition: should be fresh after good scrape")
	}
	// Now a stale scrape (redirect/login → no windows, error set).
	stale := UsageData{Error: "session expired"}
	a.applyScrape(stale, PaygData{}, time.Now())
	if a.cookieFresh {
		t.Error("cookieFresh must be false after stale scrape")
	}
	if a.lastError == "" {
		t.Error("lastError must record the scrape failure")
	}
}

func TestApplyScrape_RateLimitedStatusDemotes(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	monRateLimited := UsageData{
		Monthly: UsageWindow{UsagePercent: 100, ResetInSec: 2592000, Status: "rate-limited", Present: true},
		HasAny:  true,
	}
	a.applyScrape(monRateLimited, PaygData{}, time.Now())
	if a.tier != tierPayg {
		t.Errorf("rate-limited monthly should demote to payg, got %s", a.tier)
	}
}

func TestApplyScrape_FreshWindowsKeepGoFree(t *testing.T) {
	a := newTestAccount("a", tierPayg) // start payg
	good := UsageData{
		Rolling: UsageWindow{UsagePercent: 4, ResetInSec: 3600, Present: true},
		Weekly:  UsageWindow{UsagePercent: 1, ResetInSec: 604800, Present: true},
		Monthly: UsageWindow{UsagePercent: 0, ResetInSec: 2592000, Present: true},
		HasAny:  true,
	}
	a.applyScrape(good, PaygData{}, time.Now())
	if a.tier != tierGoFree {
		t.Errorf("reset windows should promote back to go_free, got %s", a.tier)
	}
}

// ─── stale-alert re-suppression ───────────────────────────────────────────────

func TestStaleAlertInfo_TransitionFiresOnce(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	now := time.Now()
	// Never fresh yet → no alert.
	if a.staleAlertInfo(now, 24*time.Hour) {
		t.Error("should not alert before ever being fresh")
	}
	// Good scrape.
	a.applyScrape(UsageData{Rolling: UsageWindow{UsagePercent: 12, ResetInSec: 3600, Present: true}, HasAny: true}, PaygData{}, now)
	// Transition to stale.
	a.applyScrape(UsageData{Error: "session expired"}, PaygData{}, now.Add(time.Second))
	if !a.staleAlertInfo(now.Add(2*time.Second), 24*time.Hour) {
		t.Error("fresh→stale transition should alert")
	}
	// Immediately again → suppressed (re-alert window not elapsed).
	if a.staleAlertInfo(now.Add(3*time.Second), 24*time.Hour) {
		t.Error("re-alert too soon should be suppressed")
	}
	// After the re-alert window → re-alert.
	if !a.staleAlertInfo(now.Add(25*time.Hour), 24*time.Hour) {
		t.Error("re-alert window elapsed should re-alert")
	}
}

// ─── picker: sticky + hysteresis, tier preference, round-robin, 401 ──────────

func twoAccountCfg() Config {
	return Config{
		HysteresisPoints:  8,
		TierSafePct:       95,
		Avoid401Cooldown:  duration(2 * time.Minute),
		Accounts: []AccountCfg{
			{Name: "a", APIKey: "sk-a", WorkspaceID: "wrk-a", AuthCookie: "Fe26.2**a"},
			{Name: "b", APIKey: "sk-b", WorkspaceID: "wrk-b", AuthCookie: "Fe26.2**b"},
		},
	}
}

func setLoad(a *account, rollPct float64) {
	a.mu.Lock()
	a.roll = UsageWindow{UsagePercent: rollPct, ResetInSec: 3600, Present: true}
	a.mu.Unlock()
}

func TestPicker_PrefersGoFreeOverPayg(t *testing.T) {
	p := newPicker(twoAccountCfg())
	a, b := p.accounts[0], p.accounts[1]
	// Both go_free initially; set loads differing way below hysteresis so a stays sticky/lowest.
	setLoad(a, 10)
	setLoad(b, 70)
	chosen, err := p.choose(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if chosen.cfg.Name != "a" {
		t.Errorf("first choice = %s, want lower-load a", chosen.cfg.Name)
	}
	// Now make b payg; a still free → a must win regardless of b's load.
	b.mu.Lock()
	b.tier = tierPayg
	b.mu.Unlock()
	setLoad(a, 90) // a near exhausted but still free
	chosen, _ = p.choose(time.Now())
	if chosen.cfg.Name != "a" {
		t.Errorf("go_free must be chosen over payg, got %s", chosen.cfg.Name)
	}
}

func TestPicker_HysteresisKeepsSticky(t *testing.T) {
	p := newPicker(twoAccountCfg())
	a, b := p.accounts[0], p.accounts[1]
	// Sticky lands on a (lower).
	setLoad(a, 50)
	setLoad(b, 55)
	p.choose(time.Now()) // establishes sticky = a (lower load, free)
	// b drops to 47 — that's 3pts below a (50), under the 8pt hysteresis → keep a.
	setLoad(b, 47)
	chosen, _ := p.choose(time.Now())
	if chosen.cfg.Name != "a" {
		t.Errorf("hysteresis should keep sticky a (b only 3pts lower), got %s", chosen.cfg.Name)
	}
	// b drops to 40 — 10pts below a → switch.
	setLoad(b, 40)
	chosen, _ = p.choose(time.Now())
	if chosen.cfg.Name != "b" {
		t.Errorf("should switch to b (10pts lower, ≥8 hysteresis), got %s", chosen.cfg.Name)
	}
}

func TestPicker_RoundRobinOnPayg(t *testing.T) {
	p := newPicker(twoAccountCfg())
	a, b := p.accounts[0], p.accounts[1]
	// Force both to PAYG.
	a.mu.Lock()
	a.tier = tierPayg
	a.mu.Unlock()
	b.mu.Lock()
	b.tier = tierPayg
	b.mu.Unlock()

	c1, _ := p.choose(time.Now())
	c2, _ := p.choose(time.Now())
	c3, _ := p.choose(time.Now())
	if c1.rrIndex != a.rrIndex && c1.rrIndex != b.rrIndex {
		t.Fatal("first choice out of set")
	}
	// Two calls must alternate between distinct accounts.
	if c1.cfg.Name == c2.cfg.Name {
		t.Errorf("round-robin must alternate: c1=%s c2=%s (same)", c1.cfg.Name, c2.cfg.Name)
	}
	// Third call returns to the first account (1,0,1 for two accounts).
	if c3.cfg.Name != c1.cfg.Name {
		t.Errorf("round-robin third call = %s, want %s (period 2)", c3.cfg.Name, c1.cfg.Name)
	}
}

func TestPicker_401AvoidExcluded(t *testing.T) {
	p := newPicker(twoAccountCfg())
	a, b := p.accounts[0], p.accounts[1]
	now := time.Now()
	// Avoid a; b remains → b must be chosen.
	a.mark401(2*time.Minute, now)
	chosen, err := p.choose(now)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.cfg.Name != "b" {
		t.Errorf("should skip avoided a, got %s", chosen.cfg.Name)
	}
	// Avoid both → 503 error.
	b.mark401(2*time.Minute, now)
	if _, err := p.choose(now); err == nil {
		t.Error("both avoided should return error")
	}
}

func TestPicker_401CooldownSelfHeals(t *testing.T) {
	p := newPicker(twoAccountCfg())
	a := p.accounts[0]
	now := time.Now()
	a.mark401(2*time.Minute, now)
	a.clear401On200(now.Add(1 * time.Minute))
	if a.isAvoided(now.Add(90 * time.Second)) {
		t.Error("clear401On200 should release the cooldown")
	}
}

// ─── swapAuth: the verified auth-header swap ────────────────────────────────

func TestSwapAuth_BearerWhenAuthorizationPresent(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer placeholder")
	req.Header.Set("anthropic-version", "2023-06-01")
	swapAuth(req, req.Header, "sk-real")
	if got := req.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want Bearer sk-real", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("other headers must be preserved; anthropic-version = %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("x-api-key must NOT be set when client used Authorization; got %q", got)
	}
}

func TestSwapAuth_XAPIKeyForAnthropic(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Api-Key", "placeholder")
	req.Header.Set("anthropic-version", "2023-06-01")
	swapAuth(req, req.Header, "sk-real")
	if got := req.Header.Get("X-Api-Key"); got != "sk-real" {
		t.Errorf("x-api-key = %q, want sk-real", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization must not be set for x-api-key clients; got %q", got)
	}
}

func TestSwapAuth_DefaultsBearerWhenNoAuthHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	swapAuth(req, req.Header, "sk-real")
	if got := req.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("default Authorization = %q, want Bearer sk-real", got)
	}
}

// ─── end-to-end proxy: cost signal flows through, key is swapped, 401 avoids ─

func TestHandleProxy_NonStreamCostDemoteAndKeySwap(t *testing.T) {
	// Fake upstream returns a 200 non-stream body with a positive cost.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if key != "Bearer sk-a" {
			t.Errorf("upstream got Authorization %q, want Bearer sk-a (swap failed)", key)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","cost":"0.00002260","choices":[]}`))
	}))
	defer upstream.Close()

	cfg := Config{Upstream: upstream.URL, RequestTimeout: duration(10 * time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "sk-a", WorkspaceID: "wrk-a", AuthCookie: "Fe26.2**a"}}}
	pc := newProxyCore(cfg)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"kimi-k2.5","stream":false}`))
	req.Header.Set("Authorization", "Bearer X")
	rec := httptest.NewRecorder()
	pc.handleProxy(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	a := pc.picker.accounts[0]
	if a.tier != tierPayg {
		t.Errorf("positive cost 200 should demote account a to payg, got %s; lastCost=%q", a.tier, a.lastCost)
	}
}

func TestHandleProxy_401TriggersCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer upstream.Close()

	cfg := Config{Upstream: upstream.URL, RequestTimeout: duration(10 * time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "sk-a", WorkspaceID: "wrk-a", AuthCookie: "Fe26.2**a"}}}
	pc := newProxyCore(cfg)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer X")
	rec := httptest.NewRecorder()
	pc.handleProxy(rec, req)

	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401 (passed through)", rec.Code)
	}
	a := pc.picker.accounts[0]
	if !a.isAvoided(time.Now()) {
		t.Error("401 should put account into cooldown")
	}
}

func TestHandleProxy_BothAvoidedReturns503(t *testing.T) {
	cfg := Config{Upstream: "http://example.invalid", RequestTimeout: duration(time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "k", WorkspaceID: "w", AuthCookie: "c"}}}
	pc := newProxyCore(cfg)
	pc.picker.accounts[0].mark401(10*time.Minute, time.Now())

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	pc.handleProxy(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}