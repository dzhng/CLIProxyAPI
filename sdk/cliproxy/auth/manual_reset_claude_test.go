package auth

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

const claudeTestModel = "claude-opus-5-5"

// withClaudeWeeklyReset is a healthy Claude credential whose 7-day window
// resets at reset (no 5h reset, so the plan-reset weight follows the 7d reset).
func withClaudeWeeklyReset(id string, reset time.Time) *Auth {
	auth := quotaAuth(id, "claude", claudeSignals("allowed", "0.2", "allowed", "0.3", ""))
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(reset.Unix(), 10)
	return auth
}

func claudeBaseWeights(auths []*Auth, now time.Time) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, a := range auths {
		weights[a.ID] = planResetRoutingWeight("claude", a, now)
	}
	return weights
}

func TestParseClaudeResetGrants(t *testing.T) {
	t.Parallel()
	body := []byte(`{"five_hour":{"utilization":3},"cedar_ember":{"eligible":true,"ineligible_reason":null,"at_limit":false,
		"weekly_resets_at":"2026-10-15T08:59:00Z","cooldown_until":null,"next_grant_id":"opus_launch",
		"grants":[
		{"id":"opus_launch","label":"Claude Opus 5.5 launch","resets_total":1,"resets_left":1,"starts_at":"2026-10-08T16:00:00Z","ends_at":"2026-10-22T16:00:00Z","clears":["five_hour","seven_day"],"paused":false,"usable_now":false,"use_requires_limit":true},
		{"id":"double","resets_total":3,"resets_left":2,"ends_at":"2026-11-30T08:00:00.5Z"},
		{"id":"used_up","resets_total":1,"resets_left":0,"ends_at":"2026-10-20T00:00:00Z"},
		{"id":"paused_one","resets_total":1,"resets_left":1,"ends_at":"2026-10-19T00:00:00Z","paused":true},
		{"id":"no_end","resets_total":1,"resets_left":1,"ends_at":null}]}}`)
	got, err := ParseClaudeResetGrants(body)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if len(got) != 3 || got[0].Format("2006-01-02") != "2026-10-22" || got[1].Format("2006-01-02") != "2026-11-30" || !got[1].Equal(got[2]) {
		t.Fatalf("expiries = %v, want 10-22 then 11-30 twice", got)
	}

	ineligible, err := ParseClaudeResetGrants([]byte(`{"cedar_ember":{"eligible":false,"ineligible_reason":"tier","grants":[{"id":"g","resets_total":1,"resets_left":1,"ends_at":"2026-10-22T16:00:00Z"}]}}`))
	if err != nil || len(ineligible) != 0 {
		t.Fatalf("ineligible = %v err=%v, want empty", ineligible, err)
	}
	if empty, err := ParseClaudeResetGrants([]byte(`{"cedar_ember":{"eligible":true}}`)); err != nil || len(empty) != 0 {
		t.Fatalf("no grants = %v err=%v", empty, err)
	}

	for _, bad := range []string{
		`{"five_hour":{"utilization":3}}`,
		`{"cedar_ember":null}`,
		`{"cedar_ember":{}}`,
		`{"cedar_ember":{"eligible":1}}`,
		`{"cedar_ember":{"eligible":true,"grants":{}}}`,
		`{"cedar_ember":{"eligible":true,"cooldown_until":"bad"}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"g","resets_total":1,"resets_left":1},{"id":"g","resets_total":1,"resets_left":1}]}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"../bad","resets_total":1,"resets_left":1}]}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"g","resets_total":1,"resets_left":2}]}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"g","resets_total":1.5,"resets_left":1}]}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"g","resets_total":1,"resets_left":1,"ends_at":"soon"}]}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"id":"g","resets_total":1,"resets_left":1,"paused":"false"}]}}`,
		`not json`,
	} {
		if _, err := ParseClaudeResetGrants([]byte(bad)); err == nil {
			t.Fatalf("malformed payload accepted: %s", bad)
		}
	}

	// An invalid payload keeps the previous snapshot.
	const id = "mr-claude-parse-keep"
	t.Cleanup(func() { ForgetClaudeManualResets(id) })
	stamp := time.Now().Add(-time.Minute)
	if _, err := RecordClaudeResetGrants(id, body, stamp); err != nil {
		t.Fatalf("record error = %v", err)
	}
	if _, err := RecordClaudeResetGrants(id, []byte(`{"error":"rate_limited"}`), time.Now()); err == nil {
		t.Fatal("error payload was recorded")
	}
	if at, ok := ClaudeManualResetFetchedAt(id); !ok || !at.Equal(stamp) {
		t.Fatalf("fetchedAt = %v ok=%v, want the earlier snapshot", at, ok)
	}
	if _, ok := CodexManualResetFetchedAt(id); ok {
		t.Fatal("Claude snapshot reported as a Codex snapshot")
	}
}

