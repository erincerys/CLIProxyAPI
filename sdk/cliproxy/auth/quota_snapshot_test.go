package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestObserveQuotaSnapshotPreservesState(t *testing.T) {
	now := time.Unix(1791700000, 0)
	a := &Auth{ID: "account", Provider: "claude", Quota: QuotaState{
		ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2"},
		Exceeded: true, Reason: "existing", BackoffLevel: 3, NextRecoverAt: now.Add(time.Hour),
	}, ModelStates: map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}}
	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
		t.Fatal(errRegister)
	}
	before, _ := m.GetByID(a.ID)
	headers := make(http.Header)
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.125")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.42")
	if m.ObserveQuotaSnapshot(context.Background(), a.ID, "claude", headers, now.Add(-time.Second)) {
		t.Fatal("older snapshot accepted")
	}
	if after, _ := m.GetByID(a.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("older snapshot changed account")
	}
	if !m.ObserveQuotaSnapshot(context.Background(), a.ID, "claude", headers, now.Add(time.Second)) {
		t.Fatal("fresh snapshot rejected")
	}
	after, _ := m.GetByID(a.ID)
	if !reflect.DeepEqual(cooldownFieldsOf(before.Quota), cooldownFieldsOf(after.Quota)) || !reflect.DeepEqual(before.ModelStates, after.ModelStates) || after.Generation != before.Generation+1 || after.UpdatedAt.IsZero() {
		t.Fatal("usage observation changed cooldown/model state or did not advance generation")
	}
	if rank := usageRankFor(after, "", now, rescuePolicy{}); rank.score != 0.42 {
		t.Fatalf("Claude rank = %+v", rank)
	}
	if m.ObserveQuotaSnapshot(context.Background(), "missing", "claude", headers, now) || m.ObserveQuotaSnapshot(context.Background(), a.ID, "claude", nil, now.Add(time.Hour)) {
		t.Fatal("missing auth or empty headers reported a change")
	}
}

func TestObserveQuotaSnapshotCodexWeekly(t *testing.T) {
	now := time.Unix(1791700000, 0)
	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "codex", Provider: "codex"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	headers := make(http.Header)
	headers.Set("X-Codex-Primary-Used-Percent", "4")
	headers.Set("X-Codex-Primary-Window-Minutes", "10080")
	headers.Set("X-Codex-Primary-Reset-At", "1792295858")
	headers.Set("X-Codex-Primary-Reset-After-Seconds", "537020")
	m.ObserveQuotaSnapshot(context.Background(), "codex", "codex", headers, now)
	a, _ := m.GetByID("codex")
	rank := usageRankFor(a, "", now, rescuePolicy{within: 7 * time.Hour * 24, headroom: 0.25})
	if rank.score != 0.04 || rank.weeklyReset.Unix() != 1792295858 || !rank.rescue {
		t.Fatalf("Codex weekly rank = %+v", rank)
	}
}

func TestObserveQuotaSnapshotUpdatesEveryModel(t *testing.T) {
	a, b := claudeUsageAuth("a", "0.2"), &Auth{ID: "b", Provider: "claude", Status: StatusActive}
	m := newUsageTestManager(t, &FillMostUsedSelector{}, a, b)
	for _, candidate := range []*Auth{a, b} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: "usage-model"}, {ID: "other-usage-model"}})
	}
	m.syncScheduler()
	for _, model := range []string{"usage-model", "other-usage-model"} {
		picked, _, errPick := m.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
		if errPick != nil || picked.ID != a.ID {
			t.Fatalf("initial pick = %v, %v", picked, errPick)
		}
	}
	headers := make(http.Header)
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.9")
	if !m.ObserveQuotaSnapshot(context.Background(), b.ID, "claude", headers, time.Now()) {
		t.Fatal("unused account snapshot rejected")
	}
	for _, model := range []string{"usage-model", "other-usage-model"} {
		picked, _, errPick := m.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
		if errPick != nil || picked.ID != b.ID {
			t.Fatalf("refreshed pick = %v, %v; want %s", picked, errPick, b.ID)
		}
	}
}
