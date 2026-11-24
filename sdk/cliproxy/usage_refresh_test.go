package cliproxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type usageRefreshTestFetcher struct {
	url    string
	client *http.Client
}

func (f usageRefreshTestFetcher) FetchUsageHeaders(ctx context.Context, auth *coreauth.Auth) (http.Header, int, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/"+auth.ID, nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	token, _ := auth.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errFetch := f.client.Do(req)
	if errFetch != nil {
		return nil, 0, errFetch
	}
	if errClose := resp.Body.Close(); errClose != nil {
		return nil, resp.StatusCode, errClose
	}
	return resp.Header, resp.StatusCode, nil
}

func newUsageRefreshTestWorker(t *testing.T, handler http.HandlerFunc, auths ...*coreauth.Auth) (*usageRefreshWorker, *time.Time) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m := coreauth.NewManager(nil, &coreauth.FillMostUsedSelector{}, nil)
	for _, auth := range auths {
		if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	now := time.Unix(1900000000, 0)
	worker := &usageRefreshWorker{manager: m, interval: 10 * time.Minute, now: func() time.Time { return now }, accounts: make(map[string]*usageRefreshAccount), fetcher: func(string) usageHeadersFetcher {
		return usageRefreshTestFetcher{url: server.URL, client: server.Client()}
	}}
	return worker, &now
}

func refreshTestAuth(id, provider string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: provider, Metadata: map[string]any{"access_token": "test-secret-" + id}}
}

func TestUsageRefreshStartupAndFreshResponse(t *testing.T) {
	calls := make(map[string]int)
	var callsMu sync.Mutex
	claude, codex := refreshTestAuth("claude", "claude"), refreshTestAuth("codex", "codex")
	disabled := refreshTestAuth("disabled", "claude")
	disabled.Disabled = true
	apiKey := refreshTestAuth("key", "claude")
	apiKey.Attributes = map[string]string{"api_key": "key"}
	worker, now := newUsageRefreshTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls[strings.TrimPrefix(r.URL.Path, "/")]++
		callsMu.Unlock()
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.9")
		w.Header().Set("X-Codex-Primary-Used-Percent", "80")
	}, claude, codex, disabled, apiKey, refreshTestAuth("gemini", "gemini"), &coreauth.Auth{ID: "no-token", Provider: "claude"})
	worker.refresh(context.Background(), true)
	callsMu.Lock()
	if len(calls) != 2 || calls["claude"] != 1 || calls["codex"] != 1 {
		t.Fatalf("startup calls = %v", calls)
	}
	callsMu.Unlock()
	for _, id := range []string{"claude", "codex"} {
		auth, _ := worker.manager.GetByID(id)
		if !auth.Quota.ObservedAt.Equal(*now) {
			t.Fatalf("%s observation time = %v", id, auth.Quota.ObservedAt)
		}
	}
	selector := worker.manager.Selector()
	idle := refreshTestAuth("idle", "claude")
	if _, errRegister := worker.manager.Register(context.Background(), idle); errRegister != nil {
		t.Fatal(errRegister)
	}
	worker.refresh(context.Background(), true)
	picked, errPick := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, worker.manager.List())
	if errPick != nil || picked == nil || picked.ID != "claude" {
		t.Fatalf("pick after startup refresh = %v, %v", picked, errPick)
	}
	*now = now.Add(9 * time.Minute)
	headers := make(http.Header)
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5")
	worker.manager.ObserveQuotaSnapshot(context.Background(), "claude", "claude", headers, *now)
	callsMu.Lock()
	before := calls["claude"]
	callsMu.Unlock()
	worker.refresh(context.Background(), false)
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls["claude"] != before || calls["codex"] != 3 || calls["idle"] != 2 {
		t.Fatalf("fresh account was fetched or idle account skipped: %v", calls)
	}
}

