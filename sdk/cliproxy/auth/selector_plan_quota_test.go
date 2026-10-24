package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func codexSignals(used, window, limitReached, hasCredits string) map[string]string {
	signals := map[string]string{}
	if used != "" {
		signals["X-Codex-Primary-Used-Percent"] = used
	}
	if window != "" {
		signals["X-Codex-Primary-Window-Minutes"] = window
	}
	if limitReached != "" {
		signals["X-Codex-Limit-Reached"] = limitReached
	}
	if hasCredits != "" {
		signals["X-Codex-Credits-Has-Credits"] = hasCredits
	}
	return signals
}

func claudeSignals(status5h, util5h, status7d, util7d, overage string) map[string]string {
	signals := map[string]string{}
	if status5h != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Status"] = status5h
	}
	if util5h != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = util5h
	}
	if status7d != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Status"] = status7d
	}
	if util7d != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = util7d
	}
	if overage != "" {
		signals["Anthropic-Ratelimit-Unified-Overage-Status"] = overage
	}
	return signals
}

func quotaAuth(id, provider string, signals map[string]string) *Auth {
	auth := &Auth{ID: id, Provider: provider}
	if signals != nil {
		auth.Quota.Signals = signals
	}
	return auth
}

func sessionPickOpts(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_` + id + `"}}`)}
}

func TestPlanQuotaSelectorPrefersIncludedCodexPlanOverCredits(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	exhausted := quotaAuth("auth-a", "codex", codexSignals("100", "10080", "false", "true"))
	healthy := quotaAuth("auth-b", "codex", codexSignals("12", "10080", "false", "false"))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("codex-prefer"), []*Auth{exhausted, healthy})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != healthy.ID {
		t.Fatalf("Pick() = %v, want %s", got, healthy.ID)
	}
}

func TestPlanQuotaSelectorLimitReachedIsExhaustedEvenWithCredits(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	exhausted := quotaAuth("auth-a", "codex", codexSignals("10", "10080", "true", "true"))
	healthy := quotaAuth("auth-b", "codex", codexSignals("10", "10080", "false", "false"))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("codex-limit"), []*Auth{exhausted, healthy})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != healthy.ID {
		t.Fatalf("Pick() = %v, want %s", got, healthy.ID)
	}
}

func TestPlanQuotaSelectorUsesCreditsWhenEveryCodexPlanIsExhausted(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	first := quotaAuth("auth-a", "codex", codexSignals("100", "10080", "true", "true"))
	second := quotaAuth("auth-b", "codex", codexSignals("100", "10080", "false", "true"))
	auths := []*Auth{first, second}
	got1, err1 := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("credit-1"), auths)
	got2, err2 := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("credit-2"), auths)
	if err1 != nil || err2 != nil {
		t.Fatalf("Pick() errors = %v, %v", err1, err2)
	}
	if got1 == nil || got2 == nil || got1.ID != first.ID || got2.ID != second.ID {
		t.Fatalf("credit fallback picks = %q, %q, want %s then %s", idOf(got1), idOf(got2), first.ID, second.ID)
	}
}

func TestPlanQuotaSelectorUnknownSnapshotStaysEligible(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	exhausted := quotaAuth("auth-a", "codex", codexSignals("100", "10080", "true", "true"))
	unknown := quotaAuth("auth-b", "codex", nil)
	creditsOnly := quotaAuth("auth-c", "codex", map[string]string{"X-Codex-Credits-Has-Credits": "true"})
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("unknown-snap"), []*Auth{exhausted, unknown, creditsOnly})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != unknown.ID {
		t.Fatalf("Pick() = %v, want %s (no snapshot stays with the preferred set)", got, unknown.ID)
	}
}

func TestPlanQuotaSelectorNonWeeklyWindowIsNotPlanExhausted(t *testing.T) {
	t.Parallel()
	if codexPlanWindowExhausted(codexSignals("100", "300", "false", "true")) {
		t.Fatal("5h primary window at 100% was treated as included-plan exhaustion")
	}
	if !codexPlanWindowExhausted(codexSignals("100", "", "false", "true")) {
		t.Fatal("used percent >= 100 with no window was not treated as plan exhaustion")
	}
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	shortWindow := quotaAuth("auth-a", "codex", codexSignals("100", "300", "false", "false"))
	weeklyFull := quotaAuth("auth-b", "codex", codexSignals("100", "10080", "false", "true"))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("window"), []*Auth{shortWindow, weeklyFull})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != shortWindow.ID {
		t.Fatalf("Pick() = %v, want %s", got, shortWindow.ID)
	}
}

