package convert

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	anthropicproto "claude-gateway/internal/proto/anthropic"
	openaiproto "claude-gateway/internal/proto/openai"
)

func AnthropicToGeminiRequest(ar anthropicproto.MessageCreateRequest) (GeminiGenerateContentRequest, string) {
	sysText := ""
	switch v := ar.System.(type) {
	case string:
		sysText = v
	case nil:
	default:
		b, _ := json.Marshal(v)
		sysText = string(b)
	}

	contents := make([]GeminiContent, 0, len(ar.Messages))
	for _, m := range ar.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, GeminiContent{
			Role:  role,
			Parts: []GeminiPart{{Text: anthropicContentToText(m.Content)}},
		})
	}

	req := GeminiGenerateContentRequest{
		Contents: contents,
		GenerationConfig: &GeminiGenConfig{
			Temperature: ar.Temperature,
			TopP:        ar.TopP,
		},
	}
	if strings.TrimSpace(sysText) != "" {
		req.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: sysText}}}
	}
	return req, ar.Model
}

func OpenAIToGeminiRequest(or openaiproto.ChatCompletionsRequest) (GeminiGenerateContentRequest, string) {
	sysText := ""
	contents := make([]GeminiContent, 0)

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
					sysText += s
				} else {
					b, _ := json.Marshal(m["content"])
					sysText += string(b)
				}
				continue
			}
			gRole := "user"
			if role == "assistant" {
				gRole = "model"
			}
			text := ""
			if s, ok := m["content"].(string); ok {
				text = s
			} else {
				b, _ := json.Marshal(m["content"])
				text = string(b)
			}
			contents = append(contents, GeminiContent{Role: gRole, Parts: []GeminiPart{{Text: text}}})
		}
	}

	req := GeminiGenerateContentRequest{
		Contents: contents,
		GenerationConfig: &GeminiGenConfig{
			Temperature: or.Temperature,
			TopP:        or.TopP,
			MaxTokens:   or.MaxTokens,
		},
	}
	if strings.TrimSpace(sysText) != "" {
		req.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: sysText}}}
	}
	return req, or.Model
}

func GeminiResponseText(gr GeminiGenerateContentResponse) (string, *GeminiUsage) {
	if len(gr.Candidates) == 0 {
		return "", gr.UsageMetadata
	}
	var b strings.Builder
	for _, p := range gr.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String(), gr.UsageMetadata
}

func GeminiTextToOpenAI(text string, model string, usage *GeminiUsage) OpenAIChatCompletionResponse {
	out := OpenAIChatCompletionResponse{
		ID:      "chatcmpl_" + uuid.NewString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChatChoice{{
			Index: 0,
			Message: OpenAIChatMessage{
				Role:    "assistant",
				Content: text,
			},
			FinishReason: "stop",
		}},
	}
	if usage != nil {
		out.Usage = &OpenAIChatUsage{
			PromptTokens:     usage.PromptTokenCount,
			CompletionTokens: usage.CandidatesTokenCount,
			TotalTokens:      usage.TotalTokenCount,
		}
	}
	return out
}

func GeminiTextToAnthropic(text string, model string, usage *GeminiUsage) AnthropicMessageResponse {
	ar := AnthropicMessageResponse{
		ID:         "msg_" + uuid.NewString(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    []map[string]any{{"type": "text", "text": text}},
		StopReason: "end_turn",
		Usage:      AnthropicUsage{},
	}
	if usage != nil {
		ar.Usage.InputTokens = usage.PromptTokenCount
		ar.Usage.OutputTokens = usage.CandidatesTokenCount
	}
	return ar
}

