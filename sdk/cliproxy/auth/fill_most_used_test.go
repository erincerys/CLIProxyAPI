package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeUsageAuth(id, usage string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{
		ObservedAt: time.Now(),
		Signals:    map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": usage},
	}}
}

func newUsageTestManager(t *testing.T, selector Selector, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	for _, candidate := range auths {
		candidate.ID = t.Name() + "/" + candidate.ID
		manager.RegisterExecutor(schedulerTestExecutor{provider: candidate.Provider})
		registerSchedulerModels(t, candidate.Provider, "usage-model", candidate.ID)
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return manager
}

func requireUsagePick(t *testing.T, manager *Manager, provider, wantID string) {
	t.Helper()
	picked, _, errPick := manager.pickNext(context.Background(), provider, "usage-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil || picked == nil || picked.ID != wantID {
		t.Fatalf("pickNext() = %v, %v; want %s", picked, errPick, wantID)
	}
}

func TestFillMostUsedSelection(t *testing.T) {
	for _, path := range []string{"scheduler", "legacy"} {
		for _, scenario := range []string{"ranked", "no data", "priority", "exhausted", "only exhausted", "cooldown"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				a, b, c := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.9"), claudeUsageAuth("c", "0.6")
				want := b
				auths := []*Auth{c, b, a}
				switch scenario {
				case "no data":
					for _, candidate := range auths {
						candidate.Quota = QuotaState{}
					}
					var errPick error
					want, errPick = (&FillFirstSelector{}).Pick(context.Background(), "claude", "usage-model", cliproxyexecutor.Options{}, auths)
					if errPick != nil {
						t.Fatal(errPick)
					}
				case "priority":
					a.Quota = QuotaState{}
					a.Attributes = map[string]string{"priority": "10"}
					want = a
				case "exhausted", "only exhausted":
					b.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
					b.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
					want = c
					if scenario == "only exhausted" {
						auths, want = []*Auth{b}, b
					}
				case "cooldown":
					b.ModelStates = map[string]*ModelState{"usage-model": {
						Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour),
						Quota: QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)},
					}}
					want = c
				}
				selector := &FillMostUsedSelector{}
				manager := newUsageTestManager(t, selector, auths...)
				if !manager.useSchedulerFastPath() {
					t.Fatal("fill-most-used must use the scheduler fast path")
				}
				for i := 0; i < 3; i++ {
					var picked *Auth
					var errPick error
					if path == "scheduler" {
						picked, _, errPick = manager.pickNext(context.Background(), "claude", "usage-model", cliproxyexecutor.Options{}, nil)
					} else {
						picked, _, errPick = manager.pickNextLegacy(context.Background(), "claude", "usage-model", cliproxyexecutor.Options{}, nil)
					}
					if errPick != nil || picked == nil || picked.ID != want.ID {
						t.Fatalf("pick #%d = %v, %v; want %s", i, picked, errPick, want.ID)
					}
				}
			})
		}
	}
}

func TestFillMostUsedMarkResultUpdatesRanking(t *testing.T) {
	a, b := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.8")
	manager := newUsageTestManager(t, &FillMostUsedSelector{}, a, b)
	requireUsagePick(t, manager, "claude", b.ID)
	for _, tt := range []struct{ usage, want string }{{"0.95", a.ID}, {"0.05", b.ID}} {
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"anthropic-ratelimit-unified-5h-utilization": []string{tt.usage},
		})
		manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Model: "usage-model", Success: true})
		updated, ok := manager.GetByID(a.ID)
		if !ok || updated.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != tt.usage || updated.Quota.ObservedAt.IsZero() {
			t.Fatalf("response headers did not update snapshot: %+v", updated)
		}
		requireUsagePick(t, manager, "claude", tt.want)
	}
}

func TestFillMostUsedModelSnapshot(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduler", true: "legacy"}[legacy], func(t *testing.T) {
			a, b := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.8")
			a.ModelStates = map[string]*ModelState{"usage-model": {Quota: QuotaState{
				ObservedAt: a.Quota.ObservedAt.Add(time.Second),
				Signals:    map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.95"},
			}}}
			manager := newUsageTestManager(t, &FillMostUsedSelector{}, a, b)
			var picked *Auth
			var errPick error
			if legacy {
				picked, _, errPick = manager.pickNextLegacy(context.Background(), "claude", "usage-model", cliproxyexecutor.Options{}, nil)
			} else {
				picked, _, errPick = manager.pickNext(context.Background(), "claude", "usage-model", cliproxyexecutor.Options{}, nil)
			}
			if errPick != nil || picked == nil || picked.ID != a.ID {
				t.Fatalf("model snapshot pick = %v, %v; want %s", picked, errPick, a.ID)
			}
		})
	}
}

func TestFillMostUsedMarkResultUpdatesOtherWarmedModels(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "reload"}[reload], func(t *testing.T) {
			a, b := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.8")
			manager := newUsageTestManager(t, &FillMostUsedSelector{}, a, b)
			for _, candidate := range []*Auth{a, b} {
				registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: "usage-model"}, {ID: "other-usage-model"}})
			}
			manager.syncScheduler()
			requireUsagePick(t, manager, "claude", b.ID)
			picked, _, errPick := manager.pickNext(context.Background(), "claude", "other-usage-model", cliproxyexecutor.Options{}, nil)
			if errPick != nil || picked == nil || picked.ID != b.ID {
				t.Fatalf("initial other model pick = %v, %v; want %s", picked, errPick, b.ID)
			}
			if reload {
				manager.SetSelector(&FillFirstSelector{})
			}
			ctx := internallogging.WithResponseHeadersHolder(context.Background())
			internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.95"}})
			manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Model: "other-usage-model", Success: true})
			if reload {
				manager.SetSelector(&FillMostUsedSelector{})
			}
			requireUsagePick(t, manager, "claude", a.ID)
		})
	}
}

