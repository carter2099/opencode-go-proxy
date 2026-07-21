package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	configPath := flag.String("config", os.Getenv("HOME")+"/.config/opencode-go-proxy/config.json", "path to config.json")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	pc := newProxyCore(cfg)

	go scrapeLoop(pc)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", pc.handleHealth)
	mux.HandleFunc("/usage", pc.handleUsage)
	mux.HandleFunc("/", pc.handleProxy)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.RequestTimeout.Std(),
		WriteTimeout: cfg.RequestTimeout.Std(),
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("opencode-go-proxy starting on %s → %s (%d accounts)", cfg.ListenAddr, cfg.Upstream, len(cfg.Accounts))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// scrapeLoop is the proactive layer: polls /go + /billing per account on
// pollInterval, gated by scrapeCacheTTL so a tight loop won't hammer upstream.
func scrapeLoop(pc *proxyCore) {
	poll := pc.cfg.PollInterval.Std()
	if poll <= 0 {
		poll = 60 * time.Second
	}
	realert := time.Duration(pc.cfg.StaleRealertHours) * time.Hour

	// Kick off an initial scrape promptly.
	pc.scrapeAll(time.Now(), realert)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for now := range ticker.C {
		pc.scrapeAll(now, realert)
	}
}

func (pc *proxyCore) scrapeAll(now time.Time, realert time.Duration) {
	ttl := pc.cfg.ScrapeCacheTTL.Std()
	for _, acct := range pc.picker.accounts {
		if !acct.lastScrapeAt.IsZero() && now.Sub(acct.lastScrapeAt) < ttl {
			continue // cache gate
		}
		d := fetchDashboard(pc.scrapeClient, acct.cfg.WorkspaceID, acct.cfg.AuthCookie)
		var p PaygData
		if d.HasAny {
			p, _ = fetchBillingData(pc.scrapeClient, acct.cfg.WorkspaceID, acct.cfg.AuthCookie)
		}
		acct.applyScrape(d, p, now)
		if alertingEnabled(pc.cfg) && acct.staleAlertInfo(now, realert) {
			go func(a *account) {
				lastGood := a.lastScrapeAt
				if err := sendAlertEmail(pc.cfg, a, lastGood); err != nil {
					log.Printf("[%s] stale-cookie alert email failed: %v", a.cfg.Name, err)
				} else {
					log.Printf("[%s] cookie stale — alert email sent to %s", a.cfg.Name, pc.cfg.AlertEmail)
				}
			}(acct)
		}
	}
}

// handleHealth renders the /health JSON per the spec.
// aggregateUsage summarises billing across all accounts.
type aggregateUsage struct {
	TotalAccounts   int     `json:"total_accounts"`
	ActiveAccounts  int     `json:"active_accounts"`
	MaxRollingPct   float64 `json:"max_rolling_pct"`
	MaxWeeklyPct    float64 `json:"max_weekly_pct"`
	MaxMonthlyPct   float64 `json:"max_monthly_pct"`
	TotalPaygBalance float64 `json:"total_payg_balance_usd"`
	TotalPaygUsage  float64 `json:"total_payg_usage_usd"`
	AnyAvoided      bool    `json:"any_avoided"`
	AnyStale        bool    `json:"any_stale_cookie"`
}

func computeAggregate(snaps []snapshot) aggregateUsage {
	var a aggregateUsage
	a.TotalAccounts = len(snaps)
	for _, s := range snaps {
		if !s.Avoided {
			a.ActiveAccounts++
		} else {
			a.AnyAvoided = true
		}
		if !s.CookieFresh {
			a.AnyStale = true
		}
		if s.Rolling.Present && s.Rolling.Pct > a.MaxRollingPct {
			a.MaxRollingPct = s.Rolling.Pct
		}
		if s.Weekly.Present && s.Weekly.Pct > a.MaxWeeklyPct {
			a.MaxWeeklyPct = s.Weekly.Pct
		}
		if s.Monthly.Present && s.Monthly.Pct > a.MaxMonthlyPct {
			a.MaxMonthlyPct = s.Monthly.Pct
		}
		if s.Payg.Present {
			a.TotalPaygBalance += s.Payg.BalanceUsd
			a.TotalPaygUsage += s.Payg.MonthlyUsageUsd
		}
	}
	return a
}

