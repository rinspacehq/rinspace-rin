package rin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const agentToolResponse = `{"id":"agent-request-1","object":"chat.completion","created":1,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_project_files","arguments":"{\"path\":\"main.tex\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`

func TestOpenAIProviderCarriesAgentToolsWithoutExecutingThem(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" ||
			r.Header.Get("Authorization") != "Bearer test-key" ||
			r.Header.Get("X-Rinspace-Operation-Key") != "agent-operation-1" {
			t.Errorf("Agent provider request lost its fixed transport contract")
		}
		body, _ := io.ReadAll(r.Body)
		payload := string(body)
		for _, required := range []string{`"tool_choice":"auto"`, `"stream":false`,
			`"thinking":{"type":"disabled"}`, `"name":"read_project_files"`,
			`"role":"tool"`, `"tool_call_id":"call-0"`} {
			if !strings.Contains(payload, required) {
				t.Errorf("Agent payload is missing %s: %s", required, payload)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(agentToolResponse))
	}))
	defer server.Close()
	provider := OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key"}
	request := validAgentFixture("agent-operation-1")
	request.Messages = append(request.Messages,
		AgentMessage{Role: "assistant", ToolCalls: []AgentToolCall{{ID: "call-0", Type: "function",
			Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{"path":"intro.tex"}`}}}},
		AgentMessage{Role: "tool", ToolCallID: "call-0", Content: `{"content":"Introduction"}`})
	for attempt := 0; attempt < 2; attempt++ {
		result, err := provider.CompleteAgent(context.Background(), request)
		if err != nil || result.FinishReason != "tool_calls" || len(result.ToolCalls) != 1 ||
			result.ToolCalls[0].Function.Name != "read_project_files" || result.Usage.CacheHitTokens != 60 {
			t.Fatalf("Agent completion lost tool or usage data: result=%+v err=%v", result, err)
		}
	}
	if requests != 2 {
		t.Fatalf("duplicate operation test did not reach the idempotent gateway twice: %d", requests)
	}
}

func TestOpenAIProviderAgentFailureTimeoutAndCancellationStayUnresolved(t *testing.T) {
	t.Run("provider error is redacted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("secret upstream quota record"))
		}))
		defer server.Close()
		_, err := (OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key"}).
			CompleteAgent(context.Background(), validAgentFixture("agent-operation-2"))
		if err == nil || !strings.Contains(err.Error(), "HTTP 429") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("provider error was accepted or leaked: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
		}))
		defer server.Close()
		provider := OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key",
			Client: &http.Client{Timeout: 20 * time.Millisecond}}
		_, err := provider.CompleteAgent(context.Background(), validAgentFixture("agent-operation-3"))
		<-started
		close(release)
		if err == nil {
			t.Fatal("timed out Agent call was reported as completed")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := (OpenAIProvider{Endpoint: "http://127.0.0.1:1/v1/chat/completions", APIKey: "test-key"}).
			CompleteAgent(ctx, validAgentFixture("agent-operation-4"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Agent call did not retain cancellation: %v", err)
		}
	})
	t.Run("disconnect", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})}
		_, err := (OpenAIProvider{Endpoint: "https://gateway.invalid/v1/chat/completions",
			APIKey: "test-key", Client: client}).CompleteAgent(context.Background(), validAgentFixture("agent-operation-5"))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("disconnected Agent call was not unresolved: %v", err)
		}
	})
}

func TestOpenAIProviderAgentResponseBoundAndReceiptRecovery(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Header.Get("X-Rinspace-Operation-Key") == "agent-operation-7" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				_, _ = w.Write([]byte(strings.Repeat("x", MaxAgentResponseBytes+1)))
			} else {
				w.Header().Set("Content-Length", "1048577")
				_, _ = w.Write([]byte("oversized"))
			}
		case "/v1/rin/operations/done":
			w.Header().Set("X-Rinspace-Operation-Status", "completed")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(agentToolResponse))
		case "/v1/rin/operations/unknown":
			w.Header().Set("X-Rinspace-Operation-Status", "unknown")
			w.WriteHeader(http.StatusAccepted)
		case "/v1/rin/operations/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/rin/operations/missing/abort":
			w.Header().Set("X-Rinspace-Operation-Status", "expired")
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	provider := OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key"}
	if _, err := provider.CompleteAgent(context.Background(), validAgentFixture("agent-operation-6")); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("oversized Agent response was accepted: %v", err)
	}
	if _, err := provider.CompleteAgent(context.Background(), validAgentFixture("agent-operation-7")); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("chunked oversized Agent response was accepted: %v", err)
	}
	completed, err := provider.ReadAgentReceipt(context.Background(), "done")
	if err != nil || completed.Status != ReceiptCompleted || completed.Completion.RequestID != "agent-request-1" ||
		len(completed.Completion.ToolCalls) != 1 {
		t.Fatalf("Agent receipt lost original tool completion: receipt=%+v err=%v", completed, err)
	}
	unknown, err := provider.ReadAgentReceipt(context.Background(), "unknown")
	if err != nil || unknown.Status != ReceiptUnknown {
		t.Fatalf("unknown Agent receipt became terminal: receipt=%+v err=%v", unknown, err)
	}
	if _, err := provider.ReadAgentReceipt(context.Background(), "missing"); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("unclaimed Agent operation was not distinguishable: %v", err)
	}
	aborted, err := provider.AbortAgentReceipt(context.Background(), "missing")
	if err != nil || aborted.Status != ReceiptExpired {
		t.Fatalf("unclaimed Agent operation did not get an abort tombstone: receipt=%+v err=%v", aborted, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