func TestUsageRefreshTicks(t *testing.T) {
	var callsMu sync.Mutex
	calls := 0
	worker, now := newUsageRefreshTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.4")
	}, refreshTestAuth("account", "claude"))
	ready := make(chan time.Duration)
	ticks := make(chan time.Time)
	worker.wait = func(ctx context.Context, delay time.Duration) bool {
		select {
		case ready <- delay:
		case <-ctx.Done():
			return false
		}
		select {
		case next := <-ticks:
			*now = next
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.run(ctx) }()
	defer func() { cancel(); <-done }()
	waitReady := func() {
		t.Helper()
		select {
		case delay := <-ready:
			if delay < 9*time.Minute || delay > 11*time.Minute {
				t.Fatalf("tick jitter = %v", delay)
			}
		case <-time.After(time.Second):
			t.Fatal("worker did not wait for its next tick")
		}
	}
	waitReady()
	for i := 0; i < 4; i++ {
		ticks <- now.Add(9 * time.Minute)
		waitReady()
	}
	callsMu.Lock()
	if calls != 5 {
		t.Errorf("startup and four early ticks made %d fetches, want 5", calls)
	}
	callsMu.Unlock()
	headers := make(http.Header)
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.7")
	worker.manager.ObserveQuotaSnapshot(context.Background(), "account", "claude", headers, now.Add(8*time.Minute))
	ticks <- now.Add(9 * time.Minute)
	waitReady()
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 5 {
		t.Error("tick fetched an account with a fresh response snapshot")
	}
}

func TestUsageRefreshBackoffAndTokenRotation(t *testing.T) {
	var output bytes.Buffer
	logger := log.StandardLogger()
	previousOutput, previousLevel := logger.Out, logger.GetLevel()
	logger.SetOutput(&output)
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(func() { logger.SetOutput(previousOutput); logger.SetLevel(previousLevel) })
	calls := 0
	status := http.StatusTooManyRequests
	worker, now := newUsageRefreshTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.4")
		w.WriteHeader(status)
	}, refreshTestAuth("account", "claude"))
	worker.refresh(context.Background(), true)
	account := worker.accounts["account"]
	if account.interval != 20*time.Minute {
		t.Fatalf("429 interval = %v", account.interval)
	}
	*now = now.Add(10 * time.Minute)
	worker.refresh(context.Background(), false)
	if calls != 1 {
		t.Fatal("429 account refreshed at the original interval")
	}
	*now = now.Add(10 * time.Minute)
	status = http.StatusOK
	worker.refresh(context.Background(), false)
	if calls != 2 || account.interval != worker.interval {
		t.Fatal("200 did not reset account backoff")
	}
	for i := 0; i < 10; i++ {
		status = http.StatusTooManyRequests
		*now = now.Add(time.Hour)
		worker.refresh(context.Background(), false)
	}
	if account.interval != time.Hour {
		t.Fatalf("backoff cap = %v", account.interval)
	}
	for i := 0; i < 5; i++ {
		status = http.StatusUnauthorized
		if i == 1 {
			status = http.StatusForbidden
		}
		*now = now.Add(time.Hour)
		worker.refresh(context.Background(), false)
	}
	if calls != 15 || account.strikes != 3 || strings.Count(output.String(), "level=warning") != 1 || !strings.Contains(output.String(), "auth_id=account") || !strings.Contains(output.String(), "usage refresh") {
		t.Fatalf("authorization stop calls=%d strikes=%d logs=%s", calls, account.strikes, output.String())
	}
	restarted := *worker
	restarted.interval = 20 * time.Minute
	restarted.refresh(context.Background(), true)
	if calls != 15 || strings.Count(output.String(), "level=warning") != 1 {
		t.Fatal("worker restart resumed a stopped account or repeated its warning")
	}
	auth, _ := worker.manager.GetByID("account")
	auth.Metadata["access_token"] = "rotated-secret"
	if _, errUpdate := worker.manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	status = http.StatusOK
	worker.refresh(context.Background(), false)
	if calls != 16 || worker.accounts["account"].strikes != 0 {
		t.Fatal("token rotation did not resume usage refresh")
	}
	if strings.Contains(output.String(), "test-secret-account") || strings.Contains(output.String(), "rotated-secret") || strings.Count(output.String(), "level=debug") != calls {
		t.Fatal("usage refresh logged a token or did not log once per fetch")
	}
}

