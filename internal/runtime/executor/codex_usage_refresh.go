package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchUsageHeaders reads Codex OAuth usage as canonical quota headers.
func (e *CodexExecutor) FetchUsageHeaders(ctx context.Context, auth *cliproxyauth.Auth) (http.Header, int, error) {
	baseURL := e.usageBaseURL
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/backend-api/wham/usage", nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	token, _ := auth.Metadata["access_token"].(string)
	accountID, _ := auth.Metadata["account_id"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Accept", "application/json")
	client := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 10*time.Second)
	resp, errFetch := client.Do(req)
	if errFetch != nil {
		return nil, 0, errFetch
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Error("usage refresh: close Codex response body failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var usage struct {
		PlanType  string                     `json:"plan_type"`
		RateLimit map[string]json.RawMessage `json:"rate_limit"`
	}
	if errDecode := json.NewDecoder(resp.Body).Decode(&usage); errDecode != nil {
		return nil, resp.StatusCode, errDecode
	}
	rateLimits := make(map[string]any)
	for _, key := range []string{"allowed", "limit_reached"} {
		if value, ok := usage.RateLimit[key]; ok {
			rateLimits[key] = value
		}
	}
	for _, name := range []string{"primary", "secondary"} {
		raw := usage.RateLimit[name+"_window"]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var window map[string]json.RawMessage
		if errDecode := json.Unmarshal(raw, &window); errDecode != nil {
			return nil, resp.StatusCode, errDecode
		}
		var seconds float64
		if errDecode := json.Unmarshal(window["limit_window_seconds"], &seconds); errDecode != nil {
			return nil, resp.StatusCode, errDecode
		}
		mapped := map[string]any{"window_minutes": seconds / 60}
		for _, key := range []string{"used_percent", "reset_after_seconds", "reset_at"} {
			if value, ok := window[key]; ok {
				mapped[key] = value
			}
		}
		rateLimits[name] = mapped
	}
	event := struct {
		Type       string         `json:"type"`
		RateLimits map[string]any `json:"rate_limits"`
		PlanType   string         `json:"plan_type"`
	}{"codex.rate_limits", rateLimits, usage.PlanType}
	payload, errEncode := json.Marshal(event)
	if errEncode != nil {
		return nil, resp.StatusCode, errEncode
	}
	return helps.ParseCodexQuotaEventHeaders(payload), resp.StatusCode, nil
}
