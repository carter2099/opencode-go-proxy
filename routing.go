package main

import (
	"strconv"
	"sync"
	"time"
)

// tier is the per-account billing classification the proxy steers on.
type tier int

const (
	tierGoFree tier = iota // Go subscription still has free headroom
	tierPayg               // Go exhausted; spending Zen pay-as-you-go ($25 cap)
)

func (t tier) String() string {
	switch t {
	case tierGoFree:
		return "go_free"
	case tierPayg:
		return "payg"
	}
	return "unknown"
}

// account is the full per-account runtime state, seeded from AccountCfg.
type account struct {
	cfg AccountCfg

	mu           sync.Mutex
	roll         UsageWindow
	week         UsageWindow
	mon          UsageWindow
	payg         PaygData
	tier         tier
	lastCost     string
	lastError    string
	cookieFresh  bool
	lastScrapeAt time.Time
	avoidedUntil time.Time

	// stale-cookie alert suppression: last time we emailed about this account.
	lastStaleAlert time.Time
	// everFresh tracks whether we've ever seen a fresh scrape (so the very first
	// transition into stale only fires after we know it was fresh once).
	everFresh bool

	// rrIndex is assigned by the proxy for round-robin ordering on PAYG.
	rrIndex int
}

// load is the steering pressure: the max usage percent across present windows.
func (a *account) load() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	mx := -1.0
	if a.roll.Present && a.roll.UsagePercent > mx {
		mx = a.roll.UsagePercent
	}
	if a.week.Present && a.week.UsagePercent > mx {
		mx = a.week.UsagePercent
	}
	if a.mon.Present && a.mon.UsagePercent > mx {
		mx = a.mon.UsagePercent
	}
	if mx < 0 {
		return 0 // unknown load — treat as 0 so an unknown account is preferred
	}
	return mx
}

// isAvoided reports whether the key is in 401 cooldown right now.
func (a *account) isAvoided(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Before(a.avoidedUntil)
}

// mark401 starts the 401 cooldown window.
func (a *account) mark401(cooldown time.Duration, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.avoidedUntil = now.Add(cooldown)
}

// clear401 clears the cooldown (called on a subsequent 200 from this account).
func (a *account) clear401(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.After(a.avoidedUntil) || !a.avoidedUntil.IsZero() {
		a.avoidedUntil = time.Time{}
	}
}

// applyScrape merges freshly-scraped usage + billing into the account, updates
// the tier from proactive signals, and reports the fresh→stale transition so
// the caller can email the alert.
func (a *account) applyScrape(d UsageData, p PaygData, now time.Time) (wentStale bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d.Error != "" || !d.HasAny {
		// scrape failed / login redirect
		a.lastError = d.Error
		wasFresh := a.everFresh && a.cookieFresh
		a.cookieFresh = false
		return wasFresh // true only if we actually flipped fresh→stale
	}
	a.lastError = ""
	a.roll = d.Rolling
	a.week = d.Weekly
	a.mon = d.Monthly
	a.payg = p
	a.lastScrapeAt = now
	a.cookieFresh = true
	a.everFresh = true
	// Proactive tier recompute from scrape alone. The reactive cost-flip can
	// only demote (handled in applyCost); scrape can promote back when a window
	// resets, UNLESS a prior reactive demotion set lastCost>0 and we haven't yet
	// seen a cost==0 response. We let scrape promote: if the heaviest window is
	// under the safe threshold AND its status isn't rate-limited, consider free.
	heaviest := maxWindow(a.roll, a.week, a.mon)
	if heaviest.Present && heaviest.UsagePercent < tierSafeThreshold && heaviest.Status != "rate-limited" {
		// Only promote if we're not currently flagged PAYG by a recent reactive
		// hit whose cost we haven't cleared. Clearing requires scrape to show
		// the window reset — which is exactly this case.
		a.tier = tierGoFree
	} else if heaviest.Present && (heaviest.UsagePercent >= 100 || heaviest.Status == "rate-limited") {
		a.tier = tierPayg
	}
	return false
}

// tierSafeThreshold is the load boundary for proactive go_free classification.
const tierSafeThreshold = 95.0

func maxWindow(ws ...UsageWindow) UsageWindow {
	var m UsageWindow
	for _, w := range ws {
		if w.Present && w.UsagePercent > m.UsagePercent {
			m = w
		}
	}
	return m
}

