package streamconv

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicToOpenAI_ToolUseStreaming(t *testing.T) {
	in := strings.Join([]string{
		"event: message_start",
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}",
		"",
		"event: content_block_start",
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}",
		"",
		"event: content_block_delta",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"location\\\":\\\"SF\\\"}\"}}",
		"",
		"event: message_delta",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}",
		"",
		"event: message_stop",
		"data: {\"type\":\"message_stop\"}",
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	if err := AnthropicToOpenAI(rec, bytes.NewReader([]byte(in)), "gpt-4"); err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"get_weather"`) {
		t.Fatalf("expected tool_calls in output, got: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason tool_calls, got: %s", out)
	}
}

func TestOpenAIToAnthropic_ToolCallsStreaming(t *testing.T) {
	in := strings.Join([]string{
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}",
		"",
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}",
		"",
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"arguments\":\"{\\\\\\\"location\\\\\\\":\\\\\\\"SF\\\\\\\"}\"}}]}}]}",
		"",
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	if err := OpenAIToAnthropic(rec, bytes.NewReader([]byte(in)), "claude-3"); err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("expected tool_use in output, got: %s", out)
	}
	if !strings.Contains(out, `"input_json_delta"`) || !strings.Contains(out, `"partial_json"`) {
		t.Fatalf("expected input_json_delta in output, got: %s", out)
	}
}

