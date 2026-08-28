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

	go usageLoop(pc)
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
	log.Printf("opencode-go-proxy starting on %s → %s (%d accounts, free endpoint=%t)", cfg.ListenAddr, cfg.Upstream, len(cfg.Accounts), cfg.FreeEndpointEnabled)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// usageLoop polls OpenCode's authenticated usage API for every account.
func usageLoop(pc *proxyCore) {
	poll := pc.cfg.PollInterval.Std()
	if poll <= 0 {
		poll = 60 * time.Second
	}

	pc.pollUsage(time.Now())
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for now := range ticker.C {
		pc.pollUsage(now)
	}
}

func (pc *proxyCore) pollUsage(now time.Time) {
	for _, acct := range pc.picker.accounts {
		data, err := fetchUsage(pc.usageClient, pc.upstream, acct.cfg.APIKey)
		if err != nil {
			acct.markUsageError(err)
			log.Printf("[%s] usage poll failed: %v", acct.cfg.Name, err)
			continue
		}
		acct.applyUsage(data, now, pc.cfg.TierSafePct)
	}
}

// aggregateUsage summarises quota pressure across all accounts.
type aggregateUsage struct {
	TotalAccounts  int     `json:"total_accounts"`
	ActiveAccounts int     `json:"active_accounts"`
	MaxRollingPct  float64 `json:"max_rolling_pct"`
	MaxWeeklyPct   float64 `json:"max_weekly_pct"`
	MaxMonthlyPct  float64 `json:"max_monthly_pct"`
	AnyAvoided     bool    `json:"any_avoided"`
	AnyUsageStale  bool    `json:"any_usage_stale"`
}

func computeAggregate(snaps []snapshot) aggregateUsage {
	var aggregate aggregateUsage
	aggregate.TotalAccounts = len(snaps)
	for _, snap := range snaps {
		if !snap.Avoided {
			aggregate.ActiveAccounts++
		} else {
			aggregate.AnyAvoided = true
		}
		if !snap.UsageFresh {
			aggregate.AnyUsageStale = true
		}
		if snap.Rolling.Present && snap.Rolling.Pct > aggregate.MaxRollingPct {
			aggregate.MaxRollingPct = snap.Rolling.Pct
		}
		if snap.Weekly.Present && snap.Weekly.Pct > aggregate.MaxWeeklyPct {
			aggregate.MaxWeeklyPct = snap.Weekly.Pct
		}
		if snap.Monthly.Present && snap.Monthly.Pct > aggregate.MaxMonthlyPct {
			aggregate.MaxMonthlyPct = snap.Monthly.Pct
		}
	}
	return aggregate
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
		"status":                statusString(snaps),
		"active_key":            active,
		"accounts":              snaps,
		"aggregate":             computeAggregate(snaps),
		"upstream":              pc.upstream,
		"disable_payg":          pc.cfg.DisablePayg,
		"free_endpoint_enabled": pc.cfg.FreeEndpointEnabled,
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
		if !s.UsageFresh {
			stale = " [USAGE STALE]"
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
		fmt.Fprintf(w, "\n")
	}

	// Aggregate
	w.Write([]byte("── Aggregate ──\n"))
	fmt.Fprintf(w, "Accounts: %d total, %d active", agg.TotalAccounts, agg.ActiveAccounts)
	if agg.AnyAvoided {
		w.Write([]byte(" (some avoided)"))
	}
	if agg.AnyUsageStale {
		w.Write([]byte(" [USAGE STALE]"))
	}
	w.Write([]byte("\n"))
	fmt.Fprintf(w, "Peak usage:  5h %.1f%% | 7d %.1f%% | 30d %.1f%%\n", agg.MaxRollingPct, agg.MaxWeeklyPct, agg.MaxMonthlyPct)
	fmt.Fprintf(w, "Status:      %s\n", statusString(snaps))
	fmt.Fprintf(w, "Upstream:    %s\n", pc.upstream)
	fmt.Fprintf(w, "Free endpoint: %t\n", pc.cfg.FreeEndpointEnabled)
}

func statusString(snaps []snapshot) string {
	if len(snaps) == 0 {
		return "initializing"
	}
	allAvoided := true
	allFresh := true
	anyFresh := false
	for _, snap := range snaps {
		if !snap.Avoided {
			allAvoided = false
		}
		if snap.UsageFresh {
			anyFresh = true
		} else {
			allFresh = false
		}
	}
	switch {
	case allAvoided:
		return "degraded"
	case allFresh:
		return "ok"
	case anyFresh:
		return "degraded"
	default:
		return "initializing"
	}
}
