package rin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIProviderCarriesChatContractAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Rinspace-Operation-Key") != "op-123" {
			t.Errorf("missing internal credentials or operation ID")
		}
		var request struct {
			Model    string `json:"model"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != DefaultModel || request.Thinking.Type != "disabled" || len(request.Messages) < 3 {
			t.Errorf("chat request lost model, non-thinking mode or history: %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gateway-456","model":"deepseek-flash","choices":[{"message":{"content":"你好 :rin_happy: "}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`))
	}))
	defer server.Close()
	client := Client{Provider: OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key"}}
	result, err := client.Chat(context.Background(), ChatInput{
		OperationID: "op-123", AuthorName: "测试", Page: PageContext{Title: "文章"},
		History: []Message{{Role: "user", Content: "解释这段内容"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestID != "gateway-456" || result.Usage.CacheHitTokens != 60 || result.Usage.CacheMissTokens != 40 || result.Usage.OutputTokens != 20 {
		t.Fatalf("usage receipt lost: %+v", result)
	}
}

func TestOpenAIProviderRejectsMissingUsageAndDoesNotLeakErrorBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"missing usage", 200, `{"choices":[{"message":{"content":"hello"}}]}`, "usage"},
		{"provider error", 502, `secret-provider-response`, "HTTP 502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := (OpenAIProvider{Endpoint: server.URL, APIKey: "test-key"}).Complete(context.Background(), CompletionRequest{Model: DefaultModel, MaxTokens: 20, Messages: []Message{{Role: "system", Content: "a"}, {Role: "user", Content: "b"}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-provider-response") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestOpenAIProviderReadReceiptUsesDurableGatewayStatus(t *testing.T) {
	const answer = `{"id":"gateway-456","model":"deepseek-flash","choices":[{"message":{"content":"已找到原回答"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("receipt query lost method or authorization")
		}
		switch r.URL.Path {
		case "/v1/rin/operations/done":
			w.Header().Set("X-Rinspace-Operation-Status", "completed")
			_, _ = w.Write([]byte(answer))
		case "/v1/rin/operations/failed":
			w.Header().Set("X-Rinspace-Operation-Status", "failed")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"private upstream failure"}`))
		case "/v1/rin/operations/unknown":
			w.Header().Set("X-Rinspace-Operation-Status", "unknown")
			w.WriteHeader(http.StatusAccepted)
		case "/v1/rin/operations/expired":
			w.Header().Set("X-Rinspace-Operation-Status", "expired")
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`secret-transient-error`))
		}
	}))
	defer server.Close()
	provider := OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "test-key"}
	for _, item := range []struct {
		id     string
		status ReceiptStatus
	}{
		{"done", ReceiptCompleted}, {"failed", ReceiptFailed},
		{"unknown", ReceiptUnknown}, {"expired", ReceiptExpired},
	} {
		receipt, err := provider.ReadReceipt(context.Background(), item.id)
		if err != nil || receipt.Status != item.status {
			t.Fatalf("receipt %s: status=%s err=%v", item.id, receipt.Status, err)
		}
		if item.id == "done" && (receipt.Completion.RequestID != "gateway-456" || receipt.Completion.Usage.CacheMissTokens != 40) {
			t.Fatalf("completed receipt lost answer or usage: %+v", receipt)
		}
		if item.id == "failed" && receipt.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("stored failure lost original status: %+v", receipt)
		}
	}
	for _, id := range []string{"unavailable", "bad/id"} {
		_, err := provider.ReadReceipt(context.Background(), id)
		if err == nil || strings.Contains(err.Error(), "secret-transient-error") {
			t.Fatalf("untrusted receipt response accepted or leaked: %v", err)
		}
	}
}
