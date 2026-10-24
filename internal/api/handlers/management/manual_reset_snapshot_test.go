package management

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRecordCodexManualResetSnapshotOnlyFromListingGET(t *testing.T) {
	auth := &coreauth.Auth{ID: "mgmt-mr-codex", Provider: "codex"}
	t.Cleanup(func() { coreauth.ForgetCodexManualResets(auth.ID) })
	body := []byte(`{"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"` + time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339) + `"}]}`)

	consume, _ := url.Parse("https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume")
	recordCodexManualResetSnapshot(http.MethodPost, consume, auth, 200, body)
	recordCodexManualResetSnapshot(http.MethodGet, consume, auth, 200, body)
	if _, ok := coreauth.CodexManualResetFetchedAt(auth.ID); ok {
		t.Fatal("consume endpoint was recorded")
	}
	listing, _ := url.Parse(coreauth.CodexManualResetCreditsURL)
	recordCodexManualResetSnapshot(http.MethodGet, listing, auth, 401, body)
	if _, ok := coreauth.CodexManualResetFetchedAt(auth.ID); ok {
		t.Fatal("non-2xx response was recorded")
	}
	recordCodexManualResetSnapshot(http.MethodGet, listing, auth, 200, body)
	if _, ok := coreauth.CodexManualResetFetchedAt(auth.ID); !ok {
		t.Fatal("listing GET was not recorded")
	}
}

func TestRecordClaudeManualResetSnapshotOnlyFromUsageGET(t *testing.T) {
	auth := &coreauth.Auth{ID: "mgmt-mr-claude", Provider: "claude"}
	t.Cleanup(func() { coreauth.ForgetClaudeManualResets(auth.ID) })
	body := []byte(`{"cedar_ember":{"eligible":true,"grants":[{"id":"opus_launch","resets_total":1,"resets_left":1,"ends_at":"` + time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339) + `"}]}}`)
	recorded := func() bool {
		_, ok := coreauth.ClaudeManualResetFetchedAt(auth.ID)
		return ok
	}

	claim, _ := url.Parse("https://api.anthropic.com/api/organizations/11111111-2222-3333-4444-555555555555/reset_rate_limits")
	recordClaudeManualResetSnapshot(http.MethodPost, claim, auth, 200, body)
	recordClaudeManualResetSnapshot(http.MethodGet, claim, auth, 200, body)
	if recorded() {
		t.Fatal("reset_rate_limits claim was recorded")
	}
	usage, _ := url.Parse(coreauth.ClaudeManualResetUsageURL)
	recordClaudeManualResetSnapshot(http.MethodPost, usage, auth, 200, body)
	if recorded() {
		t.Fatal("POST to the usage endpoint was recorded")
	}
	plain, _ := url.Parse("https://api.anthropic.com/api/oauth/usage")
	recordClaudeManualResetSnapshot(http.MethodGet, plain, auth, 200, body)
	if recorded() {
		t.Fatal("usage read without cedar_ember was recorded")
	}
	other, _ := url.Parse("https://evil.example.com/api/oauth/usage?cedar_ember=1")
	recordClaudeManualResetSnapshot(http.MethodGet, other, auth, 200, body)
	if recorded() {
		t.Fatal("non-Anthropic host was recorded")
	}
	recordClaudeManualResetSnapshot(http.MethodGet, usage, auth, 429, body)
	if recorded() {
		t.Fatal("non-2xx response was recorded")
	}
	codexAuth := &coreauth.Auth{ID: "mgmt-mr-claude-codex", Provider: "codex"}
	recordClaudeManualResetSnapshot(http.MethodGet, usage, codexAuth, 200, body)
	recordCodexManualResetSnapshot(http.MethodGet, usage, auth, 200, body)
	if _, ok := coreauth.ClaudeManualResetFetchedAt(codexAuth.ID); ok || recorded() {
		t.Fatal("wrong provider was recorded")
	}
	recordClaudeManualResetSnapshot(http.MethodGet, usage, auth, 200, body)
	if !recorded() {
		t.Fatal("usage GET was not recorded")
	}
}
