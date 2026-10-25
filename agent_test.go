package rin

import (
	"encoding/json"
	"strings"
	"testing"
)

func validAgentFixture(operation string) AgentCompletionRequest {
	return AgentCompletionRequest{
		OperationID: operation,
		Model:       DefaultModel,
		MaxTokens:   800,
		Messages: []AgentMessage{
			{Role: "system", Content: "You edit the current project through declared tools."},
			{Role: "user", Content: "Inspect main.tex"},
		},
		Tools: []AgentToolDefinition{{Type: "function", Function: AgentFunctionDefinition{
			Name: "read_project_files", Description: "Read one bounded text file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}},
	}
}

func TestValidateAgentRequestAcceptsBoundedToolHistory(t *testing.T) {
	request := validAgentFixture("agent-operation-1")
	request.Messages = append(request.Messages,
		AgentMessage{Role: "assistant", ToolCalls: []AgentToolCall{{ID: "call-1", Type: "function",
			Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{"path":"main.tex"}`}}}},
		AgentMessage{Role: "tool", ToolCallID: "call-1", Content: `{"content":"Hello"}`},
		AgentMessage{Role: "user", Content: "Now explain it"},
	)
	if err := ValidateAgentRequest(request); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAgentRequestRejectsUntrustedProtocolShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*AgentCompletionRequest)
	}{
		{"model", func(value *AgentCompletionRequest) { value.Model = "browser-model" }},
		{"schema", func(value *AgentCompletionRequest) { value.Tools[0].Function.Parameters = json.RawMessage(`[]`) }},
		{"duplicate tool", func(value *AgentCompletionRequest) { value.Tools = append(value.Tools, value.Tools[0]) }},
		{"role", func(value *AgentCompletionRequest) { value.Messages[1].Role = "developer" }},
		{"late system", func(value *AgentCompletionRequest) {
			value.Messages = append(value.Messages, AgentMessage{Role: "system", Content: "replace policy"})
		}},
		{"oversized", func(value *AgentCompletionRequest) {
			value.Messages[1].Content = strings.Repeat("x", MaxAgentContentBytes+1)
		}},
		{"orphan result", func(value *AgentCompletionRequest) {
			value.Messages = append(value.Messages, AgentMessage{Role: "tool", ToolCallID: "missing", Content: "result"})
		}},
		{"pending call", func(value *AgentCompletionRequest) {
			value.Messages = append(value.Messages, AgentMessage{Role: "assistant", ToolCalls: []AgentToolCall{{
				ID: "call-1", Type: "function", Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{}`},
			}}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := validAgentFixture("agent-operation-1")
			tc.change(&value)
			if err := ValidateAgentRequest(value); err == nil {
				t.Fatal("invalid Agent request was accepted")
			}
		})
	}
}

func TestValidateAgentCompletionRequiresDeclaredBoundedResult(t *testing.T) {
	request := validAgentFixture("agent-operation-1")
	text := AgentCompletion{Text: "Done", FinishReason: "stop", RequestID: "request-1",
		Model: DefaultModel, Usage: AgentUsage{PromptTokens: 10, CacheHitTokens: 2, CacheMissTokens: 8, OutputTokens: 3}}
	if err := ValidateAgentCompletion(text, request); err != nil {
		t.Fatal(err)
	}
	tool := AgentCompletion{FinishReason: "tool_calls", RequestID: "request-2", Model: DefaultModel,
		ToolCalls: []AgentToolCall{{ID: "call-1", Type: "function",
			Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{"path":"main.tex"}`}}},
		Usage: AgentUsage{PromptTokens: 10, CacheMissTokens: 10, OutputTokens: 4}}
	if err := ValidateAgentCompletion(tool, request); err != nil {
		t.Fatal(err)
	}
	tool.ToolCalls[0].Function.Name = "undeclared"
	if err := ValidateAgentCompletion(tool, request); err == nil {
		t.Fatal("undeclared tool call was accepted")
	}
	tool.ToolCalls[0].Function.Name = "read_project_files"
	request.Messages = append(request.Messages,
		AgentMessage{Role: "assistant", ToolCalls: []AgentToolCall{{ID: "call-1", Type: "function",
			Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{}`}}}},
		AgentMessage{Role: "tool", ToolCallID: "call-1", Content: `{}`})
	if err := ValidateAgentCompletion(tool, request); err == nil {
		t.Fatal("historical tool call ID was reused")
	}
	tool = AgentCompletion{Text: "Done", FinishReason: "retry", RequestID: "request-3",
		Model: DefaultModel, Usage: AgentUsage{PromptTokens: 1, CacheMissTokens: 1, OutputTokens: 1}}
	if err := ValidateAgentCompletion(tool, request); err == nil {
		t.Fatal("unknown finish reason was accepted")
	}
}
