package canonical

import "encoding/json"

type Facade string

const (
	FacadeAnthropic Facade = "anthropic"
	FacadeOpenAI    Facade = "openai"
)

type ContextKey string

const ContextKeyClientKey ContextKey = "client_key"

type Request struct {
	Facade Facade
	Model  string
	Stream bool

	System   string
	Messages []Message

	MaxTokens int

	Temperature *float64
	TopP        *float64

	Tools      []Tool
	ToolChoice json.RawMessage

	Raw json.RawMessage
}

type Message struct {
	Role    string
	Content []ContentBlock
}

type ContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

