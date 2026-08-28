package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	return &account{cfg: AccountCfg{Name: name, APIKey: "sk-test-" + name}, tier: t}
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

// ─── applyUsage: proactive tier + freshness ─────────────────────────────────

func testUsage(rolling, weekly, monthly float64) UsageData {
	return UsageData{
		Rolling: UsageWindow{UsagePercent: rolling, Status: "ok", Present: true},
		Weekly:  UsageWindow{UsagePercent: weekly, Status: "ok", Present: true},
		Monthly: UsageWindow{UsagePercent: monthly, Status: "ok", Present: true},
	}
}

func TestApplyUsageErrorMarksStaleAndPreservesWindows(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	now := time.Now()
	a.applyUsage(testUsage(12, 8, 4), now, 95)
	if !a.usageFresh {
		t.Fatal("usage should be fresh after a successful API response")
	}

	a.markUsageError(http.ErrHandlerTimeout)
	if a.usageFresh {
		t.Error("usageFresh must be false after an API error")
	}
	if a.lastError == "" {
		t.Error("lastError must record the usage API failure")
	}
	if a.roll.UsagePercent != 12 {
		t.Errorf("last good rolling percent = %v, want 12", a.roll.UsagePercent)
	}
}

func TestApplyUsageRateLimitedStatusDemotes(t *testing.T) {
	a := newTestAccount("a", tierGoFree)
	data := testUsage(10, 20, 80)
	data.Weekly.Status = "rate-limited"
	a.applyUsage(data, time.Now(), 95)
	if a.tier != tierPayg {
		t.Errorf("a rate-limited window should demote to payg, got %s", a.tier)
	}
}

func TestApplyUsageFreshWindowsPromoteGoFree(t *testing.T) {
	a := newTestAccount("a", tierPayg)
	a.applyUsage(testUsage(4, 1, 0), time.Now(), 95)
	if a.tier != tierGoFree {
		t.Errorf("fresh windows should promote back to go_free, got %s", a.tier)
	}
}

// ─── picker: sticky + hysteresis, tier preference, PAYG round-robin, 401 ────

