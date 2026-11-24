package auth

import (
	"math"
	"strconv"
	"strings"
	"time"
)

type usageRank struct {
	exhausted   bool
	rescue      bool
	weeklyReset time.Time
	score       float64
	reset       time.Time
}

type rescuePolicy struct {
	within   time.Duration
	headroom float64
}

func usageRankFor(auth *Auth, modelKey string, now time.Time, rp rescuePolicy) usageRank {
	if auth == nil {
		return usageRank{}
	}
	snapshot := auth.Quota
	if state := auth.ModelStates[modelKey]; state != nil && state.Quota.ObservedAt.After(snapshot.ObservedAt) {
		snapshot = state.Quota
	}
	if len(snapshot.Signals) == 0 {
		return usageRank{}
	}
	signals := snapshot.Signals
	var rank usageRank
	hasScore := false
	addWindow := func(utilization float64, valid bool, reset time.Time, weekly bool) {
		if !valid || (!reset.IsZero() && !reset.After(now)) {
			return
		}
		utilization = math.Min(utilization, 1)
		if !hasScore || utilization > rank.score || (utilization == rank.score && usageResetLess(reset, rank.reset)) {
			rank.score = utilization
			rank.reset = reset
			hasScore = true
		}
		if weekly {
			rank.weeklyReset = reset
			rank.rescue = rp.within > 0 && rp.headroom > 0 && reset.After(now) &&
				reset.Sub(now) <= rp.within && 1-utilization >= rp.headroom
		}
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		for _, window := range []string{"5h", "7d"} {
			prefix := "Anthropic-Ratelimit-Unified-" + window
			reset := parseUsageReset(signals[prefix+"-Reset"])
			if strings.EqualFold(strings.TrimSpace(signals[prefix+"-Status"]), "rejected") && reset.After(now) {
				rank.exhausted = true
			}
			utilization, valid := parseUsageNumber(signals[prefix+"-Utilization"])
			addWindow(utilization, valid, reset, window == "7d")
		}
	case "codex":
		rank.exhausted = strings.EqualFold(strings.TrimSpace(signals["X-Codex-Limit-Reached"]), "true")
		_, primaryLength := signals["X-Codex-Primary-Window-Minutes"]
		_, secondaryLength := signals["X-Codex-Secondary-Window-Minutes"]
		for _, window := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + window
			resetValue, hasReset := signals[prefix+"-Reset-At"]
			reset := parseUsageReset(resetValue)
			if !hasReset {
				if seconds, valid := parseUsageNumber(signals[prefix+"-Reset-After-Seconds"]); valid && seconds < float64(math.MaxInt64)/float64(time.Second) {
					reset = snapshot.ObservedAt.Add(time.Duration(seconds * float64(time.Second)))
				}
			}
			minutes, validLength := parseUsageNumber(signals[prefix+"-Window-Minutes"])
			weekly := (validLength && minutes >= 10080) || (!primaryLength && !secondaryLength && window == "Secondary")
			utilization, valid := parseUsageNumber(signals[prefix+"-Used-Percent"])
			addWindow(utilization/100, valid, reset, weekly)
		}
	}
	rank.rescue = rank.rescue && !rank.exhausted
	return rank
}

func parseUsageNumber(value string) (float64, bool) {
	number, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return number, errParse == nil && !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0
}

func parseUsageReset(value string) time.Time {
	value = strings.TrimSpace(value)
	if seconds, valid := parseUsageNumber(value); valid && seconds < float64(math.MaxInt64) {
		whole, fraction := math.Modf(seconds)
		return time.Unix(int64(whole), int64(fraction*float64(time.Second)))
	}
	if reset, errParse := time.Parse(time.RFC3339Nano, value); errParse == nil {
		return reset
	}
	return time.Time{}
}

func usageResetLess(a, b time.Time) bool {
	return !a.IsZero() && (b.IsZero() || a.Before(b))
}

func usageRankLess(a, b usageRank, aID, bID string) bool {
	if a.exhausted != b.exhausted {
		return !a.exhausted
	}
	if a.rescue != b.rescue {
		return a.rescue
	}
	if a.rescue && !a.weeklyReset.Equal(b.weeklyReset) {
		return a.weeklyReset.Before(b.weeklyReset)
	}
	if a.score != b.score {
		return a.score > b.score
	}
	if !a.reset.Equal(b.reset) {
		return usageResetLess(a.reset, b.reset)
	}
	return aID < bID
}
