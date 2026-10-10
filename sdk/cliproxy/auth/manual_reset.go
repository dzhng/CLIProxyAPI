package auth

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Codex Pro accounts can bank "manual resets" (rate-limit reset credits). Each
// one can be redeemed once to reset the weekly limit and expires on its own
// date if unused. The proxy only ever reads the list of banked resets; it never
// redeems one.
//
// Manual-reset priority: when the weekly plan windows of the candidate Codex
// credentials all reset within ManualResetPrioritySettings.Band of each other,
// the plan-reset weight of the credential whose earliest unexpired manual reset
// expires first is multiplied by Boost. That steers most new sessions onto the
// account whose banked reset would otherwise be wasted, while the others keep
// receiving a share. Missing data, expired resets, plan-exhausted credentials,
// and non-Codex credentials are never boosted, so without data the routing is
// exactly the plan-reset weighting.

const (
	// DefaultManualResetBand is how close the weekly resets must be to count
	// as "about the same".
	DefaultManualResetBand = 24 * time.Hour
	// DefaultManualResetBoost multiplies the weight of the soonest-expiring
	// manual reset. With five otherwise equal credentials, 8 gives that
	// credential 8/12 (about 67%) of new sessions.
	DefaultManualResetBoost int64 = 8
	// maxManualResetBoost bounds the configured multiplier.
	maxManualResetBoost int64 = 1000
	// manualResetDataTTL drops a snapshot that has not been refreshed, so a
	// broken fetch falls back to plain plan-reset weighting.
	manualResetDataTTL = 3 * time.Hour
	// manualResetTieWindow treats expiries this close to the earliest one as
	// a tie; every tied credential is boosted.
	manualResetTieWindow = time.Hour

	// CodexManualResetCreditsURL is the read-only endpoint listing banked
	// manual resets. The ".../consume" sibling redeems one and is never called.
	CodexManualResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

// ManualResetPrioritySettings configures manual-reset priority routing.
type ManualResetPrioritySettings struct {
	Enabled bool
	Band    time.Duration
	Boost   int64
}

// DefaultManualResetPrioritySettings is the behaviour when routing config
// leaves the keys unset.
func DefaultManualResetPrioritySettings() ManualResetPrioritySettings {
	return ManualResetPrioritySettings{Enabled: true, Band: DefaultManualResetBand, Boost: DefaultManualResetBoost}
}

var manualResetSettings atomic.Pointer[ManualResetPrioritySettings]

// ConfigureManualResetPriority installs routing settings. A non-positive band
// or a boost below 2 falls back to the defaults.
func ConfigureManualResetPriority(settings ManualResetPrioritySettings) {
	if settings.Band <= 0 {
		settings.Band = DefaultManualResetBand
	}
	if settings.Boost < 2 {
		settings.Boost = DefaultManualResetBoost
	}
	if settings.Boost > maxManualResetBoost {
		settings.Boost = maxManualResetBoost
	}
	manualResetSettings.Store(&settings)
}

func currentManualResetSettings() ManualResetPrioritySettings {
	if settings := manualResetSettings.Load(); settings != nil {
		return *settings
	}
	return DefaultManualResetPrioritySettings()
}

type manualResetRecord struct {
	expiries  []time.Time
	fetchedAt time.Time
}

var manualResetStore = struct {
	sync.RWMutex
	byAuth map[string]manualResetRecord
}{byAuth: make(map[string]manualResetRecord)}

// ErrInvalidManualResetPayload means the body was not a reset-credits listing.
var ErrInvalidManualResetPayload = errors.New("invalid manual reset credits payload")

// ParseCodexManualResetCredits extracts the expiry of every available Codex
// manual reset from a rate-limit-reset-credits response. The result is sorted.
// It mirrors the management console: only reset_type "codex_rate_limits" with
// status "available" counts.
func ParseCodexManualResetCredits(body []byte) ([]time.Time, error) {
	var root map[string]any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil || root == nil {
		return nil, ErrInvalidManualResetPayload
	}
	known := false
	for _, key := range []string{"credits", "available_count", "availableCount", "applicable_available_count", "applicableAvailableCount"} {
		if _, ok := root[key]; ok {
			known = true
			break
		}
	}
	if !known {
		return nil, ErrInvalidManualResetPayload
	}
	credits, _ := root["credits"].([]any)
	expiries := make([]time.Time, 0, len(credits))
	for _, raw := range credits {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if manualResetString(entry, "reset_type", "resetType") != "codex_rate_limits" {
			continue
		}
		if manualResetString(entry, "status") != "available" {
			continue
		}
		expiry, ok := parseManualResetTime(manualResetString(entry, "expires_at", "expiresAt"))
		if !ok {
			continue
		}
		expiries = append(expiries, expiry)
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })
	return expiries, nil
}

func manualResetString(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := entry[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case float64:
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				return strconv.FormatFloat(value, 'f', -1, 64)
			}
		}
	}
	return ""
}

func parseManualResetTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed, true
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil && seconds > 0 && !math.IsInf(seconds, 0) {
		if seconds > 1e11 {
			seconds /= 1000
		}
		return time.Unix(int64(seconds), 0), true
	}
	return time.Time{}, false
}

