package rin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	MaxAgentMessages      = 96
	MaxAgentTools         = 32
	MaxAgentContentBytes  = 64 << 10
	MaxAgentSchemaBytes   = 64 << 10
	MaxAgentRequestBytes  = 1 << 20
	MaxAgentResponseBytes = 1 << 20
)

type AgentFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type AgentToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function AgentFunctionCall `json:"function"`
}

type AgentMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []AgentToolCall `json:"tool_calls,omitempty"`
}

type AgentFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type AgentToolDefinition struct {
	Type     string                  `json:"type"`
	Function AgentFunctionDefinition `json:"function"`
}

type AgentCompletionRequest struct {
	OperationID string                `json:"operation_id,omitempty"`
	Model       string                `json:"model"`
	Messages    []AgentMessage        `json:"messages"`
	Tools       []AgentToolDefinition `json:"tools"`
	MaxTokens   int                   `json:"max_tokens"`
}

type AgentCompletion struct {
	Text         string          `json:"text,omitempty"`
	ToolCalls    []AgentToolCall `json:"tool_calls,omitempty"`
	FinishReason string          `json:"finish_reason"`
	RequestID    string          `json:"request_id"`
	Model        string          `json:"model"`
	Usage        AgentUsage      `json:"usage"`
}

type AgentUsage struct {
	PromptTokens    int64 `json:"prompt_tokens"`
	CacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	CacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
	OutputTokens    int64 `json:"completion_tokens"`
}

type AgentOperationReceipt struct {
	Status     ReceiptStatus   `json:"status"`
	Completion AgentCompletion `json:"completion,omitempty"`
	HTTPStatus int             `json:"http_status"`
}

func ValidateAgentRequest(input AgentCompletionRequest) error {
	if !validOperationID(input.OperationID) || input.Model != DefaultModel ||
		input.MaxTokens < 1 || input.MaxTokens > 4096 ||
		len(input.Messages) == 0 || len(input.Messages) > MaxAgentMessages ||
		len(input.Tools) == 0 || len(input.Tools) > MaxAgentTools {
		return errors.New("rin agent request is invalid")
	}
	toolNames := make(map[string]struct{}, len(input.Tools))
	totalBytes := 0
	for _, tool := range input.Tools {
		if tool.Type != "function" || !validAgentName(tool.Function.Name) ||
			len(tool.Function.Description) > 4096 || !utf8.ValidString(tool.Function.Description) ||
			len(tool.Function.Parameters) == 0 ||
			len(tool.Function.Parameters) > MaxAgentSchemaBytes ||
			!validJSONObject(tool.Function.Parameters) {
			return errors.New("rin agent tool definition is invalid")
		}
		if _, exists := toolNames[tool.Function.Name]; exists {
			return errors.New("rin agent tool definition is duplicated")
		}
		toolNames[tool.Function.Name] = struct{}{}
		totalBytes += len(tool.Function.Name) + len(tool.Function.Description) + len(tool.Function.Parameters)
	}
	pending := make(map[string]struct{})
	seenCallIDs := make(map[string]struct{})
	for index, message := range input.Messages {
		if len(message.Content) > MaxAgentContentBytes || !utf8.ValidString(message.Content) {
			return errors.New("rin agent message content is invalid")
		}
		totalBytes += len(message.Role) + len(message.Content) + len(message.ToolCallID)
		switch message.Role {
		case "system":
			if index != 0 || strings.TrimSpace(message.Content) == "" || message.ToolCallID != "" ||
				len(message.ToolCalls) != 0 || len(pending) != 0 {
				return errors.New("rin agent system message is invalid")
			}
		case "user":
			if strings.TrimSpace(message.Content) == "" || message.ToolCallID != "" ||
				len(message.ToolCalls) != 0 || len(pending) != 0 {
				return errors.New("rin agent message is invalid")
			}
		case "assistant":
			if message.ToolCallID != "" || len(pending) != 0 ||
				(strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0) {
				return errors.New("rin agent assistant message is invalid")
			}
			for _, call := range message.ToolCalls {
				if err := validateAgentToolCall(call, toolNames, seenCallIDs); err != nil {
					return err
				}
				pending[call.ID] = struct{}{}
				totalBytes += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
			}
		case "tool":
			if strings.TrimSpace(message.Content) == "" || len(message.ToolCalls) != 0 ||
				!validAgentCallID(message.ToolCallID) {
				return errors.New("rin agent tool result is invalid")
			}
			if _, exists := pending[message.ToolCallID]; !exists {
				return errors.New("rin agent tool result has no pending call")
			}
			delete(pending, message.ToolCallID)
		default:
			return errors.New("rin agent message role is invalid")
		}
		if index == 0 && message.Role != "system" {
			return errors.New("rin agent system message is missing")
		}
	}
	lastRole := input.Messages[len(input.Messages)-1].Role
	if len(pending) != 0 || (lastRole != "user" && lastRole != "tool") ||
		totalBytes > MaxAgentRequestBytes {
		return errors.New("rin agent request exceeds its boundary")
	}
	return nil
}

