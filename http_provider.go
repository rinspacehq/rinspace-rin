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

// OpenAIProvider calls an OpenAI-compatible chat endpoint, including new-api.
// The host owns the endpoint and secret; callers cannot choose either per message.
type OpenAIProvider struct {
	Endpoint string
	APIKey   string
	Client   *http.Client
}

func (p OpenAIProvider) Complete(ctx context.Context, input CompletionRequest) (Completion, error) {
	endpoint, err := url.Parse(strings.TrimSpace(p.Endpoint))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.Fragment != "" {
		return Completion{}, errors.New("rin provider endpoint is invalid")
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Completion{}, errors.New("rin provider key is missing")
	}
	if input.Model == "" || input.MaxTokens < 1 || input.MaxTokens > 4096 || len(input.Messages) < 2 {
		return Completion{}, errors.New("rin completion request is invalid")
	}
	if input.OperationID != "" && !validOperationID(input.OperationID) {
		return Completion{}, errors.New("rin operation ID is invalid")
	}
	payload, err := json.Marshal(struct {
		Model       string    `json:"model"`
		Messages    []Message `json:"messages"`
		Stream      bool      `json:"stream"`
		MaxTokens   int       `json:"max_tokens"`
		Temperature float64   `json:"temperature"`
		Thinking    struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}{
		Model: input.Model, Messages: input.Messages, Stream: false,
		MaxTokens: input.MaxTokens, Temperature: 0.68,
		Thinking: struct {
			Type string `json:"type"`
		}{Type: "disabled"},
	})
	if err != nil {
		return Completion{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return Completion{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if id := strings.TrimSpace(input.OperationID); id != "" {
		request.Header.Set("X-Rinspace-Operation-Key", id)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return Completion{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Completion{}, fmt.Errorf("rin provider returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Completion{}, err
	}
	return parseCompletion(body, input.Model)
}

// ReadReceipt queries the original gateway call after a timeout. Only a
// gateway status header is authoritative; a bare HTTP error is unresolved.
func (p OpenAIProvider) ReadReceipt(ctx context.Context, operationID string) (OperationReceipt, error) {
	if !validOperationID(operationID) {
		return OperationReceipt{}, errors.New("rin operation ID is invalid")
	}
	endpoint, err := url.Parse(strings.TrimSpace(p.Endpoint))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.Fragment != "" || endpoint.Path != "/v1/chat/completions" || endpoint.RawQuery != "" {
		return OperationReceipt{}, errors.New("rin provider endpoint is invalid")
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return OperationReceipt{}, errors.New("rin provider key is missing")
	}
	endpoint.Path = "/v1/rin/operations/" + operationID
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return OperationReceipt{}, err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer response.Body.Close()
	status := ReceiptStatus(response.Header.Get("X-Rinspace-Operation-Status"))
	receipt := OperationReceipt{Status: status, HTTPStatus: response.StatusCode}
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
			body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			if err != nil {
				return OperationReceipt{}, err
			}
			completion, err := parseCompletion(body, DefaultModel)
			if err != nil {
				return OperationReceipt{}, err
			}
			receipt.Completion = completion
			return receipt, nil
		}
	}
	return OperationReceipt{}, errors.New("rin operation receipt is unavailable")
}

func validOperationID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

func parseCompletion(body []byte, requestedModel string) (Completion, error) {
	var result struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
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
	if err := json.Unmarshal(body, &result); err != nil {
		return Completion{}, errors.New("rin provider returned invalid JSON")
	}
	if len(result.Choices) == 0 || result.Usage == nil || result.Usage.PromptTokens == nil || result.Usage.CompletionTokens == nil {
		return Completion{}, errors.New("rin provider returned no answer or usage")
	}
	prompt, output := *result.Usage.PromptTokens, *result.Usage.CompletionTokens
	if prompt < 0 || output < 0 {
		return Completion{}, errors.New("rin provider returned invalid usage")
	}
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
	if hit < 0 || miss < 0 || hit > prompt || miss > prompt || hit+miss != prompt {
		return Completion{}, errors.New("rin provider returned inconsistent cache usage")
	}
	return Completion{
		Text: result.Choices[0].Message.Content, RequestID: result.ID,
		Model: firstNonEmpty(result.Model, requestedModel),
		Usage: Usage{PromptTokens: prompt, CacheHitTokens: hit, CacheMissTokens: miss, OutputTokens: output},
	}, nil
}
