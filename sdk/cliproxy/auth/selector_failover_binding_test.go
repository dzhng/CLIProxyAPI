package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func failoverSessionOpts(sessionID string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:         http.Header{"X-Session-ID": []string{sessionID}},
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"test"}]}`),
		Metadata:        map[string]any{},
	}
}

func prevalidatedFailoverContext(tried map[string]struct{}) context.Context {
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	ctx = withSessionAffinityFailoverAuthIDs(ctx, tried)
	return ctx
}

func TestSessionAffinity_FailoverPreservesBindingWhenAllFail(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: "quota",
	})
	defer selector.Stop()

	authA := &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}
	authB := &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}
	authC := &Auth{ID: "auth-c", Provider: "codex", Status: StatusActive}

	opts := failoverSessionOpts("failover-all-fail")

	// First pick: should bind to auth-a
	first, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB, authC})
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != "auth-a" {
		t.Fatalf("first Pick() = %s, want auth-a", first.ID)
	}

	// Simulate failover: auth-a was tried and failed, now filtered out.
	// The binding should be preserved to auth-a even though we pick a different auth.
	tried := map[string]struct{}{"auth-a": {}}
	failoverCtx := prevalidatedFailoverContext(tried)

	// Pick with only auth-b and auth-c available (auth-a filtered by tried)
	failover, err := selector.Pick(failoverCtx, "codex", "gpt-5", opts, []*Auth{authB, authC})
	if err != nil {
		t.Fatalf("failover Pick() error = %v", err)
	}
	if failover.ID == "auth-a" {
		t.Fatalf("failover Pick() returned tried auth %s", failover.ID)
	}

	// Now verify the binding was preserved: picking with all auths available should return auth-a
	back, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB, authC})
	if err != nil {
		t.Fatalf("recovery Pick() error = %v", err)
	}
	if back.ID != "auth-a" {
		t.Fatalf("recovery Pick() = %s, want auth-a (original binding preserved)", back.ID)
	}
}

func TestSessionAffinity_FailoverSuccessRebinds(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: "quota",
	})
	defer selector.Stop()

	authA := &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}
	authB := &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}
	authC := &Auth{ID: "auth-c", Provider: "codex", Status: StatusActive}

	opts := failoverSessionOpts("failover-success")

	// First pick: should bind to auth-a
	first, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB, authC})
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != "auth-a" {
		t.Fatalf("first Pick() = %s, want auth-a", first.ID)
	}

	// Simulate failover picking auth-b (auth-a filtered)
	tried := map[string]struct{}{"auth-a": {}}
	failoverCtx := prevalidatedFailoverContext(tried)

	failover, err := selector.Pick(failoverCtx, "codex", "gpt-5", opts, []*Auth{authB, authC})
	if err != nil {
		t.Fatalf("failover Pick() error = %v", err)
	}
	// The failover pick should return auth-b (next in round-robin)
	// but NOT rebind the session yet (binding stays on auth-a)

	// Simulate success: call OnResult with success=true for the failover auth
	// Include the failover auth IDs in metadata so OnResult knows to update binding
	resultOpts := failoverSessionOpts("failover-success")
	resultOpts.Metadata[sessionAffinityFailoverAuthIDsMetadataKey] = tried
	selector.OnResult(Result{
		AuthID:   failover.ID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  true,
		Options:  resultOpts,
	})

	// Now the binding should have moved to the successful auth
	next, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB, authC})
	if err != nil {
		t.Fatalf("next Pick() error = %v", err)
	}
	if next.ID != failover.ID {
		t.Fatalf("after success Pick() = %s, want %s (binding should move to successful auth)", next.ID, failover.ID)
	}

	// Verify subsequent requests stick to the new binding
	for i := 0; i < 3; i++ {
		sticky, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB, authC})
		if err != nil {
			t.Fatalf("sticky Pick() #%d error = %v", i, err)
		}
		if sticky.ID != failover.ID {
			t.Fatalf("sticky Pick() #%d = %s, want %s", i, sticky.ID, failover.ID)
		}
	}
}

func TestSessionAffinity_FailoverMultipleAttemptsPreservesOriginal(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: "quota",
	})
	defer selector.Stop()

	auths := []*Auth{
		{ID: "auth-1", Provider: "codex", Status: StatusActive},
		{ID: "auth-2", Provider: "codex", Status: StatusActive},
		{ID: "auth-3", Provider: "codex", Status: StatusActive},
		{ID: "auth-4", Provider: "codex", Status: StatusActive},
		{ID: "auth-5", Provider: "codex", Status: StatusActive},
	}

	opts := failoverSessionOpts("failover-multiple")

	// First pick: should bind to auth-1
	first, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != "auth-1" {
		t.Fatalf("first Pick() = %s, want auth-1", first.ID)
	}

	// Simulate failover sweep: try auth-1, auth-2, auth-3, auth-4, auth-5 - all fail
	tried := map[string]struct{}{}
	availableAuths := auths
	for i := 0; i < 5; i++ {
		failoverCtx := prevalidatedFailoverContext(tried)
		available := make([]*Auth, 0)
		for _, a := range availableAuths {
			if _, wasTried := tried[a.ID]; !wasTried {
				available = append(available, a)
			}
		}
		if len(available) == 0 {
			break
		}

		pick, err := selector.Pick(failoverCtx, "codex", "gpt-5", opts, available)
		if err != nil {
			t.Fatalf("failover Pick() #%d error = %v", i, err)
		}
		tried[pick.ID] = struct{}{}

		// Simulate failure (don't call OnResult with success)
		selector.OnResult(Result{
			AuthID:   pick.ID,
			Provider: "codex",
			Model:    "gpt-5",
			Success:  false,
			Error:    &Error{Code: globalOverloadErrorCode, Message: "server_is_overloaded"},
			Options:  opts,
		})
	}

	// After all fail, the binding should still be on auth-1 (the original)
	back, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatalf("recovery Pick() error = %v", err)
	}
	if back.ID != "auth-1" {
		t.Fatalf("after all fail Pick() = %s, want auth-1 (original binding preserved)", back.ID)
	}
}

func TestSessionAffinity_NonFailoverRebindsNormally(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: "quota",
	})
	defer selector.Stop()

	authA := &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}
	authB := &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}

	opts := failoverSessionOpts("non-failover")

	// First pick: should bind to auth-a
	first, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB})
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != "auth-a" {
		t.Fatalf("first Pick() = %s, want auth-a", first.ID)
	}

	// Simulate normal selection (not failover): auth-a is unavailable (no tried set)
	// This should rebind to auth-b as normal
	// No tried set in context means this is not a failover scenario
	second, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authB})
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if second.ID != "auth-b" {
		t.Fatalf("second Pick() = %s, want auth-b", second.ID)
	}

	// The binding should have moved to auth-b (normal rebind, not failover)
	next, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB})
	if err != nil {
		t.Fatalf("next Pick() error = %v", err)
	}
	if next.ID != "auth-b" {
		t.Fatalf("next Pick() = %s, want auth-b (should have rebound)", next.ID)
	}
}
