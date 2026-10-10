package auth

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func manualResetTestSettings() ManualResetPrioritySettings {
	return ManualResetPrioritySettings{Enabled: true, Band: 24 * time.Hour, Boost: 8}
}

func baseWeights(auths []*Auth, now time.Time) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, a := range auths {
		weights[a.ID] = planResetRoutingWeight("codex", a, now)
	}
	return weights
}

func forgetAll(t *testing.T, auths ...*Auth) {
	t.Cleanup(func() {
		for _, a := range auths {
			ForgetCodexManualResets(a.ID)
		}
	})
}

func TestParseCodexManualResetCreditsKeepsAvailableCodexOnly(t *testing.T) {
	t.Parallel()
	body := []byte(`{"available_count":2,"credits":[
		{"id":"a","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-11-06T07:00:00.123456Z"},
		{"id":"b","reset_type":"codex_rate_limits","status":"consumed","expires_at":"2026-10-01T07:00:00Z"},
		{"id":"c","reset_type":"other","status":"available","expires_at":"2026-10-02T07:00:00Z"},
		{"id":"d","resetType":"codex_rate_limits","status":"available","expiresAt":"2026-10-22T07:00:00Z"}]}`)
	got, err := ParseCodexManualResetCredits(body)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if len(got) != 2 || got[0].Format("2006-01-02") != "2026-10-22" || got[1].Format("2006-01-02") != "2026-11-06" {
		t.Fatalf("expiries = %v, want sorted 10-22 and 11-06", got)
	}
	if _, err := ParseCodexManualResetCredits([]byte(`{"detail":"unauthorized"}`)); err == nil {
		t.Fatal("non-listing payload was accepted")
	}
	if empty, err := ParseCodexManualResetCredits([]byte(`{"credits":[]}`)); err != nil || len(empty) != 0 {
		t.Fatalf("empty listing = %v err=%v", empty, err)
	}
}

func TestManualResetPriorityBoostsSoonestExpiryWithinBand(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withCodexResetAt("mr-band-a", now.Add(80*time.Hour)) // weekly resets a day later
	b := withCodexResetAt("mr-band-b", now.Add(60*time.Hour))
	c := withCodexResetAt("mr-band-c", now.Add(61*time.Hour))
	d := withCodexResetAt("mr-band-d", now.Add(62*time.Hour))
	forgetAll(t, a, b, c, d)
	SetCodexManualResetExpiries(a.ID, []time.Time{now.Add(13 * 24 * time.Hour), now.Add(20 * 24 * time.Hour)}, now)
	SetCodexManualResetExpiries(b.ID, []time.Time{now.Add(20 * 24 * time.Hour)}, now)
	SetCodexManualResetExpiries(c.ID, nil, now) // no banked resets

	auths := []*Auth{a, b, c, d}
	weights := baseWeights(auths, now)
	boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings())
	if len(boosted) != 1 || !boosted[a.ID] {
		t.Fatalf("boosted = %v, want only %s", boosted, a.ID)
	}
	if weights[a.ID] != 8 || weights[b.ID] != 1 || weights[c.ID] != 1 || weights[d.ID] != 1 {
		t.Fatalf("weights = %v, want a=8 others=1", weights)
	}

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	selector.lastActivityUnix.Store(time.Now().UnixNano())
	counts := map[string]int{}
	for i := 0; i < 55; i++ {
		got, err := selector.Pick(context.Background(), "codex", "gpt-6-sol", sessionPickOpts("band-"+strconv.Itoa(i)), auths)
		if err != nil {
			t.Fatalf("Pick(%d) error = %v", i, err)
		}
		counts[got.ID]++
	}
	if counts[a.ID] != 40 || counts[b.ID] != 5 || counts[c.ID] != 5 || counts[d.ID] != 5 {
		t.Fatalf("counts = %v, want a=40 (8/11) and 5 each for the rest", counts)
	}
}

func TestManualResetPriorityIgnoredWhenWeeklyResetsFarApart(t *testing.T) {
	t.Parallel()
	now := time.Now()
	soon := withCodexResetAt("mr-far-soon", now.Add(2*time.Hour))
	late := withCodexResetAt("mr-far-late", now.Add(72*time.Hour))
	forgetAll(t, soon, late)
	SetCodexManualResetExpiries(late.ID, []time.Time{now.Add(5 * 24 * time.Hour)}, now)
	auths := []*Auth{soon, late}
	weights := baseWeights(auths, now)
	want := baseWeights(auths, now)
	if boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v, want none outside the band", boosted)
	}
	for id, w := range want {
		if weights[id] != w {
			t.Fatalf("weights = %v, want unchanged %v", weights, want)
		}
	}
	// A wider configured band lets the boost apply.
	wide := manualResetTestSettings()
	wide.Band = 96 * time.Hour
	if boosted := applyManualResetPriority("codex", auths, weights, now, wide); !boosted[late.ID] {
		t.Fatalf("boosted = %v, want %s with a 96h band", boosted, late.ID)
	}
}

