package rin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (p OpenAIProvider) CompleteAgent(ctx context.Context, input AgentCompletionRequest) (AgentCompletion, error) {
	if err := ValidateAgentRequest(input); err != nil {
		return AgentCompletion{}, err
	}
	endpoint, err := p.agentEndpoint()
	if err != nil {
		return AgentCompletion{}, err
	}
	payload, err := json.Marshal(struct {
		Model       string                `json:"model"`
		Messages    []AgentMessage        `json:"messages"`
		Tools       []AgentToolDefinition `json:"tools"`
		ToolChoice  string                `json:"tool_choice"`
		Stream      bool                  `json:"stream"`
		MaxTokens   int                   `json:"max_tokens"`
		Temperature float64               `json:"temperature"`
		Thinking    struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}{
		Model: input.Model, Messages: input.Messages, Tools: input.Tools,
		ToolChoice: "auto", Stream: false, MaxTokens: input.MaxTokens, Temperature: 0.2,
		Thinking: struct {
			Type string `json:"type"`
		}{Type: "disabled"},
	})
	if err != nil {
		return AgentCompletion{}, errors.New("rin agent request could not be encoded")
	}
	if len(payload) > MaxAgentRequestBytes {
		return AgentCompletion{}, errors.New("rin agent request exceeds its boundary")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return AgentCompletion{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Rinspace-Operation-Key", input.OperationID)
	response, err := p.agentHTTPClient().Do(request)
	if err != nil {
		return AgentCompletion{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AgentCompletion{}, fmt.Errorf("rin agent provider returned HTTP %d", response.StatusCode)
	}
	body, err := readAgentResponse(response)
	if err != nil {
		return AgentCompletion{}, err
	}
	return parseAgentCompletion(body, input)
}

func (p OpenAIProvider) ReadAgentReceipt(ctx context.Context, operationID string) (AgentOperationReceipt, error) {
	return p.readAgentReceipt(ctx, operationID, false)
}

func (p OpenAIProvider) AbortAgentReceipt(ctx context.Context, operationID string) (AgentOperationReceipt, error) {
	return p.readAgentReceipt(ctx, operationID, true)
}

func (p OpenAIProvider) readAgentReceipt(ctx context.Context, operationID string, abort bool) (AgentOperationReceipt, error) {
	if !validOperationID(operationID) {
		return AgentOperationReceipt{}, errors.New("rin operation ID is invalid")
	}
	endpoint, err := p.agentEndpoint()
	if err != nil {
		return AgentOperationReceipt{}, err
	}
	endpoint.Path = "/v1/rin/operations/" + operationID
	method := http.MethodGet
	if abort {
		method = http.MethodPost
		endpoint.Path += "/abort"
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return AgentOperationReceipt{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("Accept", "application/json")
	response, err := p.agentHTTPClient().Do(request)
	if err != nil {
		return AgentOperationReceipt{}, err
	}
	defer response.Body.Close()
	if !abort && response.StatusCode == http.StatusNotFound && response.Header.Get("X-Rinspace-Operation-Status") == "" {
		return AgentOperationReceipt{}, ErrReceiptNotFound
	}
	status := ReceiptStatus(response.Header.Get("X-Rinspace-Operation-Status"))
	receipt := AgentOperationReceipt{Status: status, HTTPStatus: response.StatusCode}
	switch status {
	case ReceiptUnknown:
		if response.StatusCode == http.StatusAccepted {
			return receipt, nil
		}
	case ReceiptExpired:
		if response.StatusCode == http.StatusGone {
			return receipt, nil
		}
	case ReceiptFailed:
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return receipt, nil
		}
	case ReceiptCompleted:
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			body, err := readAgentResponse(response)
			if err != nil {
				return AgentOperationReceipt{}, err
			}
			completion, err := parseAgentCompletion(body, AgentCompletionRequest{Model: DefaultModel})
			if err != nil {
				return AgentOperationReceipt{}, err
			}
			receipt.Completion = completion
			return receipt, nil
		}
	}
	return AgentOperationReceipt{}, errors.New("rin agent operation receipt is unavailable")
}

func (p OpenAIProvider) agentEndpoint() (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(p.Endpoint))
	if err != nil || endpoint.Host == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" ||
		endpoint.Path != "/v1/chat/completions" {
		return nil, errors.New("rin agent provider endpoint is invalid")
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return nil, errors.New("rin agent provider key is missing")
	}
	return endpoint, nil
}

func (p OpenAIProvider) agentHTTPClient() *http.Client {
	client := &http.Client{Timeout: 120 * time.Second}
	if p.Client != nil {
		clone := *p.Client
		client = &clone
		if client.Timeout == 0 {
			client.Timeout = 120 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("rin agent provider redirects are disabled")
	}
	return client
}

func readAgentResponse(response *http.Response) ([]byte, error) {
	if response.ContentLength > MaxAgentResponseBytes {
		return nil, errors.New("rin agent provider response exceeds its boundary")
	}
	if contentType := strings.ToLower(response.Header.Get("Content-Type")); !strings.HasPrefix(contentType, "application/json") {
		return nil, errors.New("rin agent provider response type is invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxAgentResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxAgentResponseBytes {
		return nil, errors.New("rin agent provider response exceeds its boundary")
	}
	return body, nil
}

func parseAgentCompletion(body []byte, request AgentCompletionRequest) (AgentCompletion, error) {
	var result struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string          `json:"content"`
				ToolCalls []AgentToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens          *int64 `json:"prompt_tokens"`
			CompletionTokens      *int64 `json:"completion_tokens"`
			PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`
			PromptTokensDetails   struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&result); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return AgentCompletion{}, errors.New("rin agent provider returned invalid JSON")
	}
	if len(result.Choices) != 1 || result.Usage == nil ||
		result.Usage.PromptTokens == nil || result.Usage.CompletionTokens == nil {
		return AgentCompletion{}, errors.New("rin agent provider returned no completion or usage")
	}
	prompt, output := *result.Usage.PromptTokens, *result.Usage.CompletionTokens
	hit := int64(0)
	if result.Usage.PromptCacheHitTokens != nil {
		hit = *result.Usage.PromptCacheHitTokens
	} else if result.Usage.PromptTokensDetails.CachedTokens != nil {
		hit = *result.Usage.PromptTokensDetails.CachedTokens
	}
	miss := prompt - hit
	if result.Usage.PromptCacheMissTokens != nil {
		miss = *result.Usage.PromptCacheMissTokens
	}
	model := result.Model
	if model == "" {
		model = request.Model
	}
	completion := AgentCompletion{
		Text: result.Choices[0].Message.Content, ToolCalls: result.Choices[0].Message.ToolCalls,
		FinishReason: result.Choices[0].FinishReason, RequestID: result.ID, Model: model,
		Usage: AgentUsage{PromptTokens: prompt, CacheHitTokens: hit, CacheMissTokens: miss, OutputTokens: output},
	}
	validationRequest := request
	if len(validationRequest.Tools) == 0 {
		for _, call := range completion.ToolCalls {
			validationRequest.Tools = append(validationRequest.Tools, AgentToolDefinition{
				Type: "function", Function: AgentFunctionDefinition{Name: call.Function.Name},
			})
		}
	}
	if err := ValidateAgentCompletion(completion, validationRequest); err != nil {
		return AgentCompletion{}, err
	}
	return completion, nil
}
