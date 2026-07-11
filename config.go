package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the on-disk JSON config at ~/.config/opencode-go-proxy/config.json.
type Config struct {
	ListenAddr         string        `json:"listen_addr"`
	Upstream           string        `json:"upstream"`
	PollInterval       duration      `json:"poll_interval"`
	ScrapeCacheTTL     duration      `json:"scrape_cache_ttl"`
	HysteresisPoints   float64       `json:"hysteresis_points"`
	TierSafePct        float64       `json:"tier_safe_pct"`
	AlertEmail         string        `json:"alert_email"`
	SMTPConfigPath     string        `json:"smtp_config_path"`
	StaleRealertHours  int           `json:"stale_realert_hours"`
	Avoid401Cooldown   duration      `json:"avoid_401_cooldown"`
	RequestTimeout     duration      `json:"request_timeout"`
	Accounts           []AccountCfg  `json:"accounts"`
}

// AccountCfg is the static credential block per OpenCode Go account.
type AccountCfg struct {
	Name        string `json:"name"`
	APIKey      string `json:"api_key"`
	WorkspaceID string `json:"workspace_id"`
	AuthCookie  string `json:"auth_cookie"`
}

// duration is a JSON string parsed via time.ParseDuration.
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = duration(v)
	return nil
}

func (d duration) Std() time.Duration { return time.Duration(d) }

func defaultConfig() Config {
	return Config{
		ListenAddr:        "127.0.0.1:8082",
		Upstream:          "https://opencode.ai/zen/go",
		PollInterval:      duration(60 * time.Second),
		ScrapeCacheTTL:    duration(90 * time.Second),
		HysteresisPoints:  8,
		TierSafePct:       95,
		AlertEmail:        "carter2099@pm.me",
		SMTPConfigPath:    "/home/carter/scripts/.smtp_config",
		StaleRealertHours: 24,
		Avoid401Cooldown:  duration(2 * time.Minute),
		RequestTimeout:    duration(10 * time.Minute),
	}
}

// loadConfig reads + validates the JSON config, applying defaults for any
// zero-valued optional fields.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	// Unmarshal into a temporary so json's zero-vs-absent distinction can't
	// erase a default; we overlay explicitly via a second pass for optional
	// scalars. For struct fields absent in JSON, the default already sits in
	// cfg, so we unmarshal ON TOP of cfg's defaults.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Re-apply defaults for fields whose zero value is a meaningful setting.
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8082"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = "https://opencode.ai/zen/go"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = duration(60 * time.Second)
	}
	if cfg.ScrapeCacheTTL == 0 {
		cfg.ScrapeCacheTTL = duration(90 * time.Second)
	}
	if cfg.HysteresisPoints == 0 {
		cfg.HysteresisPoints = 8
	}
	if cfg.TierSafePct == 0 {
		cfg.TierSafePct = 95
	}
	if cfg.AlertEmail == "" {
		cfg.AlertEmail = "carter2099@pm.me"
	}
	if cfg.SMTPConfigPath == "" {
		cfg.SMTPConfigPath = "/home/carter/scripts/.smtp_config"
	}
	if cfg.StaleRealertHours == 0 {
		cfg.StaleRealertHours = 24
	}
	if cfg.Avoid401Cooldown == 0 {
		cfg.Avoid401Cooldown = duration(2 * time.Minute)
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = duration(10 * time.Minute)
	}
	if len(cfg.Accounts) == 0 {
		return cfg, fmt.Errorf("config %s: no accounts configured", path)
	}
	for i, a := range cfg.Accounts {
		if a.Name == "" || a.APIKey == "" || a.WorkspaceID == "" || a.AuthCookie == "" {
			return cfg, fmt.Errorf("config %s: account[%d] missing one of name/api_key/workspace_id/auth_cookie", path, i)
		}
	}
	return cfg, nil
}