// RecordCodexManualResetCredits parses a reset-credits response and stores
// it for authID. An invalid payload leaves the previous snapshot untouched.
func RecordCodexManualResetCredits(authID string, body []byte, now time.Time) ([]time.Time, error) {
	expiries, errParse := ParseCodexManualResetCredits(body)
	if errParse != nil {
		return nil, errParse
	}
	SetCodexManualResetExpiries(authID, expiries, now)
	return expiries, nil
}

// SetCodexManualResetExpiries stores the banked manual-reset expiries for a
// credential. An empty list records "no banked resets".
func SetCodexManualResetExpiries(authID string, expiries []time.Time, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	copied := append([]time.Time(nil), expiries...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].Before(copied[j]) })
	manualResetStore.Lock()
	manualResetStore.byAuth[authID] = manualResetRecord{expiries: copied, fetchedAt: now}
	manualResetStore.Unlock()
}

// ForgetCodexManualResets drops the stored snapshot for a credential.
func ForgetCodexManualResets(authID string) {
	manualResetStore.Lock()
	delete(manualResetStore.byAuth, strings.TrimSpace(authID))
	manualResetStore.Unlock()
}

// CodexManualResetFetchedAt reports when the snapshot for authID was stored.
func CodexManualResetFetchedAt(authID string) (time.Time, bool) {
	manualResetStore.RLock()
	record, ok := manualResetStore.byAuth[strings.TrimSpace(authID)]
	manualResetStore.RUnlock()
	return record.fetchedAt, ok
}

// codexEarliestManualReset returns the earliest unexpired banked reset and the
// count of unexpired resets. A missing or stale snapshot reports false.
func codexEarliestManualReset(authID string, now time.Time) (time.Time, int, bool) {
	manualResetStore.RLock()
	record, ok := manualResetStore.byAuth[strings.TrimSpace(authID)]
	manualResetStore.RUnlock()
	if !ok || now.Sub(record.fetchedAt) > manualResetDataTTL {
		return time.Time{}, 0, false
	}
	var earliest time.Time
	count := 0
	for _, expiry := range record.expiries {
		if !expiry.After(now) {
			continue
		}
		if count == 0 {
			earliest = expiry
		}
		count++
	}
	return earliest, count, count > 0
}

// applyManualResetPriority multiplies, in place, the weight of the Codex
// credential(s) with the soonest-expiring banked manual reset when the weekly
// resets of the candidates are within the configured band. It reports which
// credentials were boosted.
func applyManualResetPriority(provider string, auths []*Auth, weights map[string]int64, now time.Time, settings ManualResetPrioritySettings) map[string]bool {
	if !settings.Enabled || settings.Boost < 2 || len(auths) == 0 || len(weights) == 0 {
		return nil
	}
	var minReset, maxReset time.Time
	haveReset := false
	codexCandidates := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || candidate.ID == "" || planQuotaProviderKind(provider, candidate) != "codex" {
			continue
		}
		codexCandidates = append(codexCandidates, candidate)
		reset, ok := codexPrimaryReset(candidate, now)
		if !ok {
			continue
		}
		if !haveReset || reset.Before(minReset) {
			minReset = reset
		}
		if !haveReset || reset.After(maxReset) {
			maxReset = reset
		}
		haveReset = true
	}
	if len(codexCandidates) == 0 {
		return nil
	}
	if haveReset && maxReset.Sub(minReset) > settings.Band {
		return nil
	}
	earliestByID := make(map[string]time.Time, len(codexCandidates))
	var earliest time.Time
	for _, candidate := range codexCandidates {
		if authPlanWindowExhausted(provider, candidate) {
			continue
		}
		if _, ok := weights[candidate.ID]; !ok {
			continue
		}
		expiry, _, ok := codexEarliestManualReset(candidate.ID, now)
		if !ok {
			continue
		}
		earliestByID[candidate.ID] = expiry
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}
	if len(earliestByID) == 0 {
		return nil
	}
	boosted := make(map[string]bool, 1)
	for id, expiry := range earliestByID {
		if expiry.Sub(earliest) > manualResetTieWindow {
			continue
		}
		weights[id] = saturatingMulInt64(weights[id], settings.Boost)
		boosted[id] = true
	}
	return boosted
}

func saturatingMulInt64(value, factor int64) int64 {
	if value <= 0 || factor <= 0 {
		return value
	}
	if value > math.MaxInt64/factor {
		return math.MaxInt64
	}
	return value * factor
}

var shortAuthLabelPattern = regexp.MustCompile(`^([a-z]+-[0-9a-f]{8})`)

// ShortAuthLabel returns a log-safe credential label such as
// "codex-3f7c380d". It never returns an email or token.
func ShortAuthLabel(auth *Auth) string {
	if auth == nil {
		return "-"
	}
	for _, raw := range []string{auth.FileName, auth.ID} {
		base := strings.ToLower(filepath.Base(strings.TrimSpace(raw)))
		if match := shortAuthLabelPattern.FindStringSubmatch(base); len(match) == 2 {
			return match[1]
		}
	}
	sum := sha256.Sum256([]byte(auth.ID))
	return fmt.Sprintf("auth#%x", sum[:4])
}

// ManualResetPriorityEnabled reports whether manual-reset priority is on.
func ManualResetPriorityEnabled() bool {
	return currentManualResetSettings().Enabled
}
