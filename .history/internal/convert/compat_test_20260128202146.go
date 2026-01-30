package convert

import (
	"encoding/json"
	"testing"

	anthropicproto "claude-gateway/internal/proto/anthropic"
	openaiproto "claude-gateway/internal/proto/openai"
)

func TestOpenAIToAnthropicMessageRequest_ToolsAndToolCalls(t *testing.T) {
	rawTools := json.RawMessage(`[
		{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}}
	]`)
	rawToolChoice := json.RawMessage(`"auto"`)
	req := openaiproto.ChatCompletionsRequest{
		Model: "gpt-4",
		Messages: []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{
				"role":    "assistant",
				"content": "calling tool",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": `{"location":"SF"}`,
						},
					},
				},
			},
		},
		Tools:      rawTools,
		ToolChoice: rawToolChoice,
	}

	ar, err := OpenAIToAnthropicMessageRequest(req)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if ar.System != "sys" {
		t.Fatalf("expected system sys, got %#v", ar.System)
	}
	if len(ar.Tools) != 1 || ar.Tools[0].Name != "get_weather" {
		t.Fatalf("expected 1 tool get_weather, got %#v", ar.Tools)
	}
	if string(ar.ToolChoice) != `{"type":"auto"}` {
		t.Fatalf("unexpected tool_choice: %s", string(ar.ToolChoice))
	}
	if len(ar.Messages) != 2 {
		t.Fatalf("expected 2 messages (user,assistant), got %d", len(ar.Messages))
	}
	if ar.Messages[1].Role != "assistant" {
		t.Fatalf("expected assistant role, got %s", ar.Messages[1].Role)
	}
	blocks, ok := ar.Messages[1].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map blocks, got %T", ar.Messages[1].Content)
	}
	foundTool := false
	for _, b := range blocks {
		if b["type"] == "tool_use" && b["id"] == "call_1" && b["name"] == "get_weather" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("expected tool_use block, got %#v", blocks)
	}
}

func TestAnthropicToOpenAIChatRequest_ToolUseAndToolResult(t *testing.T) {
	ar := anthropicproto.MessageCreateRequest{
		Model:     "claude-3",
		MaxTokens: 100,
		System:    "sys",
		Tools: []anthropicproto.ToolDefinition{
			{
				Name:        "get_weather",
				Description: "Get weather",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
			},
		},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"get_weather"}`),
		Messages: []anthropicproto.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"location": "SF"}}}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny"}}},
		},
	}

	or, err := AnthropicToOpenAIChatRequest(ar)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	switch msgs := or.Messages.(type) {
	case []map[string]any:
		if len(msgs) < 4 {
			t.Fatalf("expected >=4 messages (system,user,assistant,tool), got %d", len(msgs))
		}
	case []any:
		if len(msgs) < 4 {
			t.Fatalf("expected >=4 messages (system,user,assistant,tool), got %d", len(msgs))
		}
	default:
		t.Fatalf("expected messages array, got %T", or.Messages)
	}
	var tc map[string]any
	if err := json.Unmarshal(or.ToolChoice, &tc); err != nil {
		t.Fatalf("tool_choice not json: %v", err)
	}
	if tc["type"] != "function" {
		t.Fatalf("unexpected tool_choice.type: %#v", tc["type"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil || fn["name"] != "get_weather" {
		t.Fatalf("unexpected tool_choice.function: %#v", tc["function"])
	}
	if len(or.Tools) == 0 {
		t.Fatalf("expected openai tools")
	}
}

func TestResponseConversion_StopReasonsAndToolCalls(t *testing.T) {
	ar := AnthropicMessageResponse{
		ID:    "msg_1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-3",
		Content: []map[string]any{
			{"type": "text", "text": "hi"},
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"location": "SF"}},
		},
		StopReason: "tool_use",
		Usage:      AnthropicUsage{InputTokens: 1, OutputTokens: 2},
	}
	oresp := AnthropicResponseToOpenAI(ar)
	if len(oresp.Choices) != 1 {
		t.Fatalf("expected 1 choice")
	}
	if oresp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", oresp.Choices[0].FinishReason)
	}
	if len(oresp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %#v", oresp.Choices[0].Message.ToolCalls)
	}

	back := OpenAIResponseToAnthropic(OpenAIChatCompletionResponse{
		Choices: []OpenAIChatChoice{{
			Index:        0,
			FinishReason: "length",
			Message:      OpenAIChatMessage{Role: "assistant", Content: "x"},
		}},
	}, "claude-3")
	if back.StopReason != "max_tokens" {
		t.Fatalf("expected max_tokens stop_reason, got %q", back.StopReason)
	}
}
