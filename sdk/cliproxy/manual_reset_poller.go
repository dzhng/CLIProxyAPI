package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// manualResetPollInterval is how often each Codex credential's banked
	// manual resets are re-read. The management console refreshes the same
	// snapshot whenever it loads quota.
	manualResetPollInterval = 20 * time.Minute
	// manualResetRetryInterval spaces out retries after a failed read.
	manualResetRetryInterval = 5 * time.Minute
	manualResetPollTick      = time.Minute
	manualResetFetchTimeout  = 20 * time.Second
	manualResetStartDelay    = 15 * time.Second
	manualResetMaxBody       = 1 << 20
)

// startCodexManualResetPoller keeps a read-only snapshot of each Codex
// credential's banked manual resets for manual-reset priority routing. It only
// performs GET requests against the listing endpoint and never redeems a reset.
func (s *Service) startCodexManualResetPoller(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	go func() {
		attempts := make(map[string]time.Time)
		lastLogged := make(map[string]string)
		timer := time.NewTimer(manualResetStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.pollCodexManualResets(ctx, attempts, lastLogged)
			timer.Reset(manualResetPollTick)
		}
	}()
}

func (s *Service) pollCodexManualResets(ctx context.Context, attempts map[string]time.Time, lastLogged map[string]string) {
	if !coreauth.ManualResetPriorityEnabled() {
		return
	}
	now := time.Now()
	seen := make(map[string]bool)
	for _, auth := range s.coreManager.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		seen[auth.ID] = true
		if fetchedAt, ok := coreauth.CodexManualResetFetchedAt(auth.ID); ok && now.Sub(fetchedAt) < manualResetPollInterval {
			continue
		}
		if last, ok := attempts[auth.ID]; ok && now.Sub(last) < manualResetRetryInterval {
			continue
		}
		attempts[auth.ID] = now
		expiries, errFetch := s.fetchCodexManualResets(ctx, auth)
		label := coreauth.ShortAuthLabel(auth)
		if errFetch != nil {
			log.Debugf("codex manual resets: fetch failed | auth=%s err=%v", label, errFetch)
			continue
		}
		earliest := "-"
		upcoming := 0
		for _, expiry := range expiries {
			if expiry.After(now) {
				if upcoming == 0 {
					earliest = expiry.Local().Format(time.RFC3339)
				}
				upcoming++
			}
		}
		summary := fmt.Sprintf("available=%d earliest=%s", upcoming, earliest)
		if lastLogged[auth.ID] != summary {
			lastLogged[auth.ID] = summary
			log.Infof("codex manual resets refreshed | auth=%s %s", label, summary)
		}
	}
	for id := range attempts {
		if !seen[id] {
			delete(attempts, id)
			delete(lastLogged, id)
			coreauth.ForgetCodexManualResets(id)
		}
	}
}

func (s *Service) fetchCodexManualResets(ctx context.Context, auth *coreauth.Auth) ([]time.Time, error) {
	token := ""
	accountID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["access_token"].(string); ok {
			token = strings.TrimSpace(v)
		}
		if v, ok := auth.Metadata["account_id"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no access token")
	}
	reqCtx, cancel := context.WithTimeout(ctx, manualResetFetchTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, coreauth.CodexManualResetCreditsURL, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("Originator", "Codex Desktop")
	req.Header.Set("User-Agent", "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	client := helps.NewProxyAwareHTTPClient(reqCtx, cfg, auth, manualResetFetchTimeout)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, manualResetMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return coreauth.RecordCodexManualResetCredits(auth.ID, body, time.Now())
}
