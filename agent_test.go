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

func TestValidateAgentRequestProviderOutputCeiling(t *testing.T) {
	request := validAgentFixture("agent-limit-1")
	request.MaxTokens = MaxAgentOutputTokens
	if err := ValidateAgentRequest(request); err != nil {
		t.Fatalf("provider maximum was rejected: %v", err)
	}
	request.MaxTokens++
	if err := ValidateAgentRequest(request); err == nil {
		t.Fatal("request above provider maximum was accepted")
	}
}

func TestLearningBudgetRetainsOrdinaryAgentLimits(t *testing.T) {
	request := validAgentFixture("learning-1")
	request.MaxTokens = 32768
	request.Messages[1].Content = strings.Repeat("x", 80000)
	for len(request.Messages) < 100 {
		request.Messages = append(request.Messages, AgentMessage{Role: "user", Content: "Original followup."})
	}
	if err := ValidateAgentRequest(request); err == nil {
		t.Fatal("ordinary Agent received learning context limits")
	}
	request.BudgetProfile = ReaderLearningBudgetV1
	if err := ValidateAgentRequest(request); err != nil {
		t.Fatal(err)
	}
	result := AgentCompletion{RequestID: "synthetic-1", Model: DefaultModel, FinishReason: "tool_calls", ToolCalls: []AgentToolCall{{ID: "present-1", Type: "function", Function: AgentFunctionCall{Name: "read_project_files", Arguments: `{"text":"` + strings.Repeat("a", 80000) + `"}`}}}, Usage: AgentUsage{PromptTokens: 1000, CacheMissTokens: 1000, OutputTokens: 200}}
	if err := ValidateAgentCompletion(result, request); err != nil {
		t.Fatalf("complete structured teaching output was truncated: %v", err)
	}
	ordinary := request
	ordinary.BudgetProfile = ""
	if err := ValidateAgentCompletion(result, ordinary); err == nil {
		t.Fatal("learning output widened ordinary tool arguments")
	}
	for _, scenario := range []string{"output", "messages", "content", "unknown-profile"} {
		t.Run(scenario, func(t *testing.T) {
			changed := request
			changed.Messages = append([]AgentMessage{}, request.Messages...)
			switch scenario {
			case "output":
				changed.MaxTokens = 65537
			case "messages":
				for len(changed.Messages) < 257 {
					changed.Messages = append(changed.Messages, AgentMessage{Role: "user", Content: "bounded"})
				}
			case "content":
				changed.Messages[1].Content = strings.Repeat("x", (1<<20)+1)
			case "unknown-profile":
				changed.BudgetProfile = "browser-selected"
			}
			if err := ValidateAgentRequest(changed); err == nil {
				t.Fatal("invalid learning budget accepted")
			}
		})
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
	request.MaxTokens = MaxAgentOutputTokens
	text.Text = strings.Repeat("a", 70<<10)
	text.Usage.OutputTokens = 70 << 10
	if err := ValidateAgentCompletion(text, request); err != nil {
		t.Fatalf("reply above the former 64 KiB boundary was rejected: %v", err)
	}
	request.MaxTokens = 800
	text.Text = "Done"
	text.Usage.OutputTokens = 3
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
