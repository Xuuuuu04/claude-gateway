package streamconv

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIToAnthropic(t *testing.T) {
	in := strings.Join([]string{
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}",
		"",
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	rr := httptest.NewRecorder()
	if err := OpenAIToAnthropic(rr, strings.NewReader(in), "claude-sonnet-4-5"); err != nil {
		t.Fatalf("OpenAIToAnthropic: %v", err)
	}

	out := rr.Body.String()
	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("missing message_start: %s", out)
	}
	if !strings.Contains(out, "\"text\":\"Hello\"") || !strings.Contains(out, "\"text\":\" world\"") {
		t.Fatalf("missing text deltas: %s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("missing message_stop: %s", out)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	in := strings.Join([]string{
		"event: message_start",
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\"}}",
		"",
		"event: content_block_delta",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}",
		"",
		"event: message_stop",
		"data: {\"type\":\"message_stop\"}",
		"",
	}, "\n")

	rr := httptest.NewRecorder()
	if err := AnthropicToOpenAI(rr, bytes.NewReader([]byte(in)), "gpt-4o"); err != nil {
		t.Fatalf("AnthropicToOpenAI: %v", err)
	}

	out := rr.Body.String()
	if !strings.Contains(out, "\"object\":\"chat.completion.chunk\"") {
		t.Fatalf("missing chunk object: %s", out)
	}
	if !strings.Contains(out, "\"content\":\"Hello\"") {
		t.Fatalf("missing content delta: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing done: %s", out)
	}
}

