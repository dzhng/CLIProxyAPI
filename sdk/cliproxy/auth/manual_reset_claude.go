package auth

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"time"
)

// Claude reset grants come from the "cedar_ember" block of
// GET https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1, the
// same read the management console makes for the Quota Management page:
//
//	{"cedar_ember": {"eligible": true, "ineligible_reason": null, "at_limit": false,
//	  "weekly_resets_at": "...", "cooldown_until": null, "next_grant_id": "...",
//	  "grants": [{"id": "...", "label": "...", "resets_total": 1, "resets_left": 1,
//	    "starts_at": "...", "ends_at": "2026-10-22T16:00:00Z", "clears": ["five_hour", "seven_day"],
//	    "paused": false, "usable_now": false, "use_requires_limit": true}]}}
//
// Parsing is fail-closed like the console: a missing or malformed block, a
// malformed grant, a duplicate grant id, or an unparsable timestamp rejects the
// whole payload and the previous snapshot is kept. Each remaining reset of a
// grant counts as one banked reset expiring at the grant's ends_at. Grants that
// are paused, used up, or have no ends_at, and every grant of an ineligible
// account, are not counted because they cannot be redeemed before a known date.

var claudeResetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// ParseClaudeResetGrants extracts the expiry of every banked Claude usage-limit
// reset from an /api/oauth/usage?cedar_ember=1 response. The result is sorted.
func ParseClaudeResetGrants(body []byte) ([]time.Time, error) {
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil || root == nil {
		return nil, ErrInvalidManualResetPayload
	}
	rawBlock, ok := root["cedar_ember"]
	if !ok {
		return nil, ErrInvalidManualResetPayload
	}
	var block map[string]any
	if errUnmarshal := json.Unmarshal(rawBlock, &block); errUnmarshal != nil || block == nil {
		return nil, ErrInvalidManualResetPayload
	}
	eligible, ok := block["eligible"].(bool)
	if !ok {
		return nil, ErrInvalidManualResetPayload
	}
	for _, key := range []string{"weekly_resets_at", "cooldown_until"} {
		if _, valid := claudeOptionalTimestamp(block[key]); !valid {
			return nil, ErrInvalidManualResetPayload
		}
	}
	var grants []any
	switch value := block["grants"].(type) {
	case nil:
	case []any:
		grants = value
	default:
		return nil, ErrInvalidManualResetPayload
	}
	expiries := make([]time.Time, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, raw := range grants {
		grant, okGrant := raw.(map[string]any)
		if !okGrant {
			return nil, ErrInvalidManualResetPayload
		}
		id, okID := grant["id"].(string)
		if !okID || !claudeResetGrantIDPattern.MatchString(id) || seen[id] {
			return nil, ErrInvalidManualResetPayload
		}
		seen[id] = true
		total, okTotal := claudeCount(grant["resets_total"])
		left, okLeft := claudeCount(grant["resets_left"])
		if !okTotal || !okLeft || left > total {
			return nil, ErrInvalidManualResetPayload
		}
		if _, valid := claudeOptionalTimestamp(grant["starts_at"]); !valid {
			return nil, ErrInvalidManualResetPayload
		}
		endsAt, valid := claudeOptionalTimestamp(grant["ends_at"])
		if !valid {
			return nil, ErrInvalidManualResetPayload
		}
		paused := false
		switch value := grant["paused"].(type) {
		case nil:
		case bool:
			paused = value
		default:
			return nil, ErrInvalidManualResetPayload
		}
		if !eligible || paused || left == 0 || endsAt.IsZero() {
			continue
		}
		if left > maxClaudeGrantResets {
			left = maxClaudeGrantResets
		}
		for i := int64(0); i < left; i++ {
			expiries = append(expiries, endsAt)
		}
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })
	return expiries, nil
}

func claudeCount(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number != math.Trunc(number) || number > 1e9 {
		return 0, false
	}
	return int64(number), true
}

// claudeOptionalTimestamp returns the zero time for an absent or null value and
// reports false for anything that is not an RFC 3339 timestamp.
func claudeOptionalTimestamp(value any) (time.Time, bool) {
	switch v := value.(type) {
	case nil:
		return time.Time{}, true
	case string:
		parsed, errParse := time.Parse(time.RFC3339Nano, v)
		if errParse != nil {
			return time.Time{}, false
		}
		return parsed, true
	default:
		return time.Time{}, false
	}
}

// RecordClaudeResetGrants parses a Claude usage response and stores its banked
// resets for authID. An invalid payload leaves the previous snapshot untouched.
func RecordClaudeResetGrants(authID string, body []byte, now time.Time) ([]time.Time, error) {
	expiries, errParse := ParseClaudeResetGrants(body)
	if errParse != nil {
		return nil, errParse
	}
	SetClaudeManualResetExpiries(authID, expiries, now)
	return expiries, nil
}

// SetClaudeManualResetExpiries stores the banked Claude reset expiries for a
// credential. An empty list records "no banked resets".
func SetClaudeManualResetExpiries(authID string, expiries []time.Time, now time.Time) {
	setManualResetExpiries(manualResetKindClaude, authID, expiries, now)
}

// ClaudeManualResetFetchedAt reports when the Claude snapshot for authID was stored.
func ClaudeManualResetFetchedAt(authID string) (time.Time, bool) {
	return manualResetFetchedAt(manualResetKindClaude, authID)
}

// ForgetClaudeManualResets drops the stored snapshot for a credential.
func ForgetClaudeManualResets(authID string) {
	ForgetCodexManualResets(authID)
}

// claudeEarliestManualReset returns the earliest unexpired banked Claude reset
// and the count of unexpired resets. A missing or stale snapshot reports false.
func claudeEarliestManualReset(authID string, now time.Time) (time.Time, int, bool) {
	return earliestManualReset(manualResetKindClaude, authID, now)
}