func TestClaudeManualResetPriorityBoostsSoonestExpiryWithinBand(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withClaudeWeeklyReset("mr-cl-band-a", now.Add(80*time.Hour)) // weekly resets a day later
	b := withClaudeWeeklyReset("mr-cl-band-b", now.Add(60*time.Hour))
	c := withClaudeWeeklyReset("mr-cl-band-c", now.Add(61*time.Hour))
	d := withClaudeWeeklyReset("mr-cl-band-d", now.Add(62*time.Hour))
	forgetAll(t, a, b, c, d)
	SetClaudeManualResetExpiries(a.ID, []time.Time{now.Add(11 * 24 * time.Hour)}, now)
	SetClaudeManualResetExpiries(b.ID, []time.Time{now.Add(20 * 24 * time.Hour)}, now)
	SetClaudeManualResetExpiries(c.ID, nil, now) // no banked resets

	auths := []*Auth{a, b, c, d}
	weights := claudeBaseWeights(auths, now)
	boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings())
	if len(boosted) != 1 || !boosted[a.ID] {
		t.Fatalf("boosted = %v, want only %s", boosted, a.ID)
	}
	if weights[a.ID] != 8 || weights[b.ID] != 1 || weights[c.ID] != 1 || weights[d.ID] != 1 {
		t.Fatalf("weights = %v, want a=8 others=1", weights)
	}

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	counts := map[string]int{}
	for i := 0; i < 55; i++ {
		got, err := selector.Pick(context.Background(), "claude", claudeTestModel, sessionPickOpts("cl-band-"+strconv.Itoa(i)), auths)
		if err != nil {
			t.Fatalf("Pick(%d) error = %v", i, err)
		}
		counts[got.ID]++
	}
	if counts[a.ID] != 40 || counts[b.ID] != 5 || counts[c.ID] != 5 || counts[d.ID] != 5 {
		t.Fatalf("counts = %v, want a=40 (8/11) and 5 each for the rest", counts)
	}

	// Outside the band nothing is boosted; a wider band boosts again.
	soon := withClaudeWeeklyReset("mr-cl-far-soon", now.Add(2*time.Hour))
	late := withClaudeWeeklyReset("mr-cl-far-late", now.Add(72*time.Hour))
	forgetAll(t, soon, late)
	SetClaudeManualResetExpiries(late.ID, []time.Time{now.Add(5 * 24 * time.Hour)}, now)
	far := []*Auth{soon, late}
	fw := claudeBaseWeights(far, now)
	if got := applyManualResetPriority("claude", far, fw, now, manualResetTestSettings()); got != nil {
		t.Fatalf("boosted = %v outside the band", got)
	}
	wide := manualResetTestSettings()
	wide.Band = 96 * time.Hour
	if got := applyManualResetPriority("claude", far, fw, now, wide); !got[late.ID] {
		t.Fatalf("boosted = %v, want %s with a 96h band", got, late.ID)
	}
}

func TestClaudeManualResetPriorityNoDataOrStaleData(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withClaudeWeeklyReset("mr-cl-missing-a", now.Add(50*time.Hour))
	b := withClaudeWeeklyReset("mr-cl-missing-b", now.Add(51*time.Hour))
	forgetAll(t, a, b)
	auths := []*Auth{a, b}
	weights := claudeBaseWeights(auths, now)
	want := claudeBaseWeights(auths, now)
	if boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v without data", boosted)
	}
	SetClaudeManualResetExpiries(a.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now.Add(-manualResetDataTTL-time.Minute))
	if boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v from a stale snapshot", boosted)
	}
	// Codex-kind data on a Claude credential is not Claude data.
	SetCodexManualResetExpiries(b.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now)
	if boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings()); boosted != nil {
		t.Fatalf("boosted = %v from Codex-kind data", boosted)
	}
	for id, w := range want {
		if weights[id] != w {
			t.Fatalf("weights = %v, want unchanged %v", weights, want)
		}
	}
	// Disabled setting is a no-op even with fresh data.
	SetClaudeManualResetExpiries(a.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now)
	off := manualResetTestSettings()
	off.Enabled = false
	if boosted := applyManualResetPriority("claude", auths, weights, now, off); boosted != nil {
		t.Fatalf("boosted = %v while disabled", boosted)
	}
}

