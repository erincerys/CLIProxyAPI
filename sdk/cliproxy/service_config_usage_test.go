package cliproxy

import (
	"context"
	"math"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFillMostUsedRoutingSelector(t *testing.T) {
	for _, input := range []string{"fill-most-used", "fmu", "FillMostUsed", " FMU "} {
		t.Run(input, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: input}})
			selector, ok := newRoutingSelector(state).(*coreauth.FillMostUsedSelector)
			if state.strategy != "fill-most-used" || !ok {
				t.Fatalf("strategy = %s, selector = %T", state.strategy, newRoutingSelector(state))
			}
			if selector.RescueWithin != 24*time.Hour || selector.RescueHeadroom != 0.25 {
				t.Fatalf("default rescue policy = %+v", selector)
			}
		})
	}
}

func TestUsageRescueRoutingConfig(t *testing.T) {
	for _, tt := range []struct {
		name     string
		yaml     string
		within   time.Duration
		headroom float64
	}{
		{"absent", "", 24 * time.Hour, 0.25},
		{"disabled", "  usage-rescue-within: 0\n  usage-rescue-headroom: 0\n", 0, 0},
		{"garbage", "  usage-rescue-within: garbage\n  usage-rescue-headroom: garbage\n", 24 * time.Hour, 0.25},
		{"negative duration", "  usage-rescue-within: -1h\n", 24 * time.Hour, 0.25},
		{"custom", "  usage-rescue-within: 6h30m\n  usage-rescue-headroom: 0.4\n", 6*time.Hour + 30*time.Minute, 0.4},
		{"clamp low", "  usage-rescue-headroom: -0.5\n", 24 * time.Hour, 0},
		{"clamp high", "  usage-rescue-headroom: 1.5\n", 24 * time.Hour, 1},
		{"NaN", "  usage-rescue-headroom: .nan\n", 24 * time.Hour, 0.25},
		{"infinite", "  usage-rescue-headroom: .inf\n", 24 * time.Hour, 0.25},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := internalconfig.ParseConfigBytes([]byte("config-version: 8\nrouting:\n  strategy: fill-most-used\n" + tt.yaml))
			if errParse != nil {
				t.Fatal(errParse)
			}
			state := normalizedRoutingRuntimeState(cfg)
			selector := newRoutingSelector(state).(*coreauth.FillMostUsedSelector)
			if selector.RescueWithin != tt.within || selector.RescueHeadroom != tt.headroom {
				t.Fatalf("rescue policy = %+v, want %v/%v", selector, tt.within, tt.headroom)
			}
		})
	}
	if state := normalizedRoutingRuntimeState(nil); state.usageRescueWithin != 24*time.Hour || state.usageRescueHeadroom != 0.25 {
		t.Fatalf("nil config policy = %+v", state)
	}
	nan := math.NaN()
	if state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{UsageRescueHeadroom: &nan}}); state.usageRescueHeadroom != 0.25 {
		t.Fatalf("unparsable runtime headroom = %v, want 0.25", state.usageRescueHeadroom)
	}
}

func TestUsageRescueConfigRebuildsSelector(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	headroom := 0.4
	for _, routing := range []internalconfig.RoutingConfig{
		{Strategy: "fill-most-used"},
		{Strategy: "fill-most-used", UsageRescueWithin: "0"},
		{Strategy: "fill-most-used", UsageRescueWithin: "0", UsageRescueHeadroom: &headroom},
		{Strategy: "fill-most-used", SessionAffinity: true, UsageRescueWithin: "4h", UsageRescueHeadroom: &headroom},
	} {
		previous := service.coreManager.Selector()
		cfg := &internalconfig.Config{Routing: routing, AuthDir: t.TempDir()}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
			t.Fatal("applyManagerConfig failed")
		}
		selector := service.coreManager.Selector()
		if selector == previous {
			t.Fatal("changed rescue configuration did not rebuild selector")
		}
		state := normalizedRoutingRuntimeState(cfg)
		if routing.SessionAffinity {
			affinity, ok := selector.(*coreauth.SessionAffinitySelector)
			if !ok {
				t.Fatalf("selector = %T, want session affinity", selector)
			}
			t.Cleanup(affinity.Stop)
		} else {
			mostUsed := selector.(*coreauth.FillMostUsedSelector)
			if mostUsed.RescueWithin != state.usageRescueWithin || mostUsed.RescueHeadroom != state.usageRescueHeadroom {
				t.Fatalf("rebuilt rescue policy = %+v, want %+v", mostUsed, state)
			}
		}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 2}) {
			t.Fatal("repeat applyManagerConfig failed")
		}
		if service.coreManager.Selector() != selector {
			t.Fatal("unchanged configuration rebuilt selector")
		}
	}
}
