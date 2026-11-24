package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeUsageRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer test-claude-secret" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || !strings.HasPrefix(r.UserAgent(), "claude-cli/") {
			t.Errorf("unexpected Claude usage request shape")
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":12.5,"resets_at":null},"seven_day":{"utilization":42,"resets_at":"2030-10-11T23:59:59.572809+00:00"},"seven_day_opus":{"utilization":20,"resets_at":"invalid","locked_reason":"policy"},"numeric_reset":{"utilization":5,"resets_at":123},"tangelo":{"utilization":100},"iguana_necktie":{"utilization":0.0,"resets_at":"2030-11-05T07:59:00+00:00","limit_dollars":250,"used_dollars":0.0,"remaining_dollars":250.0,"locked_reason":null},"seven_day_sonnet":null,"missing":{"resets_at":null},"nonnumeric":{"utilization":"30"},"limits":[{"percent":99}],"extra_usage":{"utilization":99},"spend":{"utilization":99}}`)
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "test-claude-secret"}, Attributes: map[string]string{"base_url": "https://ignored.invalid"}}
	e := NewClaudeExecutor(nil)
	e.usageBaseURL = server.URL
	headers, status, errFetch := e.FetchUsageHeaders(context.Background(), auth)
	if errFetch != nil || status != http.StatusOK {
		t.Fatalf("fetch status=%d err=%v", status, errFetch)
	}
	reset, errParse := time.Parse(time.RFC3339Nano, "2030-10-11T23:59:59.572809+00:00")
	if errParse != nil {
		t.Fatal(errParse)
	}
	want := map[string]string{
		"5h-Utilization": "0.125", "5h-Status": "allowed",
		"7d-Utilization": "0.42", "7d-Status": "allowed", "7d-Reset": fmt.Sprint(reset.Unix()),
		"seven_day_opus-Utilization": "0.2", "seven_day_opus-Status": "rejected",
		"tangelo-Utilization": "1", "tangelo-Status": "rejected",
		"numeric_reset-Utilization": "0.05", "numeric_reset-Status": "allowed",
	}
	if len(headers) != len(want) {
		t.Fatalf("unexpected headers: %v", headers)
	}
	for suffix, value := range want {
		if got := headers.Get("Anthropic-Ratelimit-Unified-" + suffix); got != value {
			t.Errorf("%s = %q, want %q", suffix, got, value)
		}
	}
}

func TestCodexUsageRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer test-codex-secret" || r.Header.Get("ChatGPT-Account-Id") != "account-123" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected Codex usage request shape")
		}
		fmt.Fprint(w, `{"plan_type":"prolite","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_after_seconds":537020,"reset_at":1792295858},"secondary_window":null},"code_review_rate_limit":null,"credits":{"balance":"0"}}`)
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "test-codex-secret", "account_id": "account-123"}}
	e := NewCodexExecutor(nil)
	e.usageBaseURL = server.URL
	headers, status, errFetch := e.FetchUsageHeaders(context.Background(), auth)
	if errFetch != nil || status != http.StatusOK {
		t.Fatalf("fetch status=%d err=%v", status, errFetch)
	}
	want := map[string]string{
		"X-Codex-Primary-Used-Percent": "4", "X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At": "1792295858", "X-Codex-Primary-Reset-After-Seconds": "537020",
		"X-Codex-Allowed": "true", "X-Codex-Limit-Reached": "false", "X-Codex-Plan-Type": "prolite",
	}
	if len(headers) != len(want) {
		t.Fatalf("unexpected headers: %v", headers)
	}
	for key, value := range want {
		if got := headers.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestUsageRefreshFetchErrors(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, status := range []int{200, 401, 403, 429} {
			t.Run(fmt.Sprintf("%s/%d", provider, status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					fmt.Fprint(w, `invalid JSON`)
				}))
				defer server.Close()
				auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "secret"}}
				var headers http.Header
				var gotStatus int
				var errFetch error
				if provider == "claude" {
					e := NewClaudeExecutor(nil)
					e.usageBaseURL = server.URL
					headers, gotStatus, errFetch = e.FetchUsageHeaders(context.Background(), auth)
				} else {
					e := NewCodexExecutor(nil)
					e.usageBaseURL = server.URL
					headers, gotStatus, errFetch = e.FetchUsageHeaders(context.Background(), auth)
				}
				if gotStatus != status || len(headers) != 0 || (errFetch != nil) != (status == 200) {
					t.Fatalf("fetch = %v, %d, %v", headers, gotStatus, errFetch)
				}
			})
		}
	}
}
