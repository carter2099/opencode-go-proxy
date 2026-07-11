package main

import (
	"encoding/json"
	"flag"
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
		if acct.staleAlertInfo(now, realert) {
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
		"status":     statusString(snaps),
		"active_key": active,
		"accounts":   snaps,
		"upstream":   pc.upstream,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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