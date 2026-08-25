package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validUsageJSON = `{
  "usage": {
    "rolling": {"status":"ok","percent":42,"resetsAt":"2026-08-25T05:12:28.201Z"},
    "weekly": {"status":"ok","percent":17.5,"resetsAt":"2026-08-31T00:00:00.201Z"},
    "monthly": {"status":"rate-limited","percent":100,"resetsAt":"2026-09-07T19:31:19.195Z"}
  }
}`

func TestParseUsageResponse(t *testing.T) {
	data, err := parseUsageResponse([]byte(validUsageJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !data.Rolling.Present || data.Rolling.UsagePercent != 42 || data.Rolling.Status != "ok" {
		t.Errorf("rolling = %+v", data.Rolling)
	}
	if data.Weekly.UsagePercent != 17.5 {
		t.Errorf("weekly percent = %v, want 17.5", data.Weekly.UsagePercent)
	}
	if data.Monthly.Status != "rate-limited" || data.Monthly.UsagePercent != 100 {
		t.Errorf("monthly = %+v", data.Monthly)
	}
	wantReset, err := time.Parse(time.RFC3339Nano, "2026-08-25T05:12:28.201Z")
	if err != nil {
		t.Fatal(err)
	}
	if !data.Rolling.ResetsAt.Equal(wantReset) {
		t.Errorf("rolling reset = %s, want %s", data.Rolling.ResetsAt, wantReset)
	}
}

func TestParseUsageResponseRejectsIncompleteWindows(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing monthly",
			body: `{"usage":{
				"rolling":{"status":"ok","percent":0,"resetsAt":"2026-08-25T05:00:00Z"},
				"weekly":{"status":"ok","percent":0,"resetsAt":"2026-08-31T00:00:00Z"}
			}}`,
			want: "missing monthly window",
		},
		{
			name: "missing percent",
			body: `{"usage":{
				"rolling":{"status":"ok","resetsAt":"2026-08-25T05:00:00Z"},
				"weekly":{"status":"ok","percent":0,"resetsAt":"2026-08-31T00:00:00Z"},
				"monthly":{"status":"ok","percent":0,"resetsAt":"2026-09-20T00:00:00Z"}
			}}`,
			want: "missing rolling percent",
		},
		{
			name: "invalid status",
			body: `{"usage":{
				"rolling":{"status":"unknown","percent":0,"resetsAt":"2026-08-25T05:00:00Z"},
				"weekly":{"status":"ok","percent":0,"resetsAt":"2026-08-31T00:00:00Z"},
				"monthly":{"status":"ok","percent":0,"resetsAt":"2026-09-20T00:00:00Z"}
			}}`,
			want: `invalid rolling status "unknown"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseUsageResponse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestFetchUsageUsesInferenceAPIKey(t *testing.T) {
	var requestError string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/v1/usage":
			requestError = "path = " + r.URL.Path
		case r.Header.Get("Authorization") != "Bearer sk-test":
			requestError = "authorization header missing"
		case r.Header.Get("Accept") != "application/json":
			requestError = "accept header missing"
		case r.Header.Get("User-Agent") != usageUserAgent:
			requestError = "user-agent = " + r.Header.Get("User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(validUsageJSON))
	}))
	defer server.Close()

	data, err := fetchUsage(server.Client(), server.URL+"/", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if requestError != "" {
		t.Fatal(requestError)
	}
	if data.Rolling.UsagePercent != 42 {
		t.Errorf("rolling percent = %v, want 42", data.Rolling.UsagePercent)
	}
}

func TestFetchUsageSurfacesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`))
	}))
	defer server.Close()

	_, err := fetchUsage(server.Client(), server.URL, "bad-key")
	if err == nil || !strings.Contains(err.Error(), "usage API HTTP 401: Unauthorized") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRequiresOnlyNameAndAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"accounts":[{"name":"primary","api_key":"sk-test"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Name != "primary" {
		t.Fatalf("accounts = %+v", cfg.Accounts)
	}
}

func TestSnapshotWindowUsesAbsoluteReset(t *testing.T) {
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	got := snapshotWindow(UsageWindow{
		UsagePercent: 12,
		ResetsAt:     now.Add(90 * time.Second),
		Status:       "ok",
		Present:      true,
	}, now)
	if got.ResetIn != "1m" {
		t.Errorf("reset_in = %q, want 1m", got.ResetIn)
	}
}

func TestDurStr(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
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