func TestPlanQuotaSelectorRebindsAwayFromExhaustedStickyAuth(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	authA := quotaAuth("auth-a", "codex", codexSignals("10", "10080", "false", "false"))
	authB := quotaAuth("auth-b", "codex", codexSignals("20", "10080", "false", "false"))
	opts := sessionPickOpts("rebind-sticky")
	auths := []*Auth{authA, authB}
	first, errFirst := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errFirst != nil {
		t.Fatalf("first Pick() error = %v", errFirst)
	}
	if first.ID != authA.ID {
		t.Fatalf("first Pick() = %s, want %s", first.ID, authA.ID)
	}

	authA.Quota.Signals = codexSignals("100", "10080", "true", "true")
	second, errSecond := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errSecond != nil {
		t.Fatalf("second Pick() error = %v", errSecond)
	}
	if second.ID != authB.ID {
		t.Fatalf("second Pick() = %s, want rebind to %s", second.ID, authB.ID)
	}
	third, errThird := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errThird != nil {
		t.Fatalf("third Pick() error = %v", errThird)
	}
	if third.ID != authB.ID {
		t.Fatalf("third Pick() = %s, want sticky %s", third.ID, authB.ID)
	}
}

func TestPlanQuotaSelectorDoesNotRebindBetweenQuotaAuths(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	authA := quotaAuth("auth-a", "codex", codexSignals("100", "10080", "true", "true"))
	authB := quotaAuth("auth-b", "codex", codexSignals("15", "10080", "false", "false"))
	opts := sessionPickOpts("keep-quota")
	auths := []*Auth{authA, authB}
	first, errFirst := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errFirst != nil {
		t.Fatalf("first Pick() error = %v", errFirst)
	}
	if first.ID != authB.ID {
		t.Fatalf("first Pick() = %s, want %s", first.ID, authB.ID)
	}

	authA.Quota.Signals = codexSignals("5", "10080", "false", "false")
	second, errSecond := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if errSecond != nil {
		t.Fatalf("second Pick() error = %v", errSecond)
	}
	if second.ID != authB.ID {
		t.Fatalf("second Pick() = %s, want to stay on quota auth %s", second.ID, authB.ID)
	}
}

func TestPlanQuotaSelectorPrefersLowerPriorityPlanOverHighPriorityCredits(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()

	high := quotaAuth("auth-z", "codex", codexSignals("100", "10080", "true", "true"))
	high.Attributes = map[string]string{"priority": "10"}
	low := quotaAuth("auth-a", "codex", codexSignals("8", "10080", "false", "false"))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", sessionPickOpts("priority"), []*Auth{high, low})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != low.ID {
		t.Fatalf("Pick() = %v, want lower-priority plan auth %s", got, low.ID)
	}
}

func TestPlanQuotaSelectorKeepsPriorityWhenEveryPlanIsExhausted(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	defer selector.Stop()

	high := quotaAuth("auth-z", "codex", codexSignals("100", "10080", "true", "true"))
	high.Attributes = map[string]string{"priority": "10"}
	low := quotaAuth("auth-a", "codex", codexSignals("100", "10080", "true", "true"))
	got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{OriginalRequest: []byte(`{"model":"gpt-5"}`)}, []*Auth{low, high})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != high.ID {
		t.Fatalf("Pick() = %v, want highest-priority exhausted auth %s", got, high.ID)
	}
}

func TestPlanQuotaSelectorClaudeOverageAndWindows(t *testing.T) {
	t.Parallel()
	healthy := claudeSignals("allowed", "0.2", "allowed", "0.4", "rejected")
	if claudePlanWindowExhausted(healthy) {
		t.Fatal("healthy windows with overage rejected were treated as plan exhaustion")
	}
	if !claudePlanWindowExhausted(claudeSignals("allowed", "0.2", "allowed", "0.4", "allowed")) {
		t.Fatal("overage allowed was not treated as plan exhaustion")
	}
	if !claudePlanWindowExhausted(claudeSignals("allowed", "0.2", "allowed", "0.4", "in_use")) {
		t.Fatal("overage in_use was not treated as plan exhaustion")
	}
	if !claudePlanWindowExhausted(claudeSignals("allowed", "1", "allowed", "0.1", "")) {
		t.Fatal("utilization >= 1 was not treated as plan exhaustion")
	}
	if !claudePlanWindowExhausted(claudeSignals("rejected", "0.1", "allowed", "0.1", "rejected")) {
		t.Fatal("rejected 5h status was not treated as plan exhaustion")
	}
	if claudePlanWindowExhausted(map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected"}) {
		t.Fatal("unified status alone was treated as plan exhaustion")
	}

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	overage := quotaAuth("auth-a", "claude", claudeSignals("allowed", "0.2", "allowed", "0.4", "allowed"))
	plan := quotaAuth("auth-b", "claude", healthy)
	got, errPick := selector.Pick(context.Background(), "claude", "claude-sonnet-4-5", sessionPickOpts("claude-overage"), []*Auth{overage, plan})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != plan.ID {
		t.Fatalf("Pick() = %v, want %s", got, plan.ID)
	}
}

func TestPlanQuotaSelectorIgnoresSnapshotsForOtherProviders(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	marked := quotaAuth("auth-a", "gemini", codexSignals("100", "10080", "true", "true"))
	other := quotaAuth("auth-b", "gemini", codexSignals("1", "10080", "false", "false"))
	got, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", sessionPickOpts("gemini"), []*Auth{marked, other})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != marked.ID {
		t.Fatalf("Pick() = %v, want %s (non codex/claude snapshots do not filter)", got, marked.ID)
	}
}

func idOf(auth *Auth) string {
	if auth == nil {
		return "<nil>"
	}
	return auth.ID
}
