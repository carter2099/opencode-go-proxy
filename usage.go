package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	usageUserAgent = "opencode-go-proxy/1.0"
	usageTimeout   = 10 * time.Second
	maxUsageBody   = 1 << 20
)

// UsageWindow is one rolling, weekly, or monthly quota window.
type UsageWindow struct {
	UsagePercent float64
	ResetsAt     time.Time
	Status       string
	Present      bool
}

// UsageData is the quota state returned by OpenCode's authenticated usage API.
type UsageData struct {
	Rolling UsageWindow
	Weekly  UsageWindow
	Monthly UsageWindow
}

type usageAPIResponse struct {
	Usage *usageAPIWindows `json:"usage"`
}

type usageAPIWindows struct {
	Rolling *usageAPIWindow `json:"rolling"`
	Weekly  *usageAPIWindow `json:"weekly"`
	Monthly *usageAPIWindow `json:"monthly"`
}

type usageAPIWindow struct {
	Status   string     `json:"status"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resetsAt"`
}

func usageURL(upstream string) string {
	return strings.TrimRight(upstream, "/") + "/v1/usage"
}

// fetchUsage reads quota windows using the same API key used for inference.
func fetchUsage(client *http.Client, upstream, apiKey string) (UsageData, error) {
	req, err := http.NewRequest(http.MethodGet, usageURL(upstream), nil)
	if err != nil {
		return UsageData{}, fmt.Errorf("build usage API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", usageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return UsageData{}, fmt.Errorf("usage API request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	if err != nil {
		return UsageData{}, fmt.Errorf("read usage API response: %w", err)
	}
	if len(body) > maxUsageBody {
		return UsageData{}, fmt.Errorf("usage API response exceeds %d bytes", maxUsageBody)
	}
	if resp.StatusCode != http.StatusOK {
		return UsageData{}, usageHTTPError(resp.StatusCode, body)
	}
	return parseUsageResponse(body)
}

func usageHTTPError(status int, body []byte) error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		return fmt.Errorf("usage API HTTP %d: %s", status, payload.Error.Message)
	}
	detail := strings.Join(strings.Fields(string(body)), " ")
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if detail == "" {
		return fmt.Errorf("usage API HTTP %d", status)
	}
	return fmt.Errorf("usage API HTTP %d: %s", status, detail)
}

func parseUsageResponse(body []byte) (UsageData, error) {
	var payload usageAPIResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageData{}, fmt.Errorf("decode usage API response: %w", err)
	}
	if payload.Usage == nil {
		return UsageData{}, fmt.Errorf("decode usage API response: missing usage")
	}

	rolling, err := parseUsageWindow("rolling", payload.Usage.Rolling)
	if err != nil {
		return UsageData{}, err
	}
	weekly, err := parseUsageWindow("weekly", payload.Usage.Weekly)
	if err != nil {
		return UsageData{}, err
	}
	monthly, err := parseUsageWindow("monthly", payload.Usage.Monthly)
	if err != nil {
		return UsageData{}, err
	}
	return UsageData{Rolling: rolling, Weekly: weekly, Monthly: monthly}, nil
}

func parseUsageWindow(name string, window *usageAPIWindow) (UsageWindow, error) {
	if window == nil {
		return UsageWindow{}, fmt.Errorf("decode usage API response: missing %s window", name)
	}
	if window.Percent == nil {
		return UsageWindow{}, fmt.Errorf("decode usage API response: missing %s percent", name)
	}
	if *window.Percent < 0 {
		return UsageWindow{}, fmt.Errorf("decode usage API response: invalid %s percent %v", name, *window.Percent)
	}
	if window.ResetsAt == nil || window.ResetsAt.IsZero() {
		return UsageWindow{}, fmt.Errorf("decode usage API response: missing %s resetsAt", name)
	}
	if window.Status != "ok" && window.Status != "rate-limited" {
		return UsageWindow{}, fmt.Errorf("decode usage API response: invalid %s status %q", name, window.Status)
	}
	return UsageWindow{
		UsagePercent: *window.Percent,
		ResetsAt:     *window.ResetsAt,
		Status:       window.Status,
		Present:      true,
	}, nil
}
