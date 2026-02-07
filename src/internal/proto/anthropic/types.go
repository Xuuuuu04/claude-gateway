package anthropic

import "encoding/json"

type MessageCreateRequest struct {
	Model       string           `json:"model"`
	MaxTokens   int              `json:"max_tokens"`
	Messages    []Message        `json:"messages"`
	System      any              `json:"system,omitempty"`
	Metadata    json.RawMessage  `json:"metadata,omitempty"`
	StopSeqs    []string         `json:"stop_sequences,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
}

type Message struct {
	Role    string       `json:"role"`
	Content MessageBlock `json:"content"`
}

type MessageBlock = any

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

