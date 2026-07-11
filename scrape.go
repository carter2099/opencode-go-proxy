package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ─── Dashboard /go usage parsing ─────────────────────────────────────────────

// UsageWindow is one rolling/weekly/monthly usage bucket.
type UsageWindow struct {
	UsagePercent float64
	ResetInSec   int64
	Status       string // "ok" / "rate-limited" / "" (unknown)
	Present      bool   // false ⇒ scrape yielded no data for this window
}

// Present windows always carry a status if the dashboard emitted one; an older
// dashboard that only emits usagePercent/resetInSec yields Status="".

// UsageData is the parsed /go dashboard state per account.
type UsageData struct {
	Rolling  UsageWindow
	Weekly   UsageWindow
	Monthly  UsageWindow
	HasAny   bool   // at least one window present
	Error    string // parser-rot / login message
	FetchedAt time.Time
}

// numRE matches a (possibly signed, possibly fractional) number in SSR output.
var numRE = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// windowObjRE captures the body of `name:$R[N]={...}` (no nested braces in
// the usage objects, so a flat [^}]* capture is safe).
func windowObjRE(name string) *regexp.Regexp {
	return regexp.MustCompile(name + `:\$R\[\d+\]=\{([^}]*)\}`)
}

var (
	reUsagePercent = regexp.MustCompile(`usagePercent:(-?\d+(?:\.\d+)?)`)
	reResetInSec   = regexp.MustCompile(`resetInSec:(-?\d+(?:\.\d+)?)`)
	reStatus       = regexp.MustCompile(`status:"?(ok|rate-limited)"?`)
)

func parseWindowBody(body string) (UsageWindow, bool) {
	pct := reUsagePercent.FindStringSubmatch(body)
	reset := reResetInSec.FindStringSubmatch(body)
	if pct == nil || reset == nil {
		return UsageWindow{}, false
	}
	w := UsageWindow{Present: true}
	w.UsagePercent, _ = strconv.ParseFloat(pct[1], 64)
	rsec, _ := strconv.ParseFloat(reset[1], 64)
	w.ResetInSec = int64(rsec)
	if st := reStatus.FindStringSubmatch(body); st != nil {
		w.Status = st[1]
	}
	return w, true
}

// looksLikeDashboard detects SSR hydration markers; used to distinguish a
// parser-format change from a login redirect.
func looksLikeDashboard(html string) bool {
	return strings.Contains(html, "rollingUsage") ||
		strings.Contains(html, "weeklyUsage") ||
		strings.Contains(html, "monthlyUsage")
}

// parseDashboard parses SolidJS SSR HTML from /workspace/<id>/go into per-window usage.
func parseDashboard(html string) UsageData {
	rollBody := windowObjRE("rollingUsage").FindStringSubmatch(html)
	weekBody := windowObjRE("weeklyUsage").FindStringSubmatch(html)
	monBody := windowObjRE("monthlyUsage").FindStringSubmatch(html)

	var d UsageData
	d.FetchedAt = time.Now()
	if rollBody != nil {
		d.Rolling, _ = parseWindowBody(rollBody[1])
	}
	if weekBody != nil {
		d.Weekly, _ = parseWindowBody(weekBody[1])
	}
	if monBody != nil {
		d.Monthly, _ = parseWindowBody(monBody[1])
	}
	d.HasAny = d.Rolling.Present || d.Weekly.Present || d.Monthly.Present
	if !d.HasAny && looksLikeDashboard(html) {
		d.Error = "parser may be outdated — dashboard markers present but no usage objects matched"
	}
	return d
}

// ─── Billing /billing parsing ────────────────────────────────────────────────

// PaygData is the parsed Zen pay-as-you-go billing state per account.
type PaygData struct {
	BalanceUsd       float64
	MonthlyUsageUsd  float64
	MonthlyLimitUsd  float64
	HasAny           bool
	FetchedAt        time.Time
}

// microcents → USD: balance & monthlyUsage are stored server-side in 1e-8 USD.
const microcents = 1e8

var (
	reBillingBalance       = regexp.MustCompile(`balance:(-?\d+(?:\.\d+)?)`)
	reBillingMonthlyUsage  = regexp.MustCompile(`monthlyUsage:(-?\d+(?:\.\d+)?)`)
	reBillingMonthlyLimit  = regexp.MustCompile(`monthlyLimit:(-?\d+(?:\.\d+)?)`)
)