func twoAccountCfg() Config {
	return Config{
		HysteresisPoints: 8,
		TierSafePct:      95,
		Avoid401Cooldown: duration(2 * time.Minute),
		Accounts: []AccountCfg{
			{Name: "a", APIKey: "sk-a"},
			{Name: "b", APIKey: "sk-b"},
		},
	}
}
func setLoad(a *account, rollPct float64) {
	a.mu.Lock()
	a.roll = UsageWindow{UsagePercent: rollPct, Status: "ok", Present: true}
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
	for _, account := range p.accounts {
		account.mu.Lock()
		account.tier = tierPayg
		account.mu.Unlock()
	}

	want := []string{"a", "b", "a"}
	for i, name := range want {
		chosen, err := p.choose(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if chosen.cfg.Name != name {
			t.Errorf("choice %d = %s, want %s", i, chosen.cfg.Name, name)
		}
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

	cfg := Config{Upstream: upstream.URL, RequestTimeout: duration(10 * time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "sk-a"}}}
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

	cfg := Config{Upstream: upstream.URL, RequestTimeout: duration(10 * time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "sk-a"}}}
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
	cfg := Config{Upstream: "http://example.invalid", RequestTimeout: duration(time.Second), Avoid401Cooldown: duration(2 * time.Minute), Accounts: []AccountCfg{{Name: "a", APIKey: "k"}}}
	pc := newProxyCore(cfg)
	pc.picker.accounts[0].mark401(10*time.Minute, time.Now())

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	pc.handleProxy(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ─── N-account PAYG round-robin ─────────────────────────────────────────────

func TestPicker_RoundRobinN(t *testing.T) {
	cfg := Config{
		HysteresisPoints: 8,
		TierSafePct:      95,
		Avoid401Cooldown: duration(2 * time.Minute),
		Accounts: []AccountCfg{
			{Name: "a", APIKey: "sk-a"},
			{Name: "b", APIKey: "sk-b"},
			{Name: "c", APIKey: "sk-c"},
		},
	}
	p := newPicker(cfg)
	for _, account := range p.accounts {
		account.mu.Lock()
		account.tier = tierPayg
		account.mu.Unlock()
	}

	want := []string{"a", "b", "c", "a"}
	for i, name := range want {
		chosen, err := p.choose(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if chosen.cfg.Name != name {
			t.Errorf("choice %d = %s, want %s", i, chosen.cfg.Name, name)
		}
	}
}

func TestPicker_DisablePayg(t *testing.T) {
	cfg := Config{
		DisablePayg:      true,
		HysteresisPoints: 8,
		TierSafePct:      95,
		Avoid401Cooldown: duration(2 * time.Minute),
		Accounts: []AccountCfg{
			{Name: "a", APIKey: "sk-a"},
			{Name: "b", APIKey: "sk-b"},
		},
	}
	p := newPicker(cfg)
	a, b := p.accounts[0], p.accounts[1]
	a.mu.Lock()
	a.tier = tierPayg
	a.mu.Unlock()
	b.mu.Lock()
	b.tier = tierPayg
	b.mu.Unlock()

	_, err := p.choose(time.Now())
	if err == nil {
		t.Error("DisablePayg should reject when all accounts are PAYG")
	}
	// But a go_free account still works.
	a.mu.Lock()
	a.tier = tierGoFree
	a.mu.Unlock()
	chosen, err := p.choose(time.Now())
	if err != nil {
		t.Fatalf("go_free should still work with DisablePayg: %v", err)
	}
	if chosen.cfg.Name != "a" {
		t.Errorf("go_free should be chosen, got %s", chosen.cfg.Name)
	}
}

func TestLoadConfig_FreeEndpointSwitch(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		path := t.TempDir() + "/config.json"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("disabled by default", func(t *testing.T) {
		cfg, err := loadConfig(writeConfig(t, `{
			"accounts": [{"name": "a", "api_key": "sk-a"}]
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.FreeEndpointEnabled {
			t.Fatal("free endpoint should default to disabled")
		}
	})

	t.Run("explicitly enabled", func(t *testing.T) {
		cfg, err := loadConfig(writeConfig(t, `{
			"free_endpoint_enabled": true,
			"free_model_map": {"deepseek-v4-flash": "deepseek-v4-flash-free"},
			"accounts": [{"name": "a", "api_key": "sk-a"}]
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.FreeEndpointEnabled {
			t.Fatal("free endpoint switch was not loaded")
		}
	})

	t.Run("enabled requires model map", func(t *testing.T) {
		_, err := loadConfig(writeConfig(t, `{
			"free_endpoint_enabled": true,
			"accounts": [{"name": "a", "api_key": "sk-a"}]
		}`))
		if err == nil {
			t.Fatal("enabled free endpoint without a model map should fail")
		}
	})
}

func TestHandleProxy_FreeEndpointSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			freeCalls := 0
			goCalls := 0
			freeModel := ""
			goModel := ""

			freeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				freeCalls++
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				freeModel = body.Model
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"free","choices":[]}`))
			}))
			defer freeUpstream.Close()

			goUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				goCalls++
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				goModel = body.Model
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"go","cost":"0","choices":[]}`))
			}))
			defer goUpstream.Close()

			cfg := Config{
				Upstream:            goUpstream.URL,
				FreeEndpointEnabled: enabled,
				FreeUpstream:        freeUpstream.URL,
				FreeModelMap: map[string]string{
					"deepseek-v4-flash": "deepseek-v4-flash-free",
				},
				RequestTimeout:   duration(10 * time.Second),
				Avoid401Cooldown: duration(2 * time.Minute),
				Accounts:         []AccountCfg{{Name: "a", APIKey: "sk-a"}},
			}
			pc := newProxyCore(cfg)

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				bytes.NewBufferString(`{"model":"deepseek-v4-flash","stream":false}`),
			)
			rec := httptest.NewRecorder()
			pc.handleProxy(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			if enabled {
				if freeCalls != 1 || goCalls != 0 {
					t.Fatalf("calls free=%d go=%d, want free=1 go=0", freeCalls, goCalls)
				}
				if freeModel != "deepseek-v4-flash-free" {
					t.Fatalf("free model = %q", freeModel)
				}
			} else {
				if freeCalls != 0 || goCalls != 1 {
					t.Fatalf("calls free=%d go=%d, want free=0 go=1", freeCalls, goCalls)
				}
				if goModel != "deepseek-v4-flash" {
					t.Fatalf("Go model = %q", goModel)
				}
			}

			health := httptest.NewRecorder()
			pc.handleHealth(health, httptest.NewRequest(http.MethodGet, "/health", nil))
			var payload map[string]interface{}
			if err := json.Unmarshal(health.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["free_endpoint_enabled"] != enabled {
				t.Fatalf("health free_endpoint_enabled = %#v", payload["free_endpoint_enabled"])
			}
		})
	}
}
