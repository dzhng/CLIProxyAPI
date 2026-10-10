package auth

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const codexWeeklyPlanWindowMinutes = 10080

// planQuotaRoutingAuths keeps credentials whose stored included-plan window is
// not exhausted. When every candidate is plan-exhausted the full set is
// returned, so a credential that would bill user credits or extra usage is
// selected only as a last resort. A credential with no usage snapshot is not
// plan-exhausted and stays in the preferred set.
func planQuotaRoutingAuths(provider string, auths []*Auth) []*Auth {
	if len(auths) == 0 {
		return auths
	}
	exhausted := 0
	for _, candidate := range auths {
		if candidate != nil && authPlanWindowExhausted(provider, candidate) {
			exhausted++
		}
	}
	if exhausted == 0 {
		return auths
	}
	preferred := make([]*Auth, 0, len(auths)-exhausted)
	for _, candidate := range auths {
		if candidate == nil || authPlanWindowExhausted(provider, candidate) {
			continue
		}
		preferred = append(preferred, candidate)
	}
	if len(preferred) == 0 {
		return auths
	}
	return preferred
}

func authIDSet(auths []*Auth) map[string]bool {
	set := make(map[string]bool, len(auths))
	for _, candidate := range auths {
		if candidate == nil || candidate.ID == "" {
			continue
		}
		set[candidate.ID] = true
	}
	return set
}

func authPlanWindowExhausted(requestProvider string, auth *Auth) bool {
	if auth == nil {
		return false
	}
	switch planQuotaProviderKind(requestProvider, auth) {
	case "codex":
		return codexPlanWindowExhausted(auth.Quota.Signals)
	case "claude":
		return claudePlanWindowExhausted(auth.Quota.Signals)
	default:
		return false
	}
}

func planQuotaProviderKind(requestProvider string, auth *Auth) string {
	provider := ""
	if auth != nil {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	}
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(requestProvider))
	}
	switch provider {
	case "codex", "openai":
		return "codex"
	case "claude", "anthropic":
		return "claude"
	default:
		return ""
	}
}

// codexPlanWindowExhausted reports whether the stored Codex snapshot shows the
// included weekly plan is used up. Limit-Reached, or a primary weekly used
// percent of at least 100, means the plan is exhausted even when the account
// still has credits. A non-weekly primary window does not, by itself, mean the
// included plan is exhausted. Missing plan fields are not exhaustion.
func codexPlanWindowExhausted(signals map[string]string) bool {
	if len(signals) == 0 {
		return false
	}
	limitReached := quotaSignal(signals, "X-Codex-Limit-Reached")
	usedRaw := quotaSignal(signals, "X-Codex-Primary-Used-Percent")
	if limitReached == "" && usedRaw == "" {
		return false
	}
	if codexTruthy(limitReached) {
		return true
	}
	if usedRaw == "" {
		return false
	}
	used, errParse := strconv.ParseFloat(usedRaw, 64)
	if errParse != nil || math.IsNaN(used) || math.IsInf(used, 0) {
		return false
	}
	windowRaw := quotaSignal(signals, "X-Codex-Primary-Window-Minutes")
	if windowRaw != "" {
		window, errWindow := strconv.ParseFloat(windowRaw, 64)
		if errWindow == nil && window > 0 && window != codexWeeklyPlanWindowMinutes {
			return false
		}
	}
	return used >= 100
}

func codexTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// claudePlanWindowExhausted reports whether the stored Anthropic unified
// snapshot shows the included 5h or 7d plan is used up. A rejected window or
// utilization of at least 1 exhausts the plan. Overage status "allowed" (or an
// in-use overage) also means the included plan is exhausted and further usage
// bills extra usage. An overage-only rejection with healthy plan windows is
// not exhaustion. Missing plan fields are not exhaustion.
func claudePlanWindowExhausted(signals map[string]string) bool {
	status5h := strings.ToLower(quotaSignal(signals, "Anthropic-Ratelimit-Unified-5h-Status"))
	status7d := strings.ToLower(quotaSignal(signals, "Anthropic-Ratelimit-Unified-7d-Status"))
	util5h := quotaSignal(signals, "Anthropic-Ratelimit-Unified-5h-Utilization")
	util7d := quotaSignal(signals, "Anthropic-Ratelimit-Unified-7d-Utilization")
	overage := quotaSignal(signals, "Anthropic-Ratelimit-Unified-Overage-Status")
	if status5h == "" && status7d == "" && util5h == "" && util7d == "" && overage == "" {
		return false
	}
	if status5h == "rejected" || status7d == "rejected" {
		return true
	}
	if claudeUtilizationExhausted(util5h) || claudeUtilizationExhausted(util7d) {
		return true
	}
	return claudeOverageSpendsIncludedPlan(overage)
}