func TestClaudeManualResetPriorityIgnoresExpiredResets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	expiredOnly := withClaudeWeeklyReset("mr-cl-exp-a", now.Add(50*time.Hour))
	mixed := withClaudeWeeklyReset("mr-cl-exp-b", now.Add(50*time.Hour))
	forgetAll(t, expiredOnly, mixed)
	SetClaudeManualResetExpiries(expiredOnly.ID, []time.Time{now.Add(-time.Hour)}, now)
	SetClaudeManualResetExpiries(mixed.ID, []time.Time{now.Add(-2 * time.Hour), now.Add(9 * 24 * time.Hour)}, now)
	auths := []*Auth{expiredOnly, mixed}
	weights := claudeBaseWeights(auths, now)
	boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings())
	if len(boosted) != 1 || !boosted[mixed.ID] {
		t.Fatalf("boosted = %v, want only %s (its unexpired reset)", boosted, mixed.ID)
	}
	if expiry, count, ok := claudeEarliestManualReset(mixed.ID, now); !ok || count != 1 || !expiry.Equal(now.Add(9*24*time.Hour)) {
		t.Fatalf("earliest = %v count=%d ok=%v", expiry, count, ok)
	}
}

func TestClaudeManualResetPrioritySkipsPlanExhaustedCredential(t *testing.T) {
	t.Parallel()
	now := time.Now()
	exhausted := quotaAuth("mr-cl-exh-a", "claude", claudeSignals("allowed", "0.4", "rejected", "1.0", ""))
	exhausted.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(50*time.Hour).Unix(), 10)
	healthy := withClaudeWeeklyReset("mr-cl-exh-b", now.Add(55*time.Hour))
	other := withClaudeWeeklyReset("mr-cl-exh-c", now.Add(56*time.Hour))
	forgetAll(t, exhausted, healthy, other)
	SetClaudeManualResetExpiries(exhausted.ID, []time.Time{now.Add(5 * 24 * time.Hour)}, now)
	SetClaudeManualResetExpiries(healthy.ID, []time.Time{now.Add(12 * 24 * time.Hour)}, now)
	auths := []*Auth{exhausted, healthy, other}
	weights := claudeBaseWeights(auths, now)
	boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings())
	if boosted[exhausted.ID] || !boosted[healthy.ID] || len(boosted) != 1 {
		t.Fatalf("boosted = %v, want %s (exhausted %s skipped)", boosted, healthy.ID, exhausted.ID)
	}

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	for i := 0; i < 20; i++ {
		got, err := selector.Pick(context.Background(), "claude", claudeTestModel, sessionPickOpts("cl-exh-"+strconv.Itoa(i)), auths)
		if err != nil {
			t.Fatalf("Pick(%d) error = %v", i, err)
		}
		if got.ID == exhausted.ID {
			t.Fatalf("Pick(%d) chose plan-exhausted %s", i, exhausted.ID)
		}
	}
}

func TestClaudeManualResetPriorityTies(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := withClaudeWeeklyReset("mr-cl-tie-a", now.Add(50*time.Hour))
	b := withClaudeWeeklyReset("mr-cl-tie-b", now.Add(50*time.Hour))
	c := withClaudeWeeklyReset("mr-cl-tie-c", now.Add(50*time.Hour))
	forgetAll(t, a, b, c)
	expiry := now.Add(7 * 24 * time.Hour)
	SetClaudeManualResetExpiries(a.ID, []time.Time{expiry}, now)
	SetClaudeManualResetExpiries(b.ID, []time.Time{expiry.Add(30 * time.Minute)}, now)
	SetClaudeManualResetExpiries(c.ID, []time.Time{expiry.Add(2 * time.Hour)}, now)
	auths := []*Auth{a, b, c}
	weights := claudeBaseWeights(auths, now)
	boosted := applyManualResetPriority("claude", auths, weights, now, manualResetTestSettings())
	if !boosted[a.ID] || !boosted[b.ID] || boosted[c.ID] {
		t.Fatalf("boosted = %v, want a and b tied, c outside the 1h tie window", boosted)
	}
}

