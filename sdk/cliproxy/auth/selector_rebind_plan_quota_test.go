package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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

// Manager path (prevalidated candidates): a bound Codex login that is transiently failing AND
// plan-exhausted must lose its binding when other logins still have plan quota, so the session
// sticks to one replacement instead of hopping between quota logins on every request.
func TestManagerRebindOnQuotaDropsPlanExhaustedBindingOnTransientFailure(t *testing.T) {
	ctx := context.Background()
	provider := "codex"
	model := "rebind-plan-quota-manager-model"
	ids := []string{"rpq-mgr-a", "rpq-mgr-b", "rpq-mgr-c"}

	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
		RebindOn: SessionAffinityRebindOnQuota,
	})
	defer affinity.Stop()
	manager.SetSelector(affinity)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for i, id := range ids {
		auth := quotaAuth(id, provider, codexSignals("10", "10080", "false", "false"))
		auth.Status = StatusActive
		auth.Attributes = map[string]string{"priority": map[bool]string{true: "1", false: "0"}[i == 0]}
		if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		idCopy := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(idCopy) })
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "rpq-session"}}
	pick := func() *Auth {
		t.Helper()
		auth, _, err := manager.pickNext(ctx, provider, model, opts, nil)
		if err != nil || auth == nil {
			t.Fatalf("pick: %v %v", auth, err)
		}
		return auth
	}
	if got := pick(); got.ID != ids[0] {
		t.Fatalf("cold pick = %s, want %s", got.ID, ids[0])
	}
	manager.MarkResult(ctx, Result{AuthID: ids[0], Provider: provider, Model: model, Success: true, Options: opts})

	// Bound login runs out of plan quota (credits only) and then fails transiently.
	exhausted, _ := manager.GetByID(ids[0])
	exhausted = exhausted.Clone()
	exhausted.Quota.Signals = codexSignals("100", "10080", "true", "true")
	if _, err := manager.Update(WithSkipPersist(ctx), exhausted); err != nil {
		t.Fatalf("Update: %v", err)
	}
	manager.MarkResult(ctx, Result{AuthID: ids[0], Provider: provider, Model: model, Success: false,
		Error: &Error{HTTPStatus: http.StatusBadGateway, Message: "502 overloaded"}, Options: opts})

	first := pick()
	if first.ID == ids[0] {
		t.Fatalf("failover pick = bound exhausted login")
	}
	manager.MarkResult(ctx, Result{AuthID: first.ID, Provider: provider, Model: model, Success: true, Options: opts})
	expireSessionAffinityPriorityModelCooldown(t, manager, ids[0], model)
	for i := 0; i < 4; i++ {
		got := pick()
		if got.ID != first.ID {
			t.Fatalf("pick #%d = %s, want session to stay on replacement %s", i, got.ID, first.ID)
		}
		manager.MarkResult(ctx, Result{AuthID: got.ID, Provider: provider, Model: model, Success: true, Options: opts})
	}
}
