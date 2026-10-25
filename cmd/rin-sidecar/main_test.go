package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rin "github.com/rinspacehq/rinspace-rin"
)

type fakeProvider struct {
	calls            int
	request          rin.CompletionRequest
	agentCalls       int
	agentRequest     rin.AgentCompletionRequest
	readID           string
	abortID          string
	readResult       rin.OperationReceipt
	abortResult      rin.OperationReceipt
	readErr          error
	agentReadID      string
	agentAbortID     string
	agentReadResult  rin.AgentOperationReceipt
	agentAbortResult rin.AgentOperationReceipt
	agentReadErr     error
}

func (fake *fakeProvider) Complete(_ context.Context, request rin.CompletionRequest) (rin.Completion, error) {
	fake.calls++
	fake.request = request
	return rin.Completion{Text: " “你好，世界” ", RequestID: "paid-1", Model: rin.DefaultModel,
		Usage: rin.Usage{PromptTokens: 9, CacheMissTokens: 9, OutputTokens: 4}}, nil
}

func (fake *fakeProvider) ReadReceipt(_ context.Context, id string) (rin.OperationReceipt, error) {
	fake.readID = id
	return fake.readResult, fake.readErr
}

func (fake *fakeProvider) AbortReceipt(_ context.Context, id string) (rin.OperationReceipt, error) {
	fake.abortID = id
	return fake.abortResult, nil
}

func (fake *fakeProvider) CompleteAgent(_ context.Context, request rin.AgentCompletionRequest) (rin.AgentCompletion, error) {
	fake.agentCalls++
	fake.agentRequest = request
	return rin.AgentCompletion{RequestID: "agent-request-1", Model: rin.DefaultModel,
		FinishReason: "tool_calls", ToolCalls: []rin.AgentToolCall{{ID: "call-1", Type: "function",
			Function: rin.AgentFunctionCall{Name: "read_project_files", Arguments: `{"path":"main.tex"}`}}},
		Usage: rin.AgentUsage{PromptTokens: 10, CacheHitTokens: 2, CacheMissTokens: 8, OutputTokens: 4}}, nil
}

func (fake *fakeProvider) ReadAgentReceipt(_ context.Context, id string) (rin.AgentOperationReceipt, error) {
	fake.agentReadID = id
	return fake.agentReadResult, fake.agentReadErr
}

func (fake *fakeProvider) AbortAgentReceipt(_ context.Context, id string) (rin.AgentOperationReceipt, error) {
	fake.agentAbortID = id
	return fake.agentAbortResult, nil
}

