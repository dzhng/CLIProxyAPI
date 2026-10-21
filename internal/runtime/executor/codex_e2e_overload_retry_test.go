package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Production error format from user's logs:
// {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY"}},"sequence_number":3}
const productionOverloadError = `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY"}},"sequence_number":3}`

func createProductionScenarioServer(events ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			eventType := "message"
			if parsed := strings.SplitN(event, `"type":"`, 2); len(parsed) == 2 {
				eventType = strings.SplitN(parsed[1], `"`, 2)[0]
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

// TestCodexExecutor_ProductionOverloadFormat tests the exact error format seen in production:
// HTTP 200 + response.created + response.in_progress + error(server_is_overloaded)
// With bootstrap buffering enabled, this should return a synchronous 503 error.
func TestCodexExecutor_ProductionOverloadFormat_BootstrapBufferingCatches(t *testing.T) {
	server := createProductionScenarioServer(
		codexCreatedEvent,
		codexInProgressEvent,
		productionOverloadError,
	)
	defer server.Close()

	cfg := &config.Config{
		Codex: config.CodexConfig{
			StreamBootstrapBuffering: true,
			StreamBootstrapTimeout:   "20s",
		},
	}

	codexExecutor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-terra",
		Payload: []byte(`{"model":"gpt-5.6-terra","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	}

	result, err := codexExecutor.ExecuteStream(context.Background(), auth, req, opts)

	if err == nil {
		// Bootstrap buffering should have caught the error and returned it synchronously
		t.Log("WARNING: Executor returned stream result instead of synchronous error")
		if result != nil && result.Chunks != nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					statusCode := 0
					if sc, ok := chunk.Err.(interface{ StatusCode() int }); ok {
						statusCode = sc.StatusCode()
					}
					t.Fatalf("Error delivered in-stream (status=%d) instead of synchronously - bootstrap buffering is not working: %v", statusCode, chunk.Err)
				}
			}
		}
		t.Fatal("Expected a synchronous error from bootstrap buffering, got success stream")
	}

	statusCode := 0
	if sc, ok := err.(interface{ StatusCode() int }); ok {
		statusCode = sc.StatusCode()
	}

	// Bootstrap buffering should convert the in-stream overload error to 503
	if statusCode != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 status from bootstrap buffering, got %d: %v", statusCode, err)
	}

	t.Logf("SUCCESS: Bootstrap buffering caught the production overload error and returned 503")
}

// TestCodexExecutor_ProductionOverloadFormat_DisabledBuffering verifies that WITHOUT bootstrap
// buffering, the error is delivered in-stream (the problem scenario).
func TestCodexExecutor_ProductionOverloadFormat_DisabledBuffering(t *testing.T) {
	server := createProductionScenarioServer(
		codexCreatedEvent,
		codexInProgressEvent,
		productionOverloadError,
	)
	defer server.Close()

	cfg := &config.Config{
		Codex: config.CodexConfig{
			StreamBootstrapBuffering: false, // Disabled
		},
	}

	codexExecutor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-terra",
		Payload: []byte(`{"model":"gpt-5.6-terra","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	}

	result, err := codexExecutor.ExecuteStream(context.Background(), auth, req, opts)

	if err != nil {
		t.Fatalf("Without buffering, executor should return a stream, not error: %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatal("Expected a stream result")
	}

	var sawError bool
	var errorStatus int
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			sawError = true
			if sc, ok := chunk.Err.(interface{ StatusCode() int }); ok {
				errorStatus = sc.StatusCode()
			}
		}
	}

	if !sawError {
		t.Fatal("Expected error to be delivered in-stream when buffering is disabled")
	}

	// Without buffering, the error comes through as 502 (from codexTerminalFailureStatus)
	if errorStatus != http.StatusBadGateway {
		t.Logf("Note: in-stream error has status %d (expected 502)", errorStatus)
	}

	t.Logf("Confirmed: Without buffering, error is delivered in-stream with status %d", errorStatus)
}

// TestCodexExecutor_ProductionOverloadFormat_WithResponseMetadata reproduces the live wire capture:
// created(0), in_progress(1), response.metadata(2), error server_is_overloaded(3). The metadata
// frame must not release the bootstrap buffer, so the overload surfaces as a synchronous 503.
func TestCodexExecutor_ProductionOverloadFormat_WithResponseMetadata(t *testing.T) {
	server := createProductionScenarioServer(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-terra"},"sequence_number":0}`,
		`{"type":"response.in_progress","response":{"id":"resp_1"},"sequence_number":1}`,
		`{"type":"response.metadata","response_id":"resp_1","sequence_number":2,"metadata":["openai_chatgpt_moderation_metadata"]}`,
		productionOverloadError,
	)
	defer server.Close()

	cfg := &config.Config{Codex: config.CodexConfig{StreamBootstrapBuffering: true, StreamBootstrapTimeout: "20s"}}
	codexExecutor := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test-key"}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: []byte(`{"model":"gpt-5.6-terra","input":"hello"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true}

	result, err := codexExecutor.ExecuteStream(context.Background(), auth, req, opts)
	if err == nil {
		if result != nil && result.Chunks != nil {
			for range result.Chunks {
			}
		}
		t.Fatal("expected a synchronous 503 overload error; response.metadata released the stream")
	}
	statusCode := 0
	if sc, ok := err.(interface{ StatusCode() int }); ok {
		statusCode = sc.StatusCode()
	}
	if statusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %v", statusCode, err)
	}
}