// handleHealth renders the /health JSON per the spec.
func (pc *proxyCore) handleHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	acct, _ := pc.picker.choose(now)
	active := ""
	if acct != nil {
		active = acct.cfg.Name
	}
	snaps := make([]snapshot, 0, len(pc.picker.accounts))
	for _, a := range pc.picker.accounts {
		snaps = append(snaps, a.snapshot(now))
	}
	resp := map[string]interface{}{
		"status":       statusString(snaps),
		"active_key":   active,
		"accounts":     snaps,
		"aggregate":    computeAggregate(snaps),
		"upstream":     pc.upstream,
		"disable_payg": pc.cfg.DisablePayg,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleUsage returns a human-readable text/plain aggregate usage report.
func (pc *proxyCore) handleUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	snaps := make([]snapshot, 0, len(pc.picker.accounts))
	for _, a := range pc.picker.accounts {
		snaps = append(snaps, a.snapshot(now))
	}
	agg := computeAggregate(snaps)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// Header
	w.Write([]byte("OpenCode Go Proxy — Aggregate Usage\n"))
	w.Write([]byte("=====================================\n\n"))

	// Per-account
	for i, s := range snaps {
		avoided := ""
		if s.Avoided {
			avoided = " [AVOIDED]"
		}
		stale := ""
		if !s.CookieFresh {
			stale = " [STALE]"
		}
		fmt.Fprintf(w, "Account %d: %s (%s)%s%s\n", i+1, s.Name, s.Tier, avoided, stale)
		if s.Rolling.Present {
			fmt.Fprintf(w, "  5h:   %5.1f%%  reset in %s  [%s]\n", s.Rolling.Pct, s.Rolling.ResetIn, s.Rolling.Status)
		}
		if s.Weekly.Present {
			fmt.Fprintf(w, "  7d:   %5.1f%%  reset in %s  [%s]\n", s.Weekly.Pct, s.Weekly.ResetIn, s.Weekly.Status)
		}
		if s.Monthly.Present {
			fmt.Fprintf(w, "  30d:  %5.1f%%  reset in %s  [%s]\n", s.Monthly.Pct, s.Monthly.ResetIn, s.Monthly.Status)
		}
		if s.Payg.Present && s.Payg.BalanceUsd > 0 {
			fmt.Fprintf(w, "  PAYG: $%.2f balance, $%.2f/mo used of $%.2f\n", s.Payg.BalanceUsd, s.Payg.MonthlyUsageUsd, s.Payg.MonthlyLimitUsd)
		}
		fmt.Fprintf(w, "\n")
	}

	// Aggregate
	w.Write([]byte("── Aggregate ──\n"))
	fmt.Fprintf(w, "Accounts: %d total, %d active", agg.TotalAccounts, agg.ActiveAccounts)
	if agg.AnyAvoided {
		w.Write([]byte(" (some avoided)"))
	}
	if agg.AnyStale {
		w.Write([]byte(" [STALE COOKIE]"))
	}
	w.Write([]byte("\n"))
	fmt.Fprintf(w, "Peak usage:  5h %.1f%% | 7d %.1f%% | 30d %.1f%%\n", agg.MaxRollingPct, agg.MaxWeeklyPct, agg.MaxMonthlyPct)
	if agg.TotalPaygBalance > 0 {
		fmt.Fprintf(w, "PAYG:        $%.2f balance, $%.2f/mo usage\n", agg.TotalPaygBalance, agg.TotalPaygUsage)
	}
	fmt.Fprintf(w, "Status:      %s\n", statusString(snaps))
	fmt.Fprintf(w, "Upstream:    %s\n", pc.upstream)
}

func statusString(snaps []snapshot) string {
	allAvoided := true
	anyFresh := false
	for _, s := range snaps {
		if !s.Avoided {
			allAvoided = false
		}
		if s.CookieFresh {
			anyFresh = true
		}
	}
	switch {
	case allAvoided && len(snaps) > 0:
		return "degraded"
	case anyFresh:
		return "ok"
	default:
		return "initializing"
	}
}
func alertingEnabled(c Config) bool { return c.AlertEmail != "" && c.SMTPConfigPath != "" }