func TestUsageRefreshProtectsResponseDuringFetch(t *testing.T) {
	var worker *usageRefreshWorker
	var now *time.Time
	worker, now = newUsageRefreshTestWorker(t, func(w http.ResponseWriter, r *http.Request) {
		headers := make(http.Header)
		headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.7")
		worker.manager.ObserveQuotaSnapshot(context.Background(), "account", "claude", headers, now.Add(time.Second))
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.2")
	}, refreshTestAuth("account", "claude"))
	worker.refresh(context.Background(), true)
	auth, _ := worker.manager.GetByID("account")
	if auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != "0.7" {
		t.Fatal("in-flight fetch replaced a newer response snapshot")
	}
}

type blockingUsageFetcher struct {
	started chan struct{}
}

func (f blockingUsageFetcher) FetchUsageHeaders(ctx context.Context, auth *coreauth.Auth) (http.Header, int, error) {
	f.started <- struct{}{}
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

func TestUsageRefreshLifecycle(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), refreshTestAuth("account", "claude")); errRegister != nil {
		t.Fatal(errRegister)
	}
	started := make(chan struct{}, 10)
	service := &Service{coreManager: m, cfg: &internalconfig.Config{}, usageRefreshFetchers: map[string]usageHeadersFetcher{"claude": blockingUsageFetcher{started: started}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer service.stopUsageRefresh()
	service.startUsageRefresh(ctx)
	if service.usageRefreshDone != nil {
		t.Fatal("round-robin started usage refresh")
	}
	apply := func(strategy, interval string) {
		t.Helper()
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy, UsageRefreshInterval: interval}, AuthDir: t.TempDir()}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg}) {
			t.Fatal("manager config update failed")
		}
	}
	waitStarted := func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("usage worker did not start")
		}
	}
	apply("fill-most-used", "0")
	if service.usageRefreshDone != nil {
		t.Fatal("disabled interval started usage refresh")
	}
	apply("fill-most-used", "10m")
	waitStarted()
	first := service.usageRefreshDone
	apply("round-robin", "10m")
	select {
	case <-first:
	default:
		t.Fatal("strategy switch did not join the old worker")
	}
	apply("fill-most-used", "10m")
	waitStarted()
	second := service.usageRefreshDone
	apply("fill-most-used", "20m")
	waitStarted()
	select {
	case <-second:
	default:
		t.Fatal("interval change did not join the old worker")
	}
	third := service.usageRefreshDone
	cancel()
	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatal("service context did not stop worker")
	}
	service.stopUsageRefresh()
	service.startUsageRefresh(context.Background())
	apply("fill-most-used", "10m")
	waitStarted()
	last := service.usageRefreshDone
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatal(errShutdown)
	}
	select {
	case <-last:
	default:
		t.Fatal("shutdown did not join worker")
	}
}

func TestUsageRefreshIntervalConfig(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"", 10 * time.Minute}, {"0", 0}, {"garbage", 10 * time.Minute}, {"-1m", 10 * time.Minute}, {"30s", 30 * time.Second}, {"[invalid]", 10 * time.Minute},
	} {
		t.Run(fmt.Sprint(tt.value), func(t *testing.T) {
			input := "routing:\n  strategy: fill-most-used\n"
			if tt.value != "" {
				input += "  usage-refresh-interval: " + tt.value + "\n"
			}
			cfg, errParse := internalconfig.ParseConfigBytes([]byte(input))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := normalizedRoutingRuntimeState(cfg).usageRefreshInterval; got != tt.want {
				t.Fatalf("interval = %v, want %v", got, tt.want)
			}
		})
	}
}
