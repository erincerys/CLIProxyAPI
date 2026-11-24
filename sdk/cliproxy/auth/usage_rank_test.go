package auth

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func TestUsageRankFor(t *testing.T) {
	now := time.Unix(1800000000, 0)
	future := now.Add(4 * time.Hour)
	past := now.Add(-time.Hour)
	futureValue := strconv.FormatInt(future.Unix(), 10)
	pastValue := strconv.FormatInt(past.Unix(), 10)
	type rankTest struct {
		name      string
		provider  string
		signals   map[string]string
		score     float64
		reset     time.Time
		exhausted bool
	}
	tests := []rankTest{
		{name: "no snapshot", provider: "claude"},
		{name: "highest utilization", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.55", "Anthropic-Ratelimit-Unified-5h-Reset": futureValue,
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.28", "Anthropic-Ratelimit-Unified-7d-Reset": futureValue,
		}, score: 0.55, reset: future},
		{name: "expired short window", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.55", "Anthropic-Ratelimit-Unified-5h-Reset": pastValue,
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.28", "Anthropic-Ratelimit-Unified-7d-Reset": futureValue,
		}, score: 0.28, reset: future},
		{name: "all expired", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.55", "Anthropic-Ratelimit-Unified-5h-Reset": pastValue,
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.28", "Anthropic-Ratelimit-Unified-7d-Reset": pastValue,
		}},
		{name: "rejected future", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": futureValue,
		}, exhausted: true},
		{name: "rejected past", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": pastValue,
		}},
		{name: "above one clamped", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "1.8",
		}, score: 1},
		{name: "restricted Claude windows", provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization":             "0.2",
			"Anthropic-Ratelimit-Unified-7d-Utilization":             "0.3",
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization":          "0.9",
			"Anthropic-Ratelimit-Unified-Seven_day_opus-Utilization": "1",
			"Anthropic-Ratelimit-Unified-7d_oi-Status":               "rejected",
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":                futureValue,
		}, score: 0.3},
		{name: "Codex percent", provider: "codex", signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "80", "X-Codex-Primary-Reset-At": futureValue,
		}, score: 0.8, reset: future},
		{name: "Codex relative reset", provider: "codex", signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "80", "X-Codex-Primary-Reset-After-Seconds": "7200",
		}, score: 0.8, reset: now.Add(time.Hour)},
		{name: "Codex absolute reset wins", provider: "codex", signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "80", "X-Codex-Primary-Reset-At": futureValue,
			"X-Codex-Primary-Reset-After-Seconds": "7200",
		}, score: 0.8, reset: future},
		{name: "Codex exhausted", provider: "codex", signals: map[string]string{
			"X-Codex-Limit-Reached": "TrUe",
		}, exhausted: true},
		{name: "Codex extra namespaces ignored", provider: "codex", signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "20", "X-Codex-Secondary-Used-Percent": "30",
			"X-Codex-Additional-Spark-Primary-Used-Percent": "90", "X-Codex-Spark-Primary-Used-Percent": "95",
			"X-Codex-Additional-Spark-Limit-Reached": "true", "X-Codex-Spark-Limit-Reached": "true",
		}, score: 0.3},
		{name: "unsupported provider", provider: "gemini", signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "80",
		}},
	}
	for _, value := range []string{"garbage", "NaN", "+Inf", "-Inf", "-0.1"} {
		tests = append(tests, rankTest{name: "skip " + value, provider: "claude", signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": value,
			"Anthropic-Ratelimit-Unified-5h-Reset":       futureValue,
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.28",
		}, score: 0.28})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{Provider: tt.provider, Quota: QuotaState{ObservedAt: now.Add(-time.Hour), Signals: tt.signals}}
			rank := usageRankFor(auth, "", now, rescuePolicy{})
			if math.Abs(rank.score-tt.score) > 1e-12 || !rank.reset.Equal(tt.reset) || rank.exhausted != tt.exhausted || rank.rescue {
				t.Fatalf("rank = %+v, want score=%v reset=%v exhausted=%v", rank, tt.score, tt.reset, tt.exhausted)
			}
		})
	}
	if rank := usageRankFor(nil, "", now, rescuePolicy{}); rank != (usageRank{}) {
		t.Fatalf("nil auth rank = %+v", rank)
	}
}

func TestUsageRankSnapshot(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tt := range []struct {
		name          string
		modelObserved time.Time
		want          float64
	}{
		{"model newer", now.Add(time.Second), 0.8},
		{"auth newer", now.Add(-time.Second), 0.2},
		{"tie favors auth", now, 0.2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2",
			}}, ModelStates: map[string]*ModelState{"model": {Quota: QuotaState{ObservedAt: tt.modelObserved, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.8",
			}}}}}
			if rank := usageRankFor(auth, "model", now, rescuePolicy{}); rank.score != tt.want {
				t.Fatalf("rank.score = %v, want %v", rank.score, tt.want)
			}
			if rank := usageRankFor(auth, "other", now, rescuePolicy{}); rank.score != 0.2 {
				t.Fatalf("unrelated model rank.score = %v, want 0.2", rank.score)
			}
		})
	}
}

func TestParseUsageReset(t *testing.T) {
	now := time.Unix(1800000000, 250000000)
	for _, value := range []string{"1800000000.25", now.Format(time.RFC3339Nano)} {
		if got := parseUsageReset(value); !got.Equal(now) {
			t.Errorf("parseUsageReset(%q) = %v, want %v", value, got, now)
		}
	}
	for _, value := range []string{"", "garbage", "NaN", "+Inf", "-1", "1e100"} {
		if got := parseUsageReset(value); !got.IsZero() {
			t.Errorf("parseUsageReset(%q) = %v, want zero", value, got)
		}
	}
}

