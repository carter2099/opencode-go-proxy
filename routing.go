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
	tierPayg               // Go exhausted; spending Zen pay-as-you-go balance
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
	tier         tier
	lastCost     string
	lastError    string
	usageFresh   bool
	lastUsageAt  time.Time
	avoidedUntil time.Time

	// rrIndex is the stable account index assigned at startup, used for
	// sticky tracking and PAYG round-robin.
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

// applyUsage merges a successful API response and updates the proactive tier.
func (a *account) applyUsage(d UsageData, now time.Time, safePct float64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.lastError = ""
	a.roll = d.Rolling
	a.week = d.Weekly
	a.mon = d.Monthly
	a.lastUsageAt = now
	a.usageFresh = true

	heaviest := maxWindow(a.roll, a.week, a.mon)
	switch {
	case anyRateLimited(a.roll, a.week, a.mon):
		a.tier = tierPayg
	case heaviest.Present && heaviest.UsagePercent >= 100:
		a.tier = tierPayg
	case heaviest.Present && heaviest.UsagePercent < safePct:
		a.tier = tierGoFree
	}
}

// markUsageError preserves the last good windows while surfacing stale API data.
func (a *account) markUsageError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageFresh = false
	a.lastError = err.Error()
}

func maxWindow(ws ...UsageWindow) UsageWindow {
	var m UsageWindow
	for _, w := range ws {
		if w.Present && w.UsagePercent > m.UsagePercent {
			m = w
		}
	}
	return m
}

func anyRateLimited(ws ...UsageWindow) bool {
	for _, w := range ws {
		if w.Present && w.Status == "rate-limited" {
			return true
		}
	}
	return false
}

// applyCost is the reactive override: a 200 response's top-level `cost` field
// is ground truth. cost>0 on a go_free account demotes it immediately.
func (a *account) applyCost(cost string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCost = cost
	if costIsPayg(cost) && a.tier == tierGoFree {
		a.tier = tierPayg // reactive demote — stale usage data cannot hide paid requests
	}
}

// clear401On200 resets 401 cooldown when a 200 lands.
func (a *account) clear401On200(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.avoidedUntil = time.Time{}
}

// costIsPayg reports whether the cost field indicates pay-as-you-go charged.
func costIsPayg(cost string) bool {
	f, err := strconv.ParseFloat(cost, 64)
	return err == nil && f > 0
}

// snapshot is an immutable read of account state for /health rendering.
type snapshot struct {
	Name         string     `json:"name"`
	Tier         string     `json:"tier"`
	APIKeyTail   string     `json:"api_key_tail"`
	Rolling      windowJSON `json:"rolling"`
	Weekly       windowJSON `json:"weekly"`
	Monthly      windowJSON `json:"monthly"`
	LastCost     string     `json:"last_cost"`
	LastError    string     `json:"last_error"`
	UsageFresh   bool       `json:"usage_fresh"`
	LastUsageAt  *time.Time `json:"last_usage_at,omitempty"`
	Avoided      bool       `json:"avoided"`
	AvoidedUntil *time.Time `json:"avoided_until,omitempty"`
}

type windowJSON struct {
	Pct     float64 `json:"pct"`
	ResetIn string  `json:"reset_in"`
	Status  string  `json:"status"`
	Present bool    `json:"present"`
}

func (a *account) snapshot(now time.Time) snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return snapshot{
		Name:         a.cfg.Name,
		Tier:         a.tier.String(),
		APIKeyTail:   tail(a.cfg.APIKey, 4),
		Rolling:      snapshotWindow(a.roll, now),
		Weekly:       snapshotWindow(a.week, now),
		Monthly:      snapshotWindow(a.mon, now),
		LastCost:     a.lastCost,
		LastError:    a.lastError,
		UsageFresh:   a.usageFresh,
		LastUsageAt:  tptr(a.lastUsageAt),
		Avoided:      now.Before(a.avoidedUntil),
		AvoidedUntil: tptr(a.avoidedUntil),
	}
}

func snapshotWindow(window UsageWindow, now time.Time) windowJSON {
	resetIn := ""
	if window.Present && !window.ResetsAt.IsZero() {
		resetIn = durStr(int64(window.ResetsAt.Sub(now).Seconds()))
	}
	return windowJSON{
		Pct:     window.UsagePercent,
		ResetIn: resetIn,
		Status:  window.Status,
		Present: window.Present,
	}
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
