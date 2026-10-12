package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func rebindQuotaSelector(t *testing.T) *SessionAffinitySelector {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: SessionAffinityRebindOnQuota,
	})
	t.Cleanup(selector.Stop)
	return selector
}

func transientCodexAuth(id string, signals map[string]string) *Auth {
	auth := quotaAuth(id, "codex", signals)
	auth.Status = StatusError
	auth.Unavailable = true
	auth.NextRetryAfter = time.Now().Add(time.Minute)
	auth.LastError = &Error{HTTPStatus: http.StatusBadGateway, Message: "502 overloaded"}
	return auth
}

func derivedSessionOpts(id string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: id}}
}

// rebind-on=quota: a transient failure of a bound auth that still has plan quota fails over for
// one request but keeps the session binding.
func TestRebindOnQuotaPlanQuotaKeepsBindingOnTransientFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	selector := rebindQuotaSelector(t)
	opts := derivedSessionOpts("keep-binding")

	a := quotaAuth("auth-a", "codex", codexSignals("10", "10080", "false", "false"))
	b := quotaAuth("auth-b", "codex", codexSignals("20", "10080", "false", "false"))
	c := quotaAuth("auth-c", "codex", codexSignals("30", "10080", "false", "false"))
	first, err := selector.Pick(ctx, "codex", "gpt-5", opts, []*Auth{a, b, c})
	if err != nil || first == nil {
		t.Fatalf("first Pick() = %v, %v", first, err)
	}
	bound := first.ID
	others := []*Auth{}
	for _, x := range []*Auth{a, b, c} {
		if x.ID != bound {
			others = append(others, x)
		}
	}
	transient := transientCodexAuth(bound, codexSignals("10", "10080", "false", "false"))
	failover, err := selector.Pick(ctx, "codex", "gpt-5", opts, append([]*Auth{transient}, others...))
	if err != nil || failover == nil || failover.ID == bound {
		t.Fatalf("failover Pick() = %v, %v; want non-%s", failover, err, bound)
	}
	for i := 0; i < 3; i++ {
		back, err := selector.Pick(ctx, "codex", "gpt-5", opts, []*Auth{a, b, c})
		if err != nil || back == nil || back.ID != bound {
			t.Fatalf("recovered Pick() #%d = %v, %v; want preserved binding %s", i, back, err, bound)
		}
	}
}

// rebind-on=quota: if the bound auth is transiently failing AND has exhausted its plan window while
// another auth still has plan quota, the binding is dropped and the session moves to the failover.
func TestRebindOnQuotaDropsBindingWhenBoundAuthPlanExhausted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	selector := rebindQuotaSelector(t)
	opts := derivedSessionOpts("drop-binding")

	a := quotaAuth("auth-a", "codex", codexSignals("10", "10080", "false", "false"))
	b := quotaAuth("auth-b", "codex", codexSignals("20", "10080", "false", "false"))
	c := quotaAuth("auth-c", "codex", codexSignals("30", "10080", "false", "false"))
	first, err := selector.Pick(ctx, "codex", "gpt-5", opts, []*Auth{a, b, c})
	if err != nil || first == nil {
		t.Fatalf("first Pick() = %v, %v", first, err)
	}
	bound := first.ID
	others := []*Auth{}
	for _, x := range []*Auth{a, b, c} {
		if x.ID != bound {
			others = append(others, x)
		}
	}
	exhaustedSignals := codexSignals("100", "10080", "true", "true")
	transient := transientCodexAuth(bound, exhaustedSignals)
	if selector.shouldPreserveBindingWithPlanQuota(ctx, "codex", append([]*Auth{transient}, others...), others, "gpt-5", bound, time.Now()) {
		t.Fatalf("shouldPreserveBindingWithPlanQuota() = true for plan-exhausted bound auth")
	}
	failover, err := selector.Pick(ctx, "codex", "gpt-5", opts, append([]*Auth{transient}, others...))
	if err != nil || failover == nil || failover.ID == bound {
		t.Fatalf("failover Pick() = %v, %v; want non-%s", failover, err, bound)
	}
	recovered := quotaAuth(bound, "codex", exhaustedSignals)
	all := append([]*Auth{recovered}, others...)
	for i := 0; i < 3; i++ {
		next, err := selector.Pick(ctx, "codex", "gpt-5", opts, all)
		if err != nil || next == nil || next.ID != failover.ID {
			t.Fatalf("Pick() #%d = %v, %v; want sticky rebind to %s", i, next, err, failover.ID)
		}
	}
}

// When every auth is plan-exhausted there is no quota auth to move to, so upstream's preserve
// behaviour applies unchanged.
func TestRebindOnQuotaPreservesBindingWhenAllPlansExhausted(t *testing.T) {
	t.Parallel()
	selector := rebindQuotaSelector(t)
	ex := codexSignals("100", "10080", "true", "true")
	bound := transientCodexAuth("auth-a", ex)
	b := quotaAuth("auth-b", "codex", ex)
	if !selector.shouldPreserveBindingWithPlanQuota(context.Background(), "codex", []*Auth{bound, b}, []*Auth{b}, "gpt-5", "auth-a", time.Now()) {
		t.Fatalf("shouldPreserveBindingWithPlanQuota() = false, want true when no plan quota remains anywhere")
	}
}