// extractBillingObject returns the billing object body anchored on
// `customerID:"cus_..."`, spanning to its matching closing brace via a depth
// count so a nested `lite:$R[27]={...}` inside the body is handled correctly.
func extractBillingObject(html string) string {
	anchor := `customerID:"cus_`
	start := strings.Index(html, anchor)
	if start == -1 {
		return ""
	}
	braceStart := strings.LastIndex(html[:start], "{")
	if braceStart == -1 {
		return ""
	}
	depth := 0
	for i := braceStart; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[braceStart : i+1]
			}
		}
	}
	return ""
}

func looksLikeBilling(html string) bool {
	return strings.Contains(html, "monthlyLimit") ||
		strings.Contains(html, "monthlyUsage") ||
		regexp.MustCompile(`balance:-?\d`).MatchString(html)
}

func numOrNil(body string, re *regexp.Regexp) (float64, bool) {
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// parseBilling parses SolidJS SSR HTML from /workspace/<id>/billing.
// Returns PaygData and an error string (parser rot vs login redirect).
func parseBilling(html string) (PaygData, string) {
	var p PaygData
	p.FetchedAt = time.Now()
	obj := extractBillingObject(html)
	if obj == "" {
		if looksLikeBilling(html) {
			return p, "billing parser may be outdated — update opencode-go-proxy"
		}
		return p, "no billing data on page"
	}
	bal, okB := numOrNil(obj, reBillingBalance)
	mu, okMU := numOrNil(obj, reBillingMonthlyUsage)
	ml, okML := numOrNil(obj, reBillingMonthlyLimit)
	if !okB && !okMU && !okML {
		return p, "billing parser may be outdated — update opencode-go-proxy"
	}
	p.HasAny = true
	if okB {
		p.BalanceUsd = bal / microcents
	}
	if okMU {
		p.MonthlyUsageUsd = mu / microcents
	}
	if okML {
		p.MonthlyLimitUsd = ml
	}
	return p, ""
}

// ─── HTTP fetch ──────────────────────────────────────────────────────────────

const (
	dashboardUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Gecko/20100101 Firefox/148.0"
	scrapeTimeout      = 10 * time.Second
)

func dashboardURL(workspaceID string) string {
	return "https://opencode.ai/workspace/" + workspaceID + "/go"
}
func billingURL(workspaceID string) string {
	return "https://opencode.ai/workspace/" + workspaceID + "/billing"
}

// fetchDashboard fetches + parses /go. The returned UsageData.Error is set on
// any failure (network, redirect-to-login, parser rot); HasAny remains false then.
func fetchDashboard(client *http.Client, workspaceID, authCookie string) UsageData {
	var d UsageData
	d.FetchedAt = time.Now()
	req, err := http.NewRequest("GET", dashboardURL(workspaceID), nil)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	req.Header.Set("Cookie", "auth="+authCookie)
	req.Header.Set("User-Agent", dashboardUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d.Error = fmt.Sprintf("HTTP %d %s", resp.StatusCode, resp.Status)
		return d
	}
	// Redirect-to-login guard: the final URL must still contain the workspace path.
	if !strings.Contains(resp.Request.URL.Path, "/workspace/"+workspaceID+"/go") {
		d.Error = "session expired or auth invalid — refresh cookie"
		return d
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d = parseDashboard(string(body))
	return d
}

// fetchBillingData fetches + parses /billing. Returns the parsed PaygData and
// an error string.
func fetchBillingData(client *http.Client, workspaceID, authCookie string) (PaygData, string) {
	req, err := http.NewRequest("GET", billingURL(workspaceID), nil)
	var p PaygData
	p.FetchedAt = time.Now()
	if err != nil {
		return p, err.Error()
	}
	req.Header.Set("Cookie", "auth="+authCookie)
	req.Header.Set("User-Agent", dashboardUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return p, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return p, fmt.Sprintf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	if !strings.Contains(resp.Request.URL.Path, "/workspace/"+workspaceID+"/billing") {
		return p, "session expired or auth invalid — refresh cookie"
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return p, err.Error()
	}
	return parseBilling(string(body))
}

// _ keeps numRE referenced if future debug paths use it.
var _ = numRE