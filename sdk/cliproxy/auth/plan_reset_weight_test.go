package auth

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func withCodexResetAt(id string, reset time.Time) *Auth {
	auth := quotaAuth(id, "codex", codexSignals("10", "10080", "false", "false"))
	auth.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(reset.Unix(), 10)
	return auth
}

func TestPlanResetRoutingWeightPrefersSoonerResetAndFloorsMissing(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	soon := withCodexResetAt("soon", now.Add(time.Hour))
	later := withCodexResetAt("later", now.Add(24*time.Hour))
	missing := quotaAuth("missing", "codex", codexSignals("10", "10080", "false", "false"))

	gotSoon := planResetRoutingWeight("codex", soon, now)
	gotLater := planResetRoutingWeight("codex", later, now)
	gotMissing := planResetRoutingWeight("codex", missing, now)
	if gotSoon != 1+86400/3600 {
		t.Fatalf("soon weight = %d, want %d", gotSoon, 1+86400/3600)
	}
	if gotLater != 1+86400/(24*3600) {
		t.Fatalf("later weight = %d, want %d", gotLater, 1+86400/(24*3600))
	}
	if gotMissing != planResetWeightFloor {
		t.Fatalf("missing weight = %d, want floor %d", gotMissing, planResetWeightFloor)
	}
	if gotSoon <= gotLater || gotLater < gotMissing {
		t.Fatalf("weights soon=%d later=%d missing=%d", gotSoon, gotLater, gotMissing)
	}
}

func TestPlanResetDeadlineUsesCodexPrimaryAndSoonerClaudeWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	observed := quotaAuth("observed", "codex", codexSignals("10", "10080", "false", "false"))
	observed.Quota.ObservedAt = now.Add(-2 * time.Hour)
	observed.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "3600"
	deadline, ok := planResetDeadline("codex", observed, now)
	if !ok {
		t.Fatal("reset-after was missing")
	}
	if !deadline.Equal(now.Add(-time.Hour)) {
		t.Fatalf("reset-after deadline = %s, want observedAt+3600s", deadline)
	}

	claude := quotaAuth("claude", "claude", claudeSignals("allowed", "0.2", "allowed", "0.3", "rejected"))
	claude.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10)
	claude.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10)
	got, ok := planResetDeadline("claude", claude, now)
	if !ok || !got.Equal(now.Add(4*time.Hour)) {
		t.Fatalf("claude reset = %s ok=%v, want the sooner 5h reset", got, ok)
	}
	if _, ok := planResetDeadline("claude", quotaAuth("bare", "claude", claudeSignals("allowed", "0.2", "allowed", "0.3", "rejected")), now); ok {
		t.Fatal("claude account with no reset field was given a deadline")
	}
}

func TestPlanQuotaFirstNewSessionIsWeightedNotFillFirst(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	now := time.Now()
	later := withCodexResetAt("auth-a", now.Add(24*time.Hour))
	soon := withCodexResetAt("auth-z", now.Add(time.Hour))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("first-new"), []*Auth{later, soon})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != soon.ID {
		t.Fatalf("Pick() = %v, want soonest reset %s", got, soon.ID)
	}
}

func TestPlanQuotaWeightedSpreadAfterRecentActivity(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()
	now := time.Now()
	later := withCodexResetAt("auth-a", now.Add(24*time.Hour))
	soon := withCodexResetAt("auth-z", now.Add(time.Hour))
	auths := []*Auth{later, soon}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("spread-"+strconv.Itoa(i)), auths)
		if errPick != nil {
			t.Fatalf("Pick(%d) error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	if counts[soon.ID] <= counts[later.ID] || counts[later.ID] == 0 {
		t.Fatalf("counts = %#v, want soonest %s to lead while %s still receives picks", counts, soon.ID, later.ID)
	}
}

func TestPlanQuotaStickySurvivesSoonerReset(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	now := time.Now()
	later := withCodexResetAt("auth-b", now.Add(24*time.Hour))
	opts := sessionPickOpts("stay")
	first, errFirst := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{later})
	if errFirst != nil {
		t.Fatalf("first Pick() error = %v", errFirst)
	}
	soon := withCodexResetAt("auth-a", now.Add(time.Hour))
	second, errSecond := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{soon, later})
	if errSecond != nil {
		t.Fatalf("second Pick() error = %v", errSecond)
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("sticky Pick() = %v, want to stay on %s", second, first.ID)
	}
}

func TestPlanQuotaMissingResetIsNotSoonest(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	missing := quotaAuth("auth-a", "codex", codexSignals("10", "10080", "false", "false"))
	known := withCodexResetAt("auth-b", time.Now().Add(6*time.Hour))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("missing-reset"), []*Auth{missing, known})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != known.ID {
		t.Fatalf("Pick() = %v, want known reset %s", got, known.ID)
	}
}
