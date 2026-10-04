package auth

import (
	"math"
	"strconv"
	"strings"
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
