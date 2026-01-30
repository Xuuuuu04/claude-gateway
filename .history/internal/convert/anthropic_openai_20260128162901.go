package convert

import (
	"encoding/json"
	"strings"

	anthropicproto "claude-gateway/internal/proto/anthropic"
	openaiproto "claude-gateway/internal/proto/openai"
)

func AnthropicToOpenAIChatRequest(ar anthropicproto.MessageCreateRequest) openaiproto.ChatCompletionsRequest {
	var systemText string
	switch v := ar.System.(type) {
	case string:
		systemText = v
	case nil:
	default:
		b, _ := json.Marshal(v)
		systemText = string(b)
	}

	msgs := make([]map[string]any, 0, len(ar.Messages)+1)
	if strings.TrimSpace(systemText) != "" {
		msgs = append(msgs, map[string]any{
			"role":    "system",
			"content": systemText,
		})
	}
	for _, m := range ar.Messages {
		msgs = append(msgs, map[string]any{
			"role":    m.Role,
			"content": anthropicContentToText(m.Content),
		})
	}

	maxTokens := ar.MaxTokens
	return openaiproto.ChatCompletionsRequest{
		Model:       ar.Model,
		Messages:    msgs,
		MaxTokens:   &maxTokens,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stream:      ar.Stream,
	}
}

func OpenAIToAnthropicMessageRequest(or openaiproto.ChatCompletionsRequest) anthropicproto.MessageCreateRequest {
	sys := ""
	msgs := make([]anthropicproto.Message, 0)

	typed, ok := or.Messages.([]any)
	if ok {
		for _, raw := range typed {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" {
				if s, ok := m["content"].(string); ok {
					sys += s
				} else {
					b, _ := json.Marshal(m["content"])
					sys += string(b)
				}
				continue
			}
			msgs = append(msgs, anthropicproto.Message{
				Role:    role,
				Content: openAIContentToAnthropicBlocks(m["content"]),
			})
		}
	}

	maxTokens := 1024
	if or.MaxTokens != nil && *or.MaxTokens > 0 {
		maxTokens = *or.MaxTokens
	}

	req := anthropicproto.MessageCreateRequest{
		Model:       or.Model,
		MaxTokens:   maxTokens,
		Messages:    msgs,
		Temperature: or.Temperature,
		TopP:        or.TopP,
		Stream:      or.Stream,
	}
	if strings.TrimSpace(sys) != "" {
		req.System = sys
	}
	return req
}

func anthropicContentToText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		j, _ := json.Marshal(v)
		return string(j)
	}
}

func openAIContentToAnthropicBlocks(content any) any {
	switch v := content.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": v}}
	default:
		j, _ := json.Marshal(v)
		return []map[string]any{{"type": "text", "text": string(j)}}
	}
}
