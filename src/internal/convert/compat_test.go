package convert

import (
	"encoding/json"
	"testing"

	anthropicproto "claude-gateway/src/internal/proto/anthropic"
	openaiproto "claude-gateway/src/internal/proto/openai"
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

func TestAnthropicToOpenAIChatRequest_ImageBlockToImageURL(t *testing.T) {
	ar := anthropicproto.MessageCreateRequest{
		Model:     "claude-3",
		MaxTokens: 64,
		Messages: []anthropicproto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "AA==",
						},
					},
					map[string]any{"type": "text", "text": "describe"},
				},
			},
		},
	}
	or, err := AnthropicToOpenAIChatRequest(ar)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	msgs, ok := or.Messages.([]map[string]any)
	if !ok || len(msgs) < 1 {
		t.Fatalf("expected []map messages, got %T", or.Messages)
	}
	content := msgs[0]["content"]
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("expected content parts array, got %T", content)
	}
	foundImage := false
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["type"] == "image_url" {
			img, _ := m["image_url"].(map[string]any)
			if img != nil && img["url"] == "data:image/png;base64,AA==" {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("expected image_url part, got %#v", parts)
	}
}

func TestOpenAIToAnthropicMessageRequest_ImageURLToImageBlock(t *testing.T) {
	req := openaiproto.ChatCompletionsRequest{
		Model: "gpt-4",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is this"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AA=="}},
				},
			},
		},
	}
	ar, err := OpenAIToAnthropicMessageRequest(req)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if len(ar.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ar.Messages))
	}
	blocks, ok := ar.Messages[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map blocks, got %T", ar.Messages[0].Content)
	}
	found := false
	for _, b := range blocks {
		if b["type"] == "image" {
			src, _ := b["source"].(map[string]any)
			if src != nil && src["type"] == "base64" && src["media_type"] == "image/jpeg" && src["data"] == "AA==" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected image block, got %#v", blocks)
	}
}

func TestAnthropicResponseToOpenAIResponses_Shape(t *testing.T) {
	ar := AnthropicMessageResponse{
		ID:    "msg_1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-3",
		Content: []map[string]any{
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"location": "SF"}},
			{"type": "text", "text": "ok"},
		},
		StopReason: "tool_use",
		Usage:      AnthropicUsage{InputTokens: 3, OutputTokens: 4},
	}
	resp := AnthropicResponseToOpenAIResponses(ar, "gpt-5")
	if resp.Object != "response" {
		t.Fatalf("expected object response, got %q", resp.Object)
	}
	if len(resp.Output) < 1 {
		t.Fatalf("expected output items")
	}
	foundMessage := false
	for _, it := range resp.Output {
		if it.Type == "message" {
			foundMessage = true
			if len(it.Content) != 1 || it.Content[0]["type"] != "output_text" {
				t.Fatalf("unexpected message content: %#v", it.Content)
			}
		}
	}
	if !foundMessage {
		t.Fatalf("expected message item in output, got %#v", resp.Output)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 7 {
		t.Fatalf("unexpected usage: %#v", resp.Usage)
	}
}

func TestOpenAIToAnthropicMessageRequest_ImageURLHttpsToURLSource(t *testing.T) {
	req := openaiproto.ChatCompletionsRequest{
		Model: "gpt-4",
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
				},
			},
		},
	}
	ar, err := OpenAIToAnthropicMessageRequest(req)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	blocks, ok := ar.Messages[0].Content.([]map[string]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %#v", ar.Messages[0].Content)
	}
	if blocks[0]["type"] != "image" {
		t.Fatalf("expected image block, got %#v", blocks[0])
	}
	src, _ := blocks[0]["source"].(map[string]any)
	if src == nil || src["type"] != "url" || src["url"] != "https://example.com/a.png" {
		t.Fatalf("unexpected image source: %#v", src)
	}
}

func TestAnthropicToOpenAIChatRequest_ImageURLSourceHttpsToImageURL(t *testing.T) {
	ar := anthropicproto.MessageCreateRequest{
		Model:     "claude-3",
		MaxTokens: 64,
		Messages: []anthropicproto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "url",
							"url":  "https://example.com/a.png",
						},
					},
				},
			},
		},
	}
	or, err := AnthropicToOpenAIChatRequest(ar)
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	msgs, ok := or.Messages.([]map[string]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %T %#v", or.Messages, or.Messages)
	}
	parts, ok := msgs[0]["content"].([]any)
	if !ok {
		t.Fatalf("expected content parts, got %T", msgs[0]["content"])
	}
	found := false
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["type"] == "image_url" {
			img, _ := m["image_url"].(map[string]any)
			if img != nil && img["url"] == "https://example.com/a.png" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected image_url=https://..., got %#v", parts)
	}
}
