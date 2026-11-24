package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchUsageHeaders reads Claude OAuth usage as canonical quota headers.
func (e *ClaudeExecutor) FetchUsageHeaders(ctx context.Context, auth *cliproxyauth.Auth) (http.Header, int, error) {
	baseURL := e.usageBaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/oauth/usage", nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	token, _ := auth.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	helps.ApplyClaudeDefaultDeviceProfileHeaders(req, e.cfg)
	client := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 10*time.Second)
	resp, errFetch := client.Do(req)
	if errFetch != nil {
		return nil, 0, errFetch
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Error("usage refresh: close Claude response body failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var windows map[string]json.RawMessage
	if errDecode := json.NewDecoder(resp.Body).Decode(&windows); errDecode != nil {
		return nil, resp.StatusCode, errDecode
	}
	headers := make(http.Header)
	for key, raw := range windows {
		if key == "limits" || key == "extra_usage" || key == "spend" {
			continue
		}
		var window struct {
			Utilization  *float64        `json:"utilization"`
			ResetsAt     json.RawMessage `json:"resets_at"`
			LockedReason json.RawMessage `json:"locked_reason"`
			LimitDollars json.RawMessage `json:"limit_dollars"`
		}
		if errDecode := json.Unmarshal(raw, &window); errDecode != nil || window.Utilization == nil {
			continue
		}
		// Dollar-denominated credit pools are not rate windows.
		if len(window.LimitDollars) != 0 && string(window.LimitDollars) != "null" {
			continue
		}
		switch key {
		case "five_hour":
			key = "5h"
		case "seven_day":
			key = "7d"
		}
		prefix := "Anthropic-Ratelimit-Unified-" + key
		headers.Set(prefix+"-Utilization", strconv.FormatFloat(*window.Utilization/100, 'f', -1, 64))
		var resetsAt string
		if errDecode := json.Unmarshal(window.ResetsAt, &resetsAt); errDecode == nil {
			if reset, errParse := time.Parse(time.RFC3339Nano, resetsAt); errParse == nil {
				headers.Set(prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
			}
		}
		status := "allowed"
		if *window.Utilization >= 100 || (len(window.LockedReason) != 0 && string(window.LockedReason) != "null") {
			status = "rejected"
		}
		headers.Set(prefix+"-Status", status)
	}
	return headers, resp.StatusCode, nil
}