func sidecarRequest(handler http.Handler, method, path string, body any, authorized bool) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorized {
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSidecarFreezesInputThenCompletesOnce(t *testing.T) {
	fake := &fakeProvider{}
	handler := sidecar{token: strings.Repeat("s", 32), provider: fake, model: rin.DefaultModel, maxTokens: 1200}.handler()
	input := rin.ChatInput{AuthorName: "测试用户", Page: rin.PageContext{Title: "书籍", Selection: "定义"},
		History: []rin.Message{{Role: "user", Content: "解释定义"}}}
	if got := sidecarRequest(handler, http.MethodPost, "/v1/prepare", input, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized prepare: %d", got.Code)
	}
	prepared := sidecarRequest(handler, http.MethodPost, "/v1/prepare", input, true)
	if prepared.Code != http.StatusOK || fake.calls != 0 {
		t.Fatalf("prepare must not call model: status=%d calls=%d", prepared.Code, fake.calls)
	}
	var frozen rin.CompletionRequest
	if err := json.Unmarshal(prepared.Body.Bytes(), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.OperationID != "" || len(frozen.Messages) != 3 || !strings.Contains(frozen.Messages[1].Content, "定义") {
		t.Fatalf("frozen context was lost: %+v", frozen)
	}
	complete := sidecarRequest(handler, http.MethodPost, "/v1/complete", map[string]any{
		"operation_id": "rin-chat-42", "frozen": frozen,
	}, true)
	if complete.Code != http.StatusOK || fake.calls != 1 || fake.request.OperationID != "rin-chat-42" {
		t.Fatalf("completion did not use original operation: status=%d calls=%d request=%+v", complete.Code, fake.calls, fake.request)
	}
	var result rin.Completion
	if err := json.Unmarshal(complete.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Text != "你好，世界" || result.Usage.OutputTokens != 4 {
		t.Fatalf("answer or metering was changed: %+v", result)
	}
}

func TestSidecarReceiptRecoveryDoesNotCallModelAgain(t *testing.T) {
	fake := &fakeProvider{readResult: rin.OperationReceipt{Status: rin.ReceiptCompleted,
		Completion: rin.Completion{Text: " ‘原回答’ ", Usage: rin.Usage{PromptTokens: 9, OutputTokens: 4}}}}
	handler := sidecar{token: strings.Repeat("s", 32), provider: fake}.handler()
	response := sidecarRequest(handler, http.MethodGet, "/v1/operations/rin-chat-42", nil, true)
	if response.Code != http.StatusOK || fake.readID != "rin-chat-42" || fake.calls != 0 {
		t.Fatalf("receipt must read original operation: status=%d id=%q calls=%d", response.Code, fake.readID, fake.calls)
	}
	var receipt rin.OperationReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Completion.Text != "原回答" {
		t.Fatalf("receipt answer was not cleaned: %+v", receipt)
	}
	fake.readErr = rin.ErrReceiptNotFound
	if got := sidecarRequest(handler, http.MethodGet, "/v1/operations/rin-chat-42", nil, true); got.Code != http.StatusNotFound {
		t.Fatalf("unclaimed operation: %d", got.Code)
	}
	fake.abortResult = rin.OperationReceipt{Status: rin.ReceiptExpired}
	if got := sidecarRequest(handler, http.MethodPost, "/v1/operations/rin-chat-42/abort", nil, true); got.Code != http.StatusGone || fake.abortID != "rin-chat-42" {
		t.Fatalf("abort tombstone: status=%d id=%q", got.Code, fake.abortID)
	}
	fake.readErr = errors.New("gateway unavailable")
	if got := sidecarRequest(handler, http.MethodGet, "/v1/operations/rin-chat-42", nil, true); got.Code != http.StatusBadGateway {
		t.Fatalf("transport failure must remain unresolved: %d", got.Code)
	}
}

func TestSidecarRejectsMalformedFrozenRequestBeforeGateway(t *testing.T) {
	fake := &fakeProvider{}
	handler := sidecar{token: strings.Repeat("s", 32), provider: fake}.handler()
	response := sidecarRequest(handler, http.MethodPost, "/v1/complete", map[string]any{
		"operation_id": "rin-chat-42", "frozen": rin.CompletionRequest{OperationID: "different", Model: rin.DefaultModel,
			Messages: []rin.Message{{Role: "system", Content: "a"}, {Role: "user", Content: "b"}}, MaxTokens: 1200},
	}, true)
	if response.Code != http.StatusBadRequest || fake.calls != 0 {
		t.Fatalf("invalid frozen request reached gateway: status=%d calls=%d", response.Code, fake.calls)
	}
}

func TestSidecarAgentProtocolFixesProviderAndNeverExecutesTools(t *testing.T) {
	fake := &fakeProvider{}
	handler := sidecar{token: strings.Repeat("s", 32), provider: fake,
		model: rin.DefaultModel, maxTokens: 1200}.handler()
	input := map[string]any{
		"operation_id": "agent-operation-1",
		"messages": []rin.AgentMessage{{Role: "system", Content: "Edit safely"},
			{Role: "user", Content: "Read the main file"}},
		"tools": []rin.AgentToolDefinition{{Type: "function", Function: rin.AgentFunctionDefinition{
			Name: "read_project_files", Description: "Read one bounded file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}},
		"max_output_tokens": 600,
	}
	if got := sidecarRequest(handler, http.MethodPost, "/v1/agent/completions", input, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized Agent call: %d", got.Code)
	}
	response := sidecarRequest(handler, http.MethodPost, "/v1/agent/completions", input, true)
	if response.Code != http.StatusOK || fake.agentCalls != 1 ||
		fake.agentRequest.Model != rin.DefaultModel || fake.agentRequest.OperationID != "agent-operation-1" ||
		fake.agentRequest.MaxTokens != 600 {
		t.Fatalf("Agent request crossed an unfixed boundary: status=%d calls=%d request=%+v",
			response.Code, fake.agentCalls, fake.agentRequest)
	}
	var completion rin.AgentCompletion
	if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil ||
		completion.FinishReason != "tool_calls" || len(completion.ToolCalls) != 1 {
		t.Fatalf("Agent tool call was not returned as data: completion=%+v err=%v", completion, err)
	}
	if body := response.Body.String(); !strings.Contains(body, `"prompt_tokens":10`) ||
		strings.Contains(body, `"PromptTokens"`) {
		t.Fatalf("Agent usage is not OpenAI-compatible JSON: %s", body)
	}
	for _, forbidden := range []string{"model", "base_url", "api_key", "headers"} {
		changed := make(map[string]any, len(input)+1)
		for key, value := range input {
			changed[key] = value
		}
		changed[forbidden] = "attacker-controlled"
		got := sidecarRequest(handler, http.MethodPost, "/v1/agent/completions", changed, true)
		if got.Code != http.StatusBadRequest || fake.agentCalls != 1 {
			t.Fatalf("forbidden Agent provider field %q reached the gateway: status=%d calls=%d",
				forbidden, got.Code, fake.agentCalls)
		}
	}
}

func TestSidecarAgentReceiptRecoveryNeverRepeatsCompletion(t *testing.T) {
	fake := &fakeProvider{agentReadResult: rin.AgentOperationReceipt{Status: rin.ReceiptCompleted,
		Completion: rin.AgentCompletion{Text: "Recovered", FinishReason: "stop",
			RequestID: "agent-receipt-1", Model: rin.DefaultModel,
			Usage: rin.AgentUsage{PromptTokens: 5, CacheMissTokens: 5, OutputTokens: 2}}}}
	handler := sidecar{token: strings.Repeat("s", 32), provider: fake}.handler()
	response := sidecarRequest(handler, http.MethodGet, "/v1/agent/operations/agent-operation-1", nil, true)
	if response.Code != http.StatusOK || fake.agentReadID != "agent-operation-1" || fake.agentCalls != 0 {
		t.Fatalf("Agent receipt repeated completion: status=%d id=%q calls=%d",
			response.Code, fake.agentReadID, fake.agentCalls)
	}
	fake.agentReadErr = rin.ErrReceiptNotFound
	if got := sidecarRequest(handler, http.MethodGet, "/v1/agent/operations/missing", nil, true); got.Code != http.StatusNotFound {
		t.Fatalf("missing Agent receipt: %d", got.Code)
	}
	fake.agentAbortResult = rin.AgentOperationReceipt{Status: rin.ReceiptExpired}
	if got := sidecarRequest(handler, http.MethodPost, "/v1/agent/operations/missing/abort", nil, true); got.Code != http.StatusGone || fake.agentAbortID != "missing" {
		t.Fatalf("Agent abort tombstone: status=%d id=%q", got.Code, fake.agentAbortID)
	}
}