func ValidateAgentCompletion(result AgentCompletion, request AgentCompletionRequest) error {
	if result.RequestID == "" || len(result.RequestID) > 256 ||
		result.Model != request.Model || !validAgentUsage(result.Usage) ||
		(request.MaxTokens > 0 && result.Usage.OutputTokens > int64(request.MaxTokens)) {
		return errors.New("rin agent completion identity or usage is invalid")
	}
	if len(result.Text) > MaxAgentContentBytes || !utf8.ValidString(result.Text) ||
		len(result.ToolCalls) > MaxAgentTools {
		return errors.New("rin agent completion content is invalid")
	}
	toolNames := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		toolNames[tool.Function.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(result.ToolCalls))
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			seen[call.ID] = struct{}{}
		}
	}
	for _, call := range result.ToolCalls {
		if err := validateAgentToolCall(call, toolNames, seen); err != nil {
			return err
		}
	}
	switch result.FinishReason {
	case "stop":
		if strings.TrimSpace(result.Text) == "" || len(result.ToolCalls) != 0 {
			return errors.New("rin agent stop response is invalid")
		}
	case "tool_calls":
		if len(result.ToolCalls) == 0 {
			return errors.New("rin agent tool response is empty")
		}
	case "length":
		if strings.TrimSpace(result.Text) == "" && len(result.ToolCalls) == 0 {
			return errors.New("rin agent length response is empty")
		}
	default:
		return errors.New("rin agent finish reason is invalid")
	}
	return nil
}

func validateAgentToolCall(call AgentToolCall, tools, seen map[string]struct{}) error {
	if !validAgentCallID(call.ID) || call.Type != "function" ||
		!validAgentName(call.Function.Name) || len(call.Function.Arguments) == 0 ||
		len(call.Function.Arguments) > MaxAgentSchemaBytes ||
		!validJSONObject([]byte(call.Function.Arguments)) {
		return errors.New("rin agent tool call is invalid")
	}
	if _, exists := tools[call.Function.Name]; !exists {
		return errors.New("rin agent tool call is not declared")
	}
	if _, exists := seen[call.ID]; exists {
		return errors.New("rin agent tool call ID is duplicated")
	}
	seen[call.ID] = struct{}{}
	return nil
}

func validAgentUsage(usage AgentUsage) bool {
	return usage.PromptTokens >= 0 && usage.CacheHitTokens >= 0 &&
		usage.CacheMissTokens >= 0 && usage.OutputTokens >= 0 &&
		usage.CacheHitTokens <= usage.PromptTokens &&
		usage.CacheMissTokens <= usage.PromptTokens &&
		usage.CacheHitTokens == usage.PromptTokens-usage.CacheMissTokens
}

func validAgentName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validAgentCallID(value string) bool {
	return len(value) <= 128 && validOperationID(value)
}

func validJSONObject(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return false
	}
	_, ok := value.(map[string]any)
	return ok
}
