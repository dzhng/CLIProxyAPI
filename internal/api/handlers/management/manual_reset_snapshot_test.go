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