// applyCost is the reactive override: a 200 response's top-level `cost` field
// is ground truth. cost>0 on a go_free account demotes it immediately.
func (a *account) applyCost(cost string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCost = cost
	if costIsPayg(cost) && a.tier == tierGoFree {
		a.tier = tierPayg // reactive demote — stale scrape can't cost you free tokens
	}
}

// clear401On200 resets 401 cooldown when a 200 lands.
func (a *account) clear401On200(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.avoidedUntil = time.Time{}
}

// staleAlertInfo decides whether a cookie-stale alert should fire now — both the
// fresh→stale transition (lastStaleAlert zero) and the periodic re-alert. It
// atomically stamps lastStaleAlert when it returns true, so the caller just
// sends the email on a true return.
func (a *account) staleAlertInfo(now time.Time, realert time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cookieFresh || !a.everFresh {
		return false
	}
	if a.lastStaleAlert.IsZero() || now.Sub(a.lastStaleAlert) >= realert {
		a.lastStaleAlert = now
		return true
	}
	return false
}

// costIsPayg reports whether the cost field indicates pay-as-you-go charged.
func costIsPayg(cost string) bool {
	f, err := strconv.ParseFloat(cost, 64)
	return err == nil && f > 0
}

// snapshot is an immutable read of account state for /health rendering.
type snapshot struct {
	Name          string    `json:"name"`
	Tier          string    `json:"tier"`
	APIKeyTail    string    `json:"api_key_tail"`
	Rolling       windowJSON `json:"rolling"`
	Weekly        windowJSON `json:"weekly"`
	Monthly       windowJSON `json:"monthly"`
	Payg          paygJSON   `json:"payg"`
	LastCost      string     `json:"last_cost"`
	LastError     string     `json:"last_error"`
	CookieFresh   bool       `json:"cookie_fresh"`
	LastScrapeAt  *time.Time `json:"last_scrape_at,omitempty"`
	Avoided       bool       `json:"avoided"`
	AvoidedUntil  *time.Time `json:"avoided_until,omitempty"`
}

type windowJSON struct {
	Pct      float64 `json:"pct"`
	ResetIn  string  `json:"reset_in"`
	Status   string  `json:"status"`
	Present  bool    `json:"present"`
}

type paygJSON struct {
	BalanceUsd      float64 `json:"balance_usd"`
	MonthlyUsageUsd float64 `json:"monthly_usage_usd"`
	MonthlyLimitUsd float64 `json:"monthly_limit_usd"`
	Present         bool    `json:"present"`
}

func (a *account) snapshot(now time.Time) snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := snapshot{
		Name:       a.cfg.Name,
		Tier:       a.tier.String(),
		APIKeyTail: tail(a.cfg.APIKey, 4),
		Rolling:    windowJSON{Pct: a.roll.UsagePercent, ResetIn: durStr(a.roll.ResetInSec), Status: a.roll.Status, Present: a.roll.Present},
		Weekly:     windowJSON{Pct: a.week.UsagePercent, ResetIn: durStr(a.week.ResetInSec), Status: a.week.Status, Present: a.week.Present},
		Monthly:    windowJSON{Pct: a.mon.UsagePercent, ResetIn: durStr(a.mon.ResetInSec), Status: a.monthStatusLocked(), Present: a.mon.Present},
		Payg:       paygJSON{BalanceUsd: a.payg.BalanceUsd, MonthlyUsageUsd: a.payg.MonthlyUsageUsd, MonthlyLimitUsd: a.payg.MonthlyLimitUsd, Present: a.payg.HasAny},
		LastCost:   a.lastCost,
		LastError:  a.lastError,
		CookieFresh: a.cookieFresh,
		LastScrapeAt: tptr(a.lastScrapeAt),
		Avoided:    now.Before(a.avoidedUntil),
		AvoidedUntil: tptr(a.avoidedUntil),
	}
	return s
}

// monthStatusLocked reads a.mon.Status under the already-held lock.
func (a *account) monthStatusLocked() string {
	return a.mon.Status
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func durStr(sec int64) string {
	if sec <= 0 {
		return "now"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0 && h > 0:
		return strconv.FormatInt(d, 10) + "d " + strconv.FormatInt(h, 10) + "h"
	case d > 0:
		return strconv.FormatInt(d, 10) + "d"
	case h > 0 && m > 0:
		return strconv.FormatInt(h, 10) + "h " + strconv.FormatInt(m, 10) + "m"
	case h > 0:
		return strconv.FormatInt(h, 10) + "h"
	case m > 0:
		return strconv.FormatInt(m, 10) + "m"
	}
	return "<1m"
}