func TestManualResetPriorityMissingDataKeepsCurrentWeights(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withCodexResetAt("mr-missing-a", now.Add(50*time.Hour))
	b := withCodexResetAt("mr-missing-b", now.Add(51*time.Hour))
	auths := []*Auth{a, b}
	weights := baseWeights(auths, now)
	want := baseWeights(auths, now)
	if boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v without data", boosted)
	}
	for id, w := range want {
		if weights[id] != w {
			t.Fatalf("weights = %v, want %v", weights, want)
		}
	}
	// Stale snapshots count as missing.
	forgetAll(t, a)
	SetCodexManualResetExpiries(a.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now.Add(-manualResetDataTTL-time.Minute))
	if boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v from a stale snapshot", boosted)
	}
	// Disabled setting is a no-op even with data.
	SetCodexManualResetExpiries(a.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now)
	off := manualResetTestSettings()
	off.Enabled = false
	if boosted := applyManualResetPriority("codex", auths, weights, now, off); boosted != nil {
		t.Fatalf("boosted = %v while disabled", boosted)
	}
}

func TestManualResetPriorityIgnoresExpiredResets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	expiredOnly := withCodexResetAt("mr-exp-a", now.Add(50*time.Hour))
	mixed := withCodexResetAt("mr-exp-b", now.Add(50*time.Hour))
	forgetAll(t, expiredOnly, mixed)
	SetCodexManualResetExpiries(expiredOnly.ID, []time.Time{now.Add(-time.Hour)}, now)
	SetCodexManualResetExpiries(mixed.ID, []time.Time{now.Add(-2 * time.Hour), now.Add(9 * 24 * time.Hour)}, now)
	auths := []*Auth{expiredOnly, mixed}
	weights := baseWeights(auths, now)
	boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings())
	if len(boosted) != 1 || !boosted[mixed.ID] {
		t.Fatalf("boosted = %v, want only %s (its unexpired reset)", boosted, mixed.ID)
	}
	if expiry, count, ok := codexEarliestManualReset(mixed.ID, now); !ok || count != 1 || !expiry.Equal(now.Add(9*24*time.Hour)) {
		t.Fatalf("earliest = %v count=%d ok=%v", expiry, count, ok)
	}
}

func TestManualResetPrioritySkipsPlanExhaustedCredential(t *testing.T) {
	t.Parallel()
	now := time.Now()
	exhausted := quotaAuth("mr-exh-a", "codex", codexSignals("100", "10080", "false", "true"))
	exhausted.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(now.Add(50*time.Hour).Unix(), 10)
	healthy := withCodexResetAt("mr-exh-b", now.Add(55*time.Hour))
	other := withCodexResetAt("mr-exh-c", now.Add(56*time.Hour))
	forgetAll(t, exhausted, healthy, other)
	SetCodexManualResetExpiries(exhausted.ID, []time.Time{now.Add(5 * 24 * time.Hour)}, now)
	SetCodexManualResetExpiries(healthy.ID, []time.Time{now.Add(12 * 24 * time.Hour)}, now)
	auths := []*Auth{exhausted, healthy, other}
	weights := baseWeights(auths, now)
	boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings())
	if boosted[exhausted.ID] || !boosted[healthy.ID] {
		t.Fatalf("boosted = %v, want %s (exhausted %s skipped)", boosted, healthy.ID, exhausted.ID)
	}

	// Through the selector the exhausted account never receives a new session.
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	selector.lastActivityUnix.Store(time.Now().UnixNano())
	for i := 0; i < 20; i++ {
		got, err := selector.Pick(context.Background(), "codex", "gpt-6-sol", sessionPickOpts("exh-"+strconv.Itoa(i)), auths)
		if err != nil {
			t.Fatalf("Pick(%d) error = %v", i, err)
		}
		if got.ID == exhausted.ID {
			t.Fatalf("Pick(%d) chose plan-exhausted %s", i, exhausted.ID)
		}
	}
}

func TestManualResetPriorityTiesAndClaudeUnchanged(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withCodexResetAt("mr-tie-a", now.Add(50*time.Hour))
	b := withCodexResetAt("mr-tie-b", now.Add(50*time.Hour))
	forgetAll(t, a, b)
	expiry := now.Add(7 * 24 * time.Hour)
	SetCodexManualResetExpiries(a.ID, []time.Time{expiry}, now)
	SetCodexManualResetExpiries(b.ID, []time.Time{expiry.Add(30 * time.Minute)}, now)
	auths := []*Auth{a, b}
	weights := baseWeights(auths, now)
	boosted := applyManualResetPriority("codex", auths, weights, now, manualResetTestSettings())
	if !boosted[a.ID] || !boosted[b.ID] {
		t.Fatalf("boosted = %v, want both tied credentials", boosted)
	}

	claude := quotaAuth("mr-claude", "claude", claudeSignals("allowed", "0.2", "allowed", "0.3", "rejected"))
	forgetAll(t, claude)
	SetCodexManualResetExpiries(claude.ID, []time.Time{expiry}, now)
	cw := map[string]int64{claude.ID: 1}
	if got := applyManualResetPriority("claude", []*Auth{claude}, cw, now, manualResetTestSettings()); got != nil || cw[claude.ID] != 1 {
		t.Fatalf("claude boosted = %v weights=%v", got, cw)
	}
}

func TestShortAuthLabelNeverLeaksEmail(t *testing.T) {
	t.Parallel()
	got := ShortAuthLabel(&Auth{ID: "codex-3f7c380d-someone@example.com-pro.json"})
	if got != "codex-3f7c380d" {
		t.Fatalf("label = %q", got)
	}
	other := ShortAuthLabel(&Auth{ID: "someone@example.com"})
	if other == "" || other == "someone@example.com" || len(other) > 16 {
		t.Fatalf("fallback label = %q", other)
	}
}
