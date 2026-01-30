package openai

import "encoding/json"

type ChatCompletionsRequest struct {
	Model       string          `json:"model"`
	Messages    any             `json:"messages"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

type ResponsesRequest struct {
	Model  string          `json:"model"`
	Input  any             `json:"input"`
	Stream bool            `json:"stream,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