func TestFillMostUsedMixedProviders(t *testing.T) {
	a, b := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.8")
	c := &Auth{ID: "c", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "99"}}}
	manager := newUsageTestManager(t, &FillMostUsedSelector{}, a, b, c)
	for _, tt := range []struct {
		name  string
		tried map[string]struct{}
		want  *Auth
	}{
		{"provider order", nil, b},
		{"next provider", map[string]struct{}{a.ID: {}, b.ID: {}}, c},
		{"predicate", map[string]struct{}{b.ID: {}}, a},
	} {
		t.Run(tt.name, func(t *testing.T) {
			picked, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"claude", "codex"}, "usage-model", cliproxyexecutor.Options{}, tt.tried)
			if errPick != nil || picked == nil || picked.ID != tt.want.ID || provider != tt.want.Provider {
				t.Fatalf("pickNextMixed() = %v, %s, %v; want %s", picked, provider, errPick, tt.want.ID)
			}
		})
	}
	c.Attributes = map[string]string{"priority": "10"}
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), c); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	picked, _, _, errPick := manager.pickNextMixed(context.Background(), []string{"claude", "codex"}, "usage-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil || picked == nil || picked.ID != c.ID {
		t.Fatalf("mixed priority pick = %v, %v; want %s", picked, errPick, c.ID)
	}
}

func TestFillMostUsedSessionAffinityColdBinding(t *testing.T) {
	a, b := claudeUsageAuth("a", "0.2"), claudeUsageAuth("b", "0.8")
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillMostUsedSelector{}, TTL: time.Hour})
	t.Cleanup(selector.Stop)
	manager := newUsageTestManager(t, selector, a, b)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"fmu-session"}}}
	picked, _, errPick := manager.pickNext(context.Background(), "claude", "usage-model", opts, nil)
	if errPick != nil || picked == nil || picked.ID != b.ID {
		t.Fatalf("cold binding = %v, %v; want %s", picked, errPick, b.ID)
	}
	if bound, status := manager.LookupSessionAffinity("claude", "usage-model", "fmu-session"); status != "bound" || bound == nil || bound.ID != b.ID {
		t.Fatalf("binding = %v, %s; want %s", bound, status, b.ID)
	}
}

func TestFillMostUsedSelectorReloadsRescuePolicy(t *testing.T) {
	now := time.Now()
	a, b := claudeUsageAuth("a", "0"), claudeUsageAuth("b", "0")
	for _, tt := range []struct {
		auth  *Auth
		usage string
		after time.Duration
	}{
		{a, "0.6", 6 * 24 * time.Hour}, {b, "0.3", 4 * time.Hour},
	} {
		tt.auth.Quota.Signals = map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": tt.usage,
			"Anthropic-Ratelimit-Unified-7d-Reset":       now.Add(tt.after).Format(time.RFC3339Nano),
		}
	}
	manager := newUsageTestManager(t, &FillMostUsedSelector{RescueWithin: 24 * time.Hour, RescueHeadroom: 0.25}, a, b)
	requireUsagePick(t, manager, "claude", b.ID)
	manager.SetSelector(&FillMostUsedSelector{})
	requireUsagePick(t, manager, "claude", a.ID)
	manager.SetSelector(&FillMostUsedSelector{RescueWithin: 24 * time.Hour, RescueHeadroom: 0.25})
	requireUsagePick(t, manager, "claude", b.ID)
}

func TestFillMostUsedRanksAtPickTime(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a, b := claudeUsageAuth("a", "0.9"), claudeUsageAuth("b", "0.3")
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = now.Add(time.Hour).Format(time.RFC3339Nano)
	view := buildReadyView([]*scheduledAuth{{auth: a}, {auth: b}})
	if picked := view.pickMostUsed(nil, "", now, rescuePolicy{}); picked.auth.ID != a.ID {
		t.Fatalf("before reset picked %s, want %s", picked.auth.ID, a.ID)
	}
	if picked := view.pickMostUsed(nil, "", now.Add(2*time.Hour), rescuePolicy{}); picked.auth.ID != b.ID {
		t.Fatalf("after reset picked %s, want %s", picked.auth.ID, b.ID)
	}
}

func TestFillMostUsedWebsocketPreference(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduler", true: "legacy"}[legacy], func(t *testing.T) {
			a := &Auth{ID: "http", Provider: "codex", Quota: QuotaState{
				Signals: map[string]string{"X-Codex-Primary-Used-Percent": "99"},
			}}
			b := &Auth{ID: "ws", Provider: "codex", Attributes: map[string]string{"websockets": "true"}, Quota: QuotaState{
				Signals: map[string]string{"X-Codex-Primary-Used-Percent": "20"},
			}}
			manager := newUsageTestManager(t, &FillMostUsedSelector{}, a, b)
			ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
			var picked *Auth
			var errPick error
			if legacy {
				picked, _, errPick = manager.pickNextLegacy(ctx, "codex", "usage-model", cliproxyexecutor.Options{}, nil)
			} else {
				picked, _, errPick = manager.pickNext(ctx, "codex", "usage-model", cliproxyexecutor.Options{}, nil)
			}
			if errPick != nil || picked == nil || picked.ID != b.ID {
				t.Fatalf("websocket pick = %v, %v; want %s", picked, errPick, b.ID)
			}
		})
	}
}