// Codex results are identical with or without Claude candidates and Claude
// data in the pool, and each provider is compared only with itself.
func TestManualResetPriorityCodexUnchangedByClaudePool(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ca := withCodexResetAt("mr-mix-codex-a", now.Add(50*time.Hour))
	cb := withCodexResetAt("mr-mix-codex-b", now.Add(51*time.Hour))
	la := withClaudeWeeklyReset("mr-mix-claude-a", now.Add(150*time.Hour)) // far from the Codex resets
	lb := withClaudeWeeklyReset("mr-mix-claude-b", now.Add(151*time.Hour))
	forgetAll(t, ca, cb, la, lb)
	SetCodexManualResetExpiries(cb.ID, []time.Time{now.Add(4 * 24 * time.Hour)}, now)
	SetClaudeManualResetExpiries(ca.ID, []time.Time{now.Add(2 * 24 * time.Hour)}, now) // Claude data on a Codex credential is ignored
	SetClaudeManualResetExpiries(la.ID, []time.Time{now.Add(3 * 24 * time.Hour)}, now)

	codexOnly := []*Auth{ca, cb}
	wantWeights := baseWeights(codexOnly, now)
	wantBoost := applyManualResetPriority("codex", codexOnly, wantWeights, now, manualResetTestSettings())
	if len(wantBoost) != 1 || !wantBoost[cb.ID] {
		t.Fatalf("codex-only boosted = %v, want %s", wantBoost, cb.ID)
	}

	mixed := []*Auth{ca, la, cb, lb}
	weights := map[string]int64{}
	for _, a := range mixed {
		weights[a.ID] = planResetRoutingWeight("", a, now)
	}
	boosted := applyManualResetPriority("", mixed, weights, now, manualResetTestSettings())
	if len(boosted) != 2 || !boosted[cb.ID] || !boosted[la.ID] {
		t.Fatalf("mixed boosted = %v, want %s and %s", boosted, cb.ID, la.ID)
	}
	for _, a := range codexOnly {
		if weights[a.ID] != wantWeights[a.ID] {
			t.Fatalf("codex weight %s = %d in mixed pool, want %d", a.ID, weights[a.ID], wantWeights[a.ID])
		}
	}
}

// Not parallel: it captures the global logger.
func TestClaudeManualResetLogLabelsHaveNoEmail(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})

	now := time.Now()
	a := withClaudeWeeklyReset("claude-605ee5a9-someone@example.com.json", now.Add(60*time.Hour))
	a.FileName = "claude-605ee5a9-someone@example.com.json"
	b := withClaudeWeeklyReset("claude-0b1c2d3e-other.person@example.org.json", now.Add(61*time.Hour))
	forgetAll(t, a, b)
	SetClaudeManualResetExpiries(a.ID, []time.Time{now.Add(10 * 24 * time.Hour)}, now)

	if got := ShortAuthLabel(a); got != "claude-605ee5a9" {
		t.Fatalf("label = %q", got)
	}
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	if _, err := selector.Pick(context.Background(), "claude", claudeTestModel, sessionPickOpts("cl-log"), []*Auth{a, b}); err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	// Only the lines written by manual-reset routing are checked; upstream's
	// session-affinity binding log prints the full auth ID on its own.
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "plan-quota routing weights") || strings.Contains(line, "manual reset") {
			lines = append(lines, line)
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "plan-quota routing weights") || !strings.Contains(out, "claude-605ee5a9") || !strings.Contains(out, "claude-0b1c2d3e") {
		t.Fatalf("weight table not logged with short labels: %q", out)
	}
	if !strings.Contains(out, "weight=8*") {
		t.Fatalf("boosted credential not marked: %q", out)
	}
	if strings.Contains(out, "@") || strings.Contains(out, "example.com") || strings.Contains(out, "example.org") {
		t.Fatalf("log leaked an email: %q", out)
	}
}