func claudeUtilizationExhausted(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	utilization, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) {
		return false
	}
	return utilization >= 1
}

func claudeOverageSpendsIncludedPlan(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "", "rejected", "disabled":
		return false
	}
	if status == "allowed" || strings.HasPrefix(status, "allowed") {
		return true
	}
	return strings.Contains(status, "in_use") || strings.Contains(status, "in-use") || strings.Contains(status, "in use")
}

func quotaSignal(signals map[string]string, name string) string {
	if len(signals) == 0 {
		return ""
	}
	if value, ok := signals[name]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

const (
	// planResetWeightFloor keeps a later-reset credential in the rotation.
	planResetWeightFloor int64 = 1
	// planResetWeightScale is one day in seconds. See planResetRoutingWeight.
	planResetWeightScale int64 = 24 * 60 * 60
)

// planResetRoutingWeight is the smooth weighted-round-robin weight for a
// credential that still has plan quota.
//
//	weight = 1 + 86400 / max(secondsUntilReset, 1)
//
// A sooner reset produces a higher weight. The leading 1 is a floor so a
// later reset still receives some new sessions. A missing reset uses that
// floor alone and is not given an invented deadline, so it never outranks a
// known reset. A known reset further than 86400 seconds away ties the floor.
func planResetRoutingWeight(provider string, auth *Auth, now time.Time) int64 {
	deadline, ok := planResetDeadline(provider, auth, now)
	if !ok {
		return planResetWeightFloor
	}
	seconds := int64(deadline.Sub(now) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return planResetWeightFloor + planResetWeightScale/seconds
}

// planResetDeadline is the relevant included-plan reset: Codex primary, or the
// sooner of the Claude 5h and 7d resets. A missing field is not a deadline.
func planResetDeadline(provider string, auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	switch planQuotaProviderKind(provider, auth) {
	case "codex":
		return codexPrimaryReset(auth, now)
	case "claude":
		return claudeSoonerPlanReset(auth)
	default:
		return time.Time{}, false
	}
}

func codexPrimaryReset(auth *Auth, now time.Time) (time.Time, bool) {
	signals := auth.Quota.Signals
	if raw := quotaSignal(signals, "X-Codex-Primary-Reset-At"); raw != "" {
		if deadline, ok := parseResetTimestamp(raw); ok {
			return deadline, true
		}
	}
	raw := quotaSignal(signals, "X-Codex-Primary-Reset-After-Seconds")
	if raw == "" {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return time.Time{}, false
	}
	base := auth.Quota.ObservedAt
	if base.IsZero() {
		base = now
	}
	return base.Add(time.Duration(seconds * float64(time.Second))), true
}

func claudeSoonerPlanReset(auth *Auth) (time.Time, bool) {
	var sooner time.Time
	found := false
	for _, name := range []string{
		"Anthropic-Ratelimit-Unified-5h-Reset",
		"Anthropic-Ratelimit-Unified-7d-Reset",
	} {
		raw := quotaSignal(auth.Quota.Signals, name)
		if raw == "" {
			continue
		}
		deadline, ok := parseResetTimestamp(raw)
		if !ok {
			continue
		}
		if !found || deadline.Before(sooner) {
			sooner = deadline
			found = true
		}
	}
	return sooner, found
}

func parseResetTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil && seconds > 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		whole := int64(seconds)
		if whole > 0 {
			fraction := int64((seconds - float64(whole)) * float64(time.Second))
			return time.Unix(whole, fraction), true
		}
	}
	if deadline, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return deadline, true
	}
	return time.Time{}, false
}

// planResetRoutingActive reports whether new sessions in this set use reset
// weighting. It stays off for providers without a plan window, and when every
// Codex or Claude credential is already plan-exhausted.
func planResetRoutingActive(provider string, auths []*Auth) bool {
	if len(auths) == 0 || planQuotaRoutingFellBack(provider, auths) {
		return false
	}
	saw := false
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		saw = true
		if planQuotaProviderKind(provider, candidate) == "" {
			return false
		}
	}
	return saw
}

// planQuotaRoutingFellBack reports that every credential is plan-exhausted, so
// the caller must keep the ordinary selector instead of reset weighting.
func planQuotaRoutingFellBack(provider string, auths []*Auth) bool {
	saw := false
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		saw = true
		if !authPlanWindowExhausted(provider, candidate) {
			return false
		}
	}
	return saw
}
