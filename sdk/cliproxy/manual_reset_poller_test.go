package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type recordedManualResetRequest struct {
	method, path, query string
	header              http.Header
}

// The Claude poller only ever sends the console's read-only GET and never
// calls the reset_rate_limits claim (or any consume path).
func TestClaudeManualResetPollerOnlyReadsUsage(t *testing.T) {
	var mu sync.Mutex
	var requests []recordedManualResetRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, recordedManualResetRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/usage" {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{"result":"reset"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"seven_day":{"utilization":28},"cedar_ember":{"eligible":true,"at_limit":false,"next_grant_id":"opus_launch","grants":[{"id":"opus_launch","label":"Claude Opus 5.5 launch","resets_total":1,"resets_left":1,"ends_at":"` + time.Now().Add(10*24*time.Hour).UTC().Format(time.RFC3339) + `","clears":["five_hour","seven_day"],"paused":false,"usable_now":false,"use_requires_limit":true}]}}`))
	}))
	defer srv.Close()

	prevURL := claudeManualResetUsageURL
	claudeManualResetUsageURL = srv.URL + "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	t.Cleanup(func() { claudeManualResetUsageURL = prevURL })

	manager := coreauth.NewManager(nil, nil, nil)
	claude := &coreauth.Auth{ID: "claude-poller-test.json", Provider: "claude", Metadata: map[string]any{"access_token": "test-token"}}
	disabled := &coreauth.Auth{ID: "claude-poller-disabled.json", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "other"}}
	for _, a := range []*coreauth.Auth{claude, disabled} {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("Register(%s) error = %v", a.ID, err)
		}
	}
	t.Cleanup(func() {
		coreauth.ForgetClaudeManualResets(claude.ID)
		coreauth.ForgetClaudeManualResets(disabled.ID)
	})
	svc := &Service{coreManager: manager}
	attempts := map[string]time.Time{}
	logged := map[string]string{}
	svc.pollManualResets(context.Background(), attempts, logged)
	svc.pollManualResets(context.Background(), attempts, logged) // fresh snapshot: no second read

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("requests = %d (%+v), want exactly one read", len(requests), requests)
	}
	for _, req := range requests {
		if req.method != http.MethodGet {
			t.Fatalf("method = %s, want GET", req.method)
		}
		if strings.Contains(req.path, "reset_rate_limits") || strings.Contains(req.path, "consume") || strings.Contains(req.path, "/organizations/") {
			t.Fatalf("claim path called: %s", req.path)
		}
	}
	req := requests[0]
	if req.path != "/api/oauth/usage" || req.query != "cedar_ember=1&skip_spend=1" {
		t.Fatalf("read = %s?%s", req.path, req.query)
	}
	if got := req.header.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.header.Get("User-Agent"); got != "claude-cli/2.1.280 (external, cli)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q", got)
	}
	if got := req.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if _, ok := coreauth.ClaudeManualResetFetchedAt(claude.ID); !ok {
		t.Fatal("Claude snapshot was not stored")
	}
	if _, ok := coreauth.CodexManualResetFetchedAt(claude.ID); ok {
		t.Fatal("Claude snapshot stored as Codex")
	}
	if _, ok := coreauth.ClaudeManualResetFetchedAt(disabled.ID); ok {
		t.Fatal("disabled credential was polled")
	}
}

// A failed read is retried after the retry interval and records nothing.
func TestClaudeManualResetPollerFailureRecordsNothing(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()
	prevURL := claudeManualResetUsageURL
	claudeManualResetUsageURL = srv.URL + "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	t.Cleanup(func() { claudeManualResetUsageURL = prevURL })

	manager := coreauth.NewManager(nil, nil, nil)
	claude := &coreauth.Auth{ID: "claude-poller-fail.json", Provider: "claude", Metadata: map[string]any{"access_token": "t"}}
	if _, err := manager.Register(context.Background(), claude); err != nil {
		t.Fatalf("Register error = %v", err)
	}
	t.Cleanup(func() { coreauth.ForgetClaudeManualResets(claude.ID) })
	svc := &Service{coreManager: manager}
	attempts := map[string]time.Time{}
	svc.pollManualResets(context.Background(), attempts, map[string]string{})
	svc.pollManualResets(context.Background(), attempts, map[string]string{})
	if _, ok := coreauth.ClaudeManualResetFetchedAt(claude.ID); ok {
		t.Fatal("failed read stored a snapshot")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (retry waits %s)", calls, manualResetRetryInterval)
	}
}
