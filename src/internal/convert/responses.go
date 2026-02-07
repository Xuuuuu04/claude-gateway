package convert

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AnthropicMessageResponse struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Role         string              `json:"role"`
	Model        string              `json:"model"`
	Content      []map[string]any    `json:"content"`
	StopReason   string              `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        AnthropicUsage      `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type OpenAIChatCompletionResponse struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"`
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []OpenAIChatChoice       `json:"choices"`
	Usage   *OpenAIChatUsage         `json:"usage,omitempty"`
}

type OpenAIChatChoice struct {
	Index        int               `json:"index"`
	Message      OpenAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason,omitempty"`
}

type OpenAIChatMessage struct {
	Role      string         `json:"role"`
	Content   any            `json:"content"`
	ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function OpenAIFunction  `json:"function"`
}

type OpenAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func AnthropicResponseToOpenAI(ar AnthropicMessageResponse) OpenAIChatCompletionResponse {
	var b strings.Builder
	var toolCalls []OpenAIToolCall
	for _, blk := range ar.Content {
		switch blk["type"] {
		case "text":
			if t, ok := blk["text"].(string); ok {
				b.WriteString(t)
			}
		case "tool_use":
			id, _ := blk["id"].(string)
			name, _ := blk["name"].(string)
			input := blk["input"]
			argBytes, _ := json.Marshal(input)
			if strings.TrimSpace(id) != "" && strings.TrimSpace(name) != "" {
				toolCalls = append(toolCalls, OpenAIToolCall{
					ID:   id,
					Type: "function",
					Function: OpenAIFunction{
						Name:      name,
						Arguments: string(argBytes),
					},
				})
			}
		}
	}

	finish := mapAnthropicStopReasonToOpenAIFinishReason(ar.StopReason)
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	usage := &OpenAIChatUsage{
		PromptTokens:     ar.Usage.InputTokens,
		CompletionTokens: ar.Usage.OutputTokens,
		TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
	}

	msg := OpenAIChatMessage{
		Role:    "assistant",
		Content: b.String(),
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	return OpenAIChatCompletionResponse{
		ID:      "chatcmpl_" + uuid.NewString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []OpenAIChatChoice{{
			Index: 0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: usage,
	}
}

func OpenAIResponseToAnthropic(or OpenAIChatCompletionResponse, model string) AnthropicMessageResponse {
	text := ""
	var toolBlocks []map[string]any
	finish := ""
	if len(or.Choices) > 0 {
		msg := or.Choices[0].Message
		if s, ok := msg.Content.(string); ok {
			text = s
		} else if msg.Content != nil {
			b, _ := json.Marshal(msg.Content)
			text = string(b)
		}
		for _, tc := range msg.ToolCalls {
			var input any
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			} else {
				input = map[string]any{}
			}
			toolBlocks = append(toolBlocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
		finish = or.Choices[0].FinishReason
	}
	usage := AnthropicUsage{}
	if or.Usage != nil {
		usage.InputTokens = or.Usage.PromptTokens
		usage.OutputTokens = or.Usage.CompletionTokens
	}

	contentBlocks := make([]map[string]any, 0, 1+len(toolBlocks))
	if strings.TrimSpace(text) != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}
	contentBlocks = append(contentBlocks, toolBlocks...)

	stopReason := mapOpenAIFinishReasonToAnthropicStopReason(finish, len(toolBlocks) > 0)
	return AnthropicMessageResponse{
		ID:         "msg_" + uuid.NewString(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    contentBlocks,
		StopReason: stopReason,
		Usage:      usage,
	}
}

func mapAnthropicStopReasonToOpenAIFinishReason(sr string) string {
	switch strings.TrimSpace(sr) {
	case "":
		return "stop"
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func mapOpenAIFinishReasonToAnthropicStopReason(fr string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch strings.TrimSpace(fr) {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	case "":
		return "end_turn"
	default:
		return "end_turn"
	}
}
