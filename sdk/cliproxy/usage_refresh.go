package cliproxy

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

type usageHeadersFetcher interface {
	FetchUsageHeaders(context.Context, *coreauth.Auth) (http.Header, int, error)
}

type usageRefreshAccount struct {
	token       string
	lastAttempt time.Time
	interval    time.Duration
	backoff     int
	strikes     int
}

type usageRefreshWorker struct {
	manager  *coreauth.Manager
	interval time.Duration
	fetcher  func(string) usageHeadersFetcher
	now      func() time.Time
	wait     func(context.Context, time.Duration) bool
	accounts map[string]*usageRefreshAccount
}

func (s *Service) startUsageRefresh(ctx context.Context) {
	s.usageRefreshMu.Lock()
	defer s.usageRefreshMu.Unlock()
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	s.usageRefreshContext = ctx
	s.applyUsageRefreshConfigLocked(cfg)
}

func (s *Service) applyUsageRefreshConfig(cfg *config.Config) {
	s.usageRefreshMu.Lock()
	defer s.usageRefreshMu.Unlock()
	s.applyUsageRefreshConfigLocked(cfg)
}

func (s *Service) applyUsageRefreshConfigLocked(cfg *config.Config) {
	state := normalizedRoutingRuntimeState(cfg)
	interval := state.usageRefreshInterval
	if state.strategy != "fill-most-used" {
		interval = 0
	}
	if s.usageRefreshContext == nil || s.usageRefreshContext.Err() != nil || s.coreManager == nil {
		return
	}
	if s.usageRefreshCancel != nil && s.usageRefreshInterval == interval {
		return
	}
	s.cancelUsageRefreshLocked()
	if interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.usageRefreshContext)
	done := make(chan struct{})
	s.usageRefreshCancel = cancel
	s.usageRefreshDone = done
	s.usageRefreshInterval = interval
	if s.usageRefreshAccounts == nil {
		s.usageRefreshAccounts = make(map[string]*usageRefreshAccount)
	}
	worker := &usageRefreshWorker{
		manager:  s.coreManager,
		interval: interval,
		now:      time.Now,
		wait:     waitUsageRefreshTick,
		accounts: s.usageRefreshAccounts,
		fetcher: func(provider string) usageHeadersFetcher {
			if fetcher := s.usageRefreshFetchers[provider]; fetcher != nil {
				return fetcher
			}
			s.cfgMu.RLock()
			currentCfg := s.cfg
			s.cfgMu.RUnlock()
			if provider == "claude" {
				return executor.NewClaudeExecutor(currentCfg)
			}
			return executor.NewCodexExecutor(currentCfg)
		},
	}
	go func() {
		defer close(done)
		worker.run(ctx)
	}()
}

func (s *Service) cancelUsageRefreshLocked() {
	if s.usageRefreshCancel != nil {
		s.usageRefreshCancel()
		<-s.usageRefreshDone
	}
	s.usageRefreshCancel = nil
	s.usageRefreshDone = nil
	s.usageRefreshInterval = 0
}

func (s *Service) stopUsageRefresh() {
	s.usageRefreshMu.Lock()
	defer s.usageRefreshMu.Unlock()
	s.usageRefreshContext = nil
	s.cancelUsageRefreshLocked()
}

func (w *usageRefreshWorker) run(ctx context.Context) {
	w.refresh(ctx, true)
	for ctx.Err() == nil {
		jittered := float64(w.interval) * (0.9 + rand.Float64()*0.2)
		delay := time.Duration(math.MaxInt64)
		if jittered < float64(math.MaxInt64) {
			delay = time.Duration(jittered)
		}
		if !w.wait(ctx, delay) {
			return
		}
		w.refresh(ctx, false)
	}
}

func waitUsageRefreshTick(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *usageRefreshWorker) refresh(ctx context.Context, startup bool) {
	auths := w.manager.List()
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	present := make(map[string]bool, len(auths))
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		present[auth.ID] = true
		token, eligible := usageRefreshToken(auth)
		if !eligible {
			continue
		}
		account := w.accounts[auth.ID]
		if account == nil || account.token != token {
			account = &usageRefreshAccount{token: token, interval: w.interval}
			w.accounts[auth.ID] = account
		}
		if account.strikes >= 3 {
			continue
		}
		account.interval = usageRefreshBackoff(w.interval, account.backoff)
		now := w.now()
		if !startup {
			if !auth.Quota.ObservedAt.IsZero() && now.Sub(auth.Quota.ObservedAt) < time.Duration(float64(w.interval)*0.8) {
				continue
			}
			if account.interval > w.interval && !account.lastAttempt.IsZero() && now.Sub(account.lastAttempt) < account.interval {
				continue
			}
		}
		account.lastAttempt = now
		headers, status, errFetch := w.fetcher(auth.Provider).FetchUsageHeaders(ctx, auth)
		window, utilization := usageRefreshBinding(auth.Provider, headers, now)
		log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider}).
			Debugf("usage refresh status=%d window=%s utilization=%g", status, window, utilization)
		if ctx.Err() != nil {
			return
		}
		switch status {
		case http.StatusOK:
			account.interval = w.interval
			account.backoff = 0
			account.strikes = 0
			if errFetch == nil {
				w.manager.ObserveQuotaSnapshot(ctx, auth.ID, auth.Provider, headers, now)
			}
		case http.StatusTooManyRequests:
			if account.backoff < 64 {
				account.backoff++
			}
			account.interval = usageRefreshBackoff(w.interval, account.backoff)
			account.strikes = 0
		case http.StatusUnauthorized, http.StatusForbidden:
			account.strikes++
			if account.strikes == 3 {
				log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider}).Warn("usage refresh stopped after three authorization failures")
			}
		default:
			account.strikes = 0
		}
	}
	for id := range w.accounts {
		if !present[id] {
			delete(w.accounts, id)
		}
	}
}

func usageRefreshBackoff(base time.Duration, backoff int) time.Duration {
	interval := base
	for i := 0; i < backoff; i++ {
		if interval >= 30*time.Minute {
			return time.Hour
		}
		interval *= 2
	}
	return interval
}

func usageRefreshToken(auth *coreauth.Auth) (string, bool) {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || (auth.Provider != "claude" && auth.Provider != "codex") {
		return "", false
	}
	if _, apiKey := auth.Attributes["api_key"]; apiKey {
		return "", false
	}
	token, _ := auth.Metadata["access_token"].(string)
	return token, strings.TrimSpace(token) != ""
}

func usageRefreshBinding(provider string, headers http.Header, now time.Time) (string, float64) {
	window, utilization := "", float64(0)
	windows := []string{"5h", "7d"}
	if provider == "codex" {
		windows = []string{"Primary", "Secondary"}
	}
	for _, name := range windows {
		prefix := "Anthropic-Ratelimit-Unified-" + name
		key := prefix + "-Utilization"
		resetKey := prefix + "-Reset"
		if provider == "codex" {
			prefix = "X-Codex-" + name
			key = prefix + "-Used-Percent"
			resetKey = prefix + "-Reset-At"
		}
		if reset, errParse := strconv.ParseInt(headers.Get(resetKey), 10, 64); errParse == nil && !time.Unix(reset, 0).After(now) {
			continue
		}
		value, errParse := strconv.ParseFloat(headers.Get(key), 64)
		if provider == "codex" {
			value /= 100
		}
		if errParse == nil && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) && (window == "" || value > utilization) {
			window, utilization = name, math.Min(value, 1)
		}
	}
	return window, utilization
}
