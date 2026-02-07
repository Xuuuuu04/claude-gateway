package convert

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type OpenAIResponsesResponse struct {
	ID        string                   `json:"id"`
	Object    string                   `json:"object"`
	CreatedAt int64                    `json:"created_at"`
	Model     string                   `json:"model"`
	Output    []OpenAIResponsesItem    `json:"output"`
	Usage     *OpenAIResponsesUsage    `json:"usage,omitempty"`
}

type OpenAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type OpenAIResponsesItem struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Status  string                 `json:"status,omitempty"`
	Role    string                 `json:"role,omitempty"`
	Content []map[string]any       `json:"content,omitempty"`

	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func AnthropicResponseToOpenAIResponses(ar AnthropicMessageResponse, model string) OpenAIResponsesResponse {
	var (
		textParts []string
		toolCalls []OpenAIResponsesItem
	)

	for _, blk := range ar.Content {
		switch blk["type"] {
		case "text":
			if t, ok := blk["text"].(string); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			callID, _ := blk["id"].(string)
			name, _ := blk["name"].(string)
			input := blk["input"]
			if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" {
				continue
			}
			argBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, OpenAIResponsesItem{
				ID:        "fc_" + uuid.NewString(),
				Type:      "function_call",
				CallID:    callID,
				Name:      name,
				Arguments: string(argBytes),
			})
		}
	}

	out := make([]OpenAIResponsesItem, 0, 1+len(toolCalls))
	out = append(out, toolCalls...)

	text := strings.Join(textParts, "")
	if strings.TrimSpace(text) != "" || len(toolCalls) == 0 {
		out = append(out, OpenAIResponsesItem{
			ID:     "msg_" + uuid.NewString(),
			Type:   "message",
			Status: "completed",
			Role:   "assistant",
			Content: []map[string]any{{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}

	var usage *OpenAIResponsesUsage
	if ar.Usage.InputTokens != 0 || ar.Usage.OutputTokens != 0 {
		usage = &OpenAIResponsesUsage{
			InputTokens:  ar.Usage.InputTokens,
			OutputTokens: ar.Usage.OutputTokens,
			TotalTokens:  ar.Usage.InputTokens + ar.Usage.OutputTokens,
		}
	}

	return OpenAIResponsesResponse{
		ID:        "resp_" + uuid.NewString(),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     model,
		Output:    out,
		Usage:     usage,
	}
}