func TestUsageRankRescue(t *testing.T) {
	now := time.Unix(1800000000, 0)
	policy := rescuePolicy{within: 24 * time.Hour, headroom: 0.25}
	for _, tt := range []struct {
		name       string
		usage      string
		resetAfter time.Duration
		exhausted  bool
		policy     rescuePolicy
		want       bool
	}{
		{"rescue", "0.30", 4 * time.Hour, false, policy, true},
		{"insufficient headroom", "0.80", 4 * time.Hour, false, policy, false},
		{"distant reset", "0.30", 30 * time.Hour, false, policy, false},
		{"expired", "0.30", -time.Hour, false, policy, false},
		{"reset now", "0.30", 0, false, policy, false},
		{"exhausted", "0.30", 4 * time.Hour, true, policy, false},
		{"within disabled", "0.30", 4 * time.Hour, false, rescuePolicy{headroom: 0.25}, false},
		{"headroom disabled", "0.30", 4 * time.Hour, false, rescuePolicy{within: 24 * time.Hour}, false},
		{"boundary", "0.75", 24 * time.Hour, false, policy, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reset := now.Add(tt.resetAfter)
			signals := map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": tt.usage,
				"Anthropic-Ratelimit-Unified-7d-Reset":       reset.Format(time.RFC3339Nano),
			}
			if tt.exhausted {
				signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
			}
			rank := usageRankFor(&Auth{Provider: "claude", Quota: QuotaState{Signals: signals}}, "", now, tt.policy)
			if rank.rescue != tt.want || (tt.want && !rank.weeklyReset.Equal(reset)) {
				t.Fatalf("rank = %+v, want rescue=%v weeklyReset=%v", rank, tt.want, reset)
			}
		})
	}
	for _, tt := range []struct {
		name            string
		primaryLength   string
		secondaryLength string
		weekly          string
		want            bool
	}{
		{"primary weekly", "10080", "300", "Primary", true},
		{"secondary weekly", "300", "10080", "Secondary", true},
		{"longer than week", "20160", "300", "Primary", true},
		{"no lengths", "", "", "Secondary", true},
		{"primary without lengths", "", "", "Primary", false},
		{"explicit short secondary", "", "300", "Secondary", false},
		{"only primary length", "300", "", "Secondary", false},
	} {
		t.Run("Codex "+tt.name, func(t *testing.T) {
			reset := now.Add(4 * time.Hour)
			signals := map[string]string{
				"X-Codex-" + tt.weekly + "-Used-Percent": "30",
				"X-Codex-" + tt.weekly + "-Reset-At":     reset.Format(time.RFC3339Nano),
			}
			if tt.primaryLength != "" {
				signals["X-Codex-Primary-Window-Minutes"] = tt.primaryLength
			}
			if tt.secondaryLength != "" {
				signals["X-Codex-Secondary-Window-Minutes"] = tt.secondaryLength
			}
			rank := usageRankFor(&Auth{Provider: "codex", Quota: QuotaState{Signals: signals}}, "", now, policy)
			if rank.rescue != tt.want || (tt.want && !rank.weeklyReset.Equal(reset)) {
				t.Fatalf("rank = %+v, want rescue=%v", rank, tt.want)
			}
		})
	}
}

func TestUsageRankLess(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tt := range []struct {
		name string
		a, b usageRank
	}{
		{"exhaustion", usageRank{}, usageRank{exhausted: true, rescue: true, score: 1}},
		{"rescue", usageRank{rescue: true, score: 0.3}, usageRank{score: 0.9}},
		{"weekly reset", usageRank{rescue: true, weeklyReset: now, score: 0.3}, usageRank{rescue: true, weeklyReset: now.Add(time.Hour), score: 0.9}},
		{"score", usageRank{score: 0.8}, usageRank{score: 0.3, reset: now}},
		{"reset", usageRank{score: 0.8, reset: now}, usageRank{score: 0.8, reset: now.Add(time.Hour)}},
		{"zero reset last", usageRank{score: 0.8, reset: now}, usageRank{score: 0.8}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !usageRankLess(tt.a, tt.b, "z", "a") || usageRankLess(tt.b, tt.a, "a", "z") {
				t.Fatalf("incorrect ordering of %+v and %+v", tt.a, tt.b)
			}
		})
	}
	if !usageRankLess(usageRank{}, usageRank{}, "a", "b") || usageRankLess(usageRank{}, usageRank{}, "a", "a") {
		t.Fatal("identical ranks must sort by auth ID")
	}
	for _, rp := range []rescuePolicy{{within: 24 * time.Hour, headroom: 0.25}, {}} {
		a := usageRankFor(&Auth{Provider: "claude", Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.60",
			"Anthropic-Ratelimit-Unified-7d-Reset":       now.Add(6 * 24 * time.Hour).Format(time.RFC3339Nano),
		}}}, "", now, rp)
		b := usageRankFor(&Auth{Provider: "claude", Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.30",
			"Anthropic-Ratelimit-Unified-7d-Reset":       now.Add(4 * time.Hour).Format(time.RFC3339Nano),
		}}}, "", now, rp)
		if got := usageRankLess(b, a, "B", "A"); got != (rp.within > 0) {
			t.Fatalf("B before A = %v for policy %+v", got, rp)
		}
	}
}
