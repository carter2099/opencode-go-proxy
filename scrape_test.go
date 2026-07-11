package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// ─── parseBilling (mirrors the pi-go-bars TS unit tests exactly) ─────────────

func TestParseBilling_RealSSR(t *testing.T) {
	p, errstr := parseBilling(fixture(t, "billing.html"))
	if errstr != "" {
		t.Fatalf("unexpected error: %s", errstr)
	}
	if p.BalanceUsd != 19.9996075 { // 1999960750 / 1e8
		t.Errorf("balance = %v, want 19.9996075", p.BalanceUsd)
	}
	if p.MonthlyUsageUsd != 0.0003925 { // 39250 / 1e8
		t.Errorf("monthlyUsage = %v, want 0.0003925", p.MonthlyUsageUsd)
	}
	if p.MonthlyLimitUsd != 50 { // whole USD, no division
		t.Errorf("monthlyLimit = %v, want 50", p.MonthlyLimitUsd)
	}
}

func TestParseBilling_DecoyBalanceRejected(t *testing.T) {
	p, errstr := parseBilling(fixture(t, "decoy-balance.html"))
	if p.BalanceUsd != 0 {
		t.Errorf("must not adopt decoy balance, got %v", p.BalanceUsd)
	}
	if p.MonthlyLimitUsd != 0 {
		t.Errorf("must not adopt decoy limit, got %v", p.MonthlyLimitUsd)
	}
	if errstr == "" {
		t.Fatal("expected an error rejecting the decoy page")
	}
}

func TestParseBilling_LoginRedirect(t *testing.T) {
	p, errstr := parseBilling(fixture(t, "login-redirect.html"))
	if p.BalanceUsd != 0 {
		t.Errorf("balance = %v, want 0", p.BalanceUsd)
	}
	if errstr == "" {
		t.Fatal("expected error on login redirect")
	}
	if !contains(errstr, "no billing data on page") {
		t.Errorf("error = %q, want it to mention 'no billing data on page'", errstr)
	}
}

func TestParseBilling_SSRShapeChanged(t *testing.T) {
	html := `<div>monthlyLimit:50 monthlyUsage:0 balance:2000000000</div>` +
		`<script>window.x={monthlyLimit:50}</script>`
	_, errstr := parseBilling(html)
	if errstr == "" {
		t.Fatal("expected parser-outdated error")
	}
	if !contains(errstr, "parser may be outdated") {
		t.Errorf("error = %q", errstr)
	}
}

func TestParseBilling_NestedObjectDepthScan(t *testing.T) {
	html := `$R[25]={customerID:"cus_TEST",balance:1500000000,reload:!1,` +
		`reloadAmount:10,reloadTrigger:5,monthlyLimit:20,monthlyUsage:0,` +
		`lite:$R[27]={useBalance:!0}}`
	p, errstr := parseBilling(html)
	if errstr != "" {
		t.Fatalf("unexpected error: %s", errstr)
	}
	if p.BalanceUsd != 15 {
		t.Errorf("balance = %v, want 15", p.BalanceUsd)
	}
	if p.MonthlyLimitUsd != 20 {
		t.Errorf("monthlyLimit = %v, want 20", p.MonthlyLimitUsd)
	}
}

// ─── parseDashboard ──────────────────────────────────────────────────────────

func TestParseDashboard_ValidWindows(t *testing.T) {
	html := `rollingUsage:$R[2]={usagePercent:42,resetInSec:3600} ` +
		`weeklyUsage:$R[3]={resetInSec:604800,usagePercent:17} ` +
		`monthlyUsage:$R[4]={usagePercent:8,resetInSec:2592000}`
	d := parseDashboard(html)
	if d.Error != "" {
		t.Fatalf("unexpected error: %s", d.Error)
	}
	if !d.HasAny {
		t.Fatal("HasAny should be true")
	}
	if d.Rolling.UsagePercent != 42 || d.Rolling.ResetInSec != 3600 {
		t.Errorf("rolling = %+v, want 42/3600", d.Rolling)
	}
	// Field order independence: weekly had resetInSec first.
	if d.Weekly.UsagePercent != 17 || d.Weekly.ResetInSec != 604800 {
		t.Errorf("weekly = %+v, want 17/604800", d.Weekly)
	}
	if d.Monthly.UsagePercent != 8 || d.Monthly.ResetInSec != 2592000 {
		t.Errorf("monthly = %+v, want 8/2592000", d.Monthly)
	}
}

func TestParseDashboard_StatusField(t *testing.T) {
	html := `monthlyUsage:$R[4]={usagePercent:100,resetInSec:2592000,status:"rate-limited"} rollingUsage:$R[1]={usagePercent:12,resetInSec:3600}`
	d := parseDashboard(html)
	if !d.HasAny {
		t.Fatal("HasAny should be true")
	}
	if d.Monthly.Status != "rate-limited" {
		t.Errorf("monthly status = %q, want rate-limited", d.Monthly.Status)
	}
	if d.Rolling.Status != "" {
		t.Errorf("rolling status = %q, want empty (no status emitted)", d.Rolling.Status)
	}
}

func TestParseDashboard_MissingWindowsParserRot(t *testing.T) {
	html := `<html><body>rollingUsage weeklyUsage monthlyUsage</body></html>`
	d := parseDashboard(html)
	if d.HasAny {
		t.Error("HasAny should be false")
	}
	if d.Error == "" || !contains(d.Error, "parser may be outdated") {
		t.Errorf("error = %q, want parser-outdated", d.Error)
	}
}

func TestParseDashboard_LoginRedirectNoErrorMarker(t *testing.T) {
	// Login page has no rollingUsage markers at all → no error (just empty).
	html := `<html><body><h1>Sign in</h1></body></html>`
	d := parseDashboard(html)
	if d.HasAny {
		t.Error("HasAny should be false on login page")
	}
	// No markers → NOT parser rot; this is the "session expired upstream" signal
	// which fetchDashboard encodes as an error string instead.
	if d.Error != "" {
		t.Errorf("error = %q, want empty (no markers present)", d.Error)
	}
}

// ─── durStr ───────────────────────────────────────────────────────────────────

func TestDurStr(t *testing.T) {
	cases := []struct{ sec int64; want string }{
		{0, "now"},
		{30, "<1m"},
		{60, "1m"},
		{90, "1m"},
		{3600, "1h"},
		{3660, "1h 1m"},
		{86400, "1d"},
		{90000, "1d 1h"},
	}
	for _, c := range cases {
		if got := durStr(c.sec); got != c.want {
			t.Errorf("durStr(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestCostIsPayg(t *testing.T) {
	if costIsPayg("0") {
		t.Error("'0' is not payg")
	}
	if costIsPayg("0.0") {
		t.Error("'0.0' is not payg")
	}
	if !costIsPayg("0.00002260") {
		t.Error("'0.00002260' should be payg")
	}
	if !costIsPayg("0.00003140") {
		t.Error("'0.00003140' should be payg")
	}
	if costIsPayg("notanumber") {
		t.Error("non-numeric is not payg")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// avoid unused import warnings depending on subset of tests run.
var _ = time.Now
var _ = strconv.Atoi