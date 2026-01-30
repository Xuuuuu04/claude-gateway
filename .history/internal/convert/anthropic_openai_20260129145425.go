package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	anthropicproto "claude-gateway/internal/proto/anthropic"
	openaiproto "claude-gateway/internal/proto/openai"
)

var ErrUnsupportedMessageShape = errors.New("unsupported message shape")
var ErrUnsupportedContentPart = errors.New("unsupported content part")
var ErrInvalidToolArguments = errors.New("invalid tool arguments")

func AnthropicToOpenAIChatRequest(ar anthropicproto.MessageCreateRequest) (openaiproto.ChatCompletionsRequest, error) {
	var systemText string
	switch v := ar.System.(type) {
	case string:
		systemText = v
	case nil:
	default:
		b, _ := json.Marshal(v)
		systemText = string(b)
	}

	outMsgs := make([]map[string]any, 0, len(ar.Messages)+1)
	if strings.TrimSpace(systemText) != "" {
		outMsgs = append(outMsgs, map[string]any{
			"role":    "system",
			"content": systemText,
		})
	}
	for _, m := range ar.Messages {
		msgs, err := anthropicMessageToOpenAIMessages(m)
		if err != nil {
			return openaiproto.ChatCompletionsRequest{}, err
		}
		outMsgs = append(outMsgs, msgs...)
	}

	maxTokens := ar.MaxTokens
	toolDefs, err := anthropicToolsToOpenAIToolsRaw(ar.Tools)
	if err != nil {
		return openaiproto.ChatCompletionsRequest{}, err
	}
	toolChoice, err := anthropicToolChoiceToOpenAIToolChoiceRaw(ar.ToolChoice)
	if err != nil {
		return openaiproto.ChatCompletionsRequest{}, err
	}
	return openaiproto.ChatCompletionsRequest{
		Model:       ar.Model,
		Messages:    outMsgs,
		MaxTokens:   &maxTokens,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stream:      ar.Stream,
		Tools:       toolDefs,
		ToolChoice:  toolChoice,
	}, nil
}

func OpenAIToAnthropicMessageRequest(or openaiproto.ChatCompletionsRequest) (anthropicproto.MessageCreateRequest, error) {
	var sysParts []string
	out := make([]anthropicproto.Message, 0)

	typed, ok := or.Messages.([]any)
	if !ok {
		return anthropicproto.MessageCreateRequest{}, ErrUnsupportedMessageShape
	}
	for _, raw := range typed {
		m, ok := raw.(map[string]any)
		if !ok {
			return anthropicproto.MessageCreateRequest{}, ErrUnsupportedMessageShape
		}
		role, _ := m["role"].(string)
		role = strings.TrimSpace(role)
		if role == "" {
			return anthropicproto.MessageCreateRequest{}, fmt.Errorf("%w: missing role", ErrUnsupportedMessageShape)
		}
		if role == "system" {
			sysParts = append(sysParts, stringifyJSONish(m["content"]))
			continue
		}

		switch role {
		case "user", "assistant":
			contentBlocks, err := openAIMessageContentToAnthropicBlocks(m["content"])
			if err != nil {
				return anthropicproto.MessageCreateRequest{}, err
			}
			toolUseBlocks, err := openAIToolCallsToAnthropicBlocks(m["tool_calls"])
			if err != nil {
				return anthropicproto.MessageCreateRequest{}, err
			}
			blocks := append(contentBlocks, toolUseBlocks...)
			out = append(out, anthropicproto.Message{
				Role:    role,
				Content: blocks,
			})
		case "tool":
			toolCallID, _ := m["tool_call_id"].(string)
			toolCallID = strings.TrimSpace(toolCallID)
			if toolCallID == "" {
				return anthropicproto.MessageCreateRequest{}, fmt.Errorf("%w: missing tool_call_id", ErrUnsupportedMessageShape)
			}
			content := stringifyJSONish(m["content"])
			out = append(out, anthropicproto.Message{
				Role: "user",
				Content: []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": toolCallID,
					"content":     content,
					"is_error":    false,
				}},
			})
		default:
			return anthropicproto.MessageCreateRequest{}, fmt.Errorf("%w: unsupported role %q", ErrUnsupportedMessageShape, role)
		}
	}

	maxTokens := 1024
	if or.MaxTokens != nil && *or.MaxTokens > 0 {
		maxTokens = *or.MaxTokens
	}

	tools, err := openAIToolsToAnthropicTools(or.Tools)
	if err != nil {
		return anthropicproto.MessageCreateRequest{}, err
	}
	toolChoice, err := openAIToolChoiceToAnthropicToolChoice(or.ToolChoice)
	if err != nil {
		return anthropicproto.MessageCreateRequest{}, err
	}

	req := anthropicproto.MessageCreateRequest{
		Model:       or.Model,
		MaxTokens:   maxTokens,
		Messages:    out,
		Temperature: or.Temperature,
		TopP:        or.TopP,
		Stream:      or.Stream,
		Tools:       tools,
		ToolChoice:  toolChoice,
	}
	sys := strings.TrimSpace(strings.Join(sysParts, "\n"))
	if sys != "" {
		req.System = sys
	}
	return req, nil
}

func anthropicMessageToOpenAIMessages(m anthropicproto.Message) ([]map[string]any, error) {
	role := strings.TrimSpace(m.Role)
	if role == "" {
		return nil, fmt.Errorf("%w: anthropic message missing role", ErrUnsupportedMessageShape)
	}

	blocks, err := anthropicContentToBlocks(m.Content)
	if err != nil {
		return nil, err
	}

	var (
		textParts      []string
		reasoningParts []string
		contentParts   []any
		hasNonText     bool
		toolCalls      []map[string]any
		toolMessages   []map[string]any
	)

	for _, blk := range blocks {
		typ, _ := blk["type"].(string)
		switch typ {
		case "text":
			if t, ok := blk["text"].(string); ok && t != "" {
				textParts = append(textParts, t)
				contentParts = append(contentParts, map[string]any{"type": "text", "text": t})
			}
		case "thinking":
			if t, ok := blk["thinking"].(string); ok && t != "" {
				reasoningParts = append(reasoningParts, t)
			}
		case "tool_use":
			id, _ := blk["id"].(string)
			name, _ := blk["name"].(string)
			input := blk["input"]
			if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("%w: tool_use missing id/name", ErrUnsupportedMessageShape)
			}
			argBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argBytes),
				},
			})
		case "tool_result":
			toolUseID, _ := blk["tool_use_id"].(string)
			if strings.TrimSpace(toolUseID) == "" {
				return nil, fmt.Errorf("%w: tool_result missing tool_use_id", ErrUnsupportedMessageShape)
			}
			content := anthropicToolResultContentToText(blk["content"])
			if strings.TrimSpace(content) == "" {
				content = stringifyJSONish(blk["content"])
			}
			toolMessages = append(toolMessages, map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			})
		case "image":
			part, err := anthropicImageBlockToOpenAIContentPart(blk)
			if err != nil {
				return nil, err
			}
			contentParts = append(contentParts, part)
			hasNonText = true
		default:
			return nil, fmt.Errorf("%w: unsupported anthropic block type %q", ErrUnsupportedContentPart, typ)
		}
	}

	var content any
	if hasNonText {
		content = contentParts
	} else {
		content = strings.Join(textParts, "")
	}

	out := make([]map[string]any, 0, 1+len(toolMessages))
	switch role {
	case "user":
		out = append(out, map[string]any{
			"role":    "user",
			"content": content,
		})
	case "assistant":
		msg := map[string]any{
			"role":    "assistant",
			"content": content,
		}
		if len(reasoningParts) > 0 {
			msg["reasoning_content"] = strings.Join(reasoningParts, "")
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
	default:
		return nil, fmt.Errorf("%w: unsupported anthropic message role %q", ErrUnsupportedMessageShape, role)
	}
	out = append(out, toolMessages...)
	return out, nil
}

func anthropicToolResultContentToText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, it := range t {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "text" {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	case []map[string]any:
		var b strings.Builder
		for _, m := range t {
			if m["type"] == "text" {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func anthropicImageBlockToOpenAIContentPart(blk map[string]any) (map[string]any, error) {
	src, _ := blk["source"].(map[string]any)
	if src == nil {
		return nil, fmt.Errorf("%w: image block missing source", ErrUnsupportedContentPart)
	}
	st, _ := src["type"].(string)
	switch strings.TrimSpace(st) {
	case "base64":
		mediaType, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		mediaType = strings.TrimSpace(mediaType)
		data = strings.TrimSpace(data)
		if mediaType == "" || data == "" {
			return nil, fmt.Errorf("%w: base64 image missing media_type/data", ErrUnsupportedContentPart)
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, nil
	case "url":
		u, _ := src["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			return nil, fmt.Errorf("%w: url image missing url", ErrUnsupportedContentPart)
		}
		if !(strings.HasPrefix(u, "data:image/") || strings.HasPrefix(u, "https://")) {
			return nil, fmt.Errorf("%w: image url must be https:// or data:image/*", ErrUnsupportedContentPart)
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": u,
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported image source type %q", ErrUnsupportedContentPart, st)
	}
}

func openAIMessageContentToAnthropicBlocks(content any) ([]map[string]any, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": v}}, nil
	case []any:
		blocks := make([]map[string]any, 0, len(v))
		for _, it := range v {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: content part not object", ErrUnsupportedContentPart)
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text":
				t, _ := m["text"].(string)
				if t == "" {
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			case "image_url":
				img, _ := m["image_url"].(map[string]any)
				u, _ := img["url"].(string)
				if strings.TrimSpace(u) == "" {
					return nil, fmt.Errorf("%w: image_url missing url", ErrUnsupportedContentPart)
				}
				if mediaType, data, ok := parseDataImageURL(u); ok {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": mediaType,
							"data":       data,
						},
					})
					break
				}
				if strings.HasPrefix(strings.TrimSpace(u), "https://") {
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "url",
							"url":  u,
						},
					})
					break
				}
				return nil, fmt.Errorf("%w: image_url must be data:image/*;base64 or https URL", ErrUnsupportedContentPart)
			default:
				return nil, fmt.Errorf("%w: unsupported content part type %q", ErrUnsupportedContentPart, typ)
			}
		}
		return blocks, nil
	default:
		return nil, fmt.Errorf("%w: unsupported OpenAI content type %T", ErrUnsupportedContentPart, v)
	}
}

func openAIToolCallsToAnthropicBlocks(v any) ([]map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: tool_calls is not array", ErrUnsupportedMessageShape)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: tool_call is not object", ErrUnsupportedMessageShape)
		}
		id, _ := m["id"].(string)
		typ, _ := m["type"].(string)
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: tool_call missing id/name", ErrUnsupportedMessageShape)
		}
		if typ != "" && typ != "function" {
			return nil, fmt.Errorf("%w: unsupported tool_call type %q", ErrUnsupportedMessageShape, typ)
		}
		var input any
		if strings.TrimSpace(args) == "" {
			input = map[string]any{}
		} else {
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidToolArguments, err)
			}
		}
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	return out, nil
}

type openAIToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

func openAIToolsToAnthropicTools(raw json.RawMessage) ([]anthropicproto.ToolDefinition, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []openAIToolDef
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("invalid tools: %w", err)
	}
	out := make([]anthropicproto.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("%w: unsupported OpenAI tool type %q", ErrUnsupportedContentPart, t.Type)
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, fmt.Errorf("%w: tool missing name", ErrUnsupportedContentPart)
		}
		out = append(out, anthropicproto.ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out, nil
}

func openAIToolChoiceToAnthropicToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	switch vv := v.(type) {
	case string:
		switch vv {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`), nil
		case "none":
			return json.RawMessage(`{"type":"none"}`), nil
		case "required":
			return json.RawMessage(`{"type":"any"}`), nil
		default:
			return nil, fmt.Errorf("%w: unsupported tool_choice %q", ErrUnsupportedMessageShape, vv)
		}
	case map[string]any:
		t, _ := vv["type"].(string)
		switch t {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`), nil
		case "none":
			return json.RawMessage(`{"type":"none"}`), nil
		case "required":
			return json.RawMessage(`{"type":"any"}`), nil
		case "function":
			fn, _ := vv["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("%w: tool_choice.function missing name", ErrUnsupportedMessageShape)
			}
			b, _ := json.Marshal(map[string]any{"type": "tool", "name": name})
			return b, nil
		default:
			return nil, fmt.Errorf("%w: unsupported tool_choice type %q", ErrUnsupportedMessageShape, t)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported tool_choice shape", ErrUnsupportedMessageShape)
	}
}

func anthropicToolsToOpenAIToolsRaw(tools []anthropicproto.ToolDefinition) (json.RawMessage, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("%w: anthropic tool missing name", ErrUnsupportedMessageShape)
		}
		var params any
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &params); err != nil {
				return nil, fmt.Errorf("invalid anthropic tool input_schema: %w", err)
			}
		} else {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	b, _ := json.Marshal(out)
	return b, nil
}

func anthropicToolChoiceToOpenAIToolChoiceRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid anthropic tool_choice: %w", err)
	}
	typ, _ := v["type"].(string)
	switch typ {
	case "auto":
		return json.RawMessage(`"auto"`), nil
	case "none":
		return json.RawMessage(`"none"`), nil
	case "any":
		return json.RawMessage(`"required"`), nil
	case "tool":
		name, _ := v["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: tool_choice.tool missing name", ErrUnsupportedMessageShape)
		}
		b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": name}})
		return b, nil
	default:
		return nil, fmt.Errorf("%w: unsupported anthropic tool_choice type %q", ErrUnsupportedMessageShape, typ)
	}
}

func anthropicContentToBlocks(content any) ([]map[string]any, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": v}}, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, it := range v {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: anthropic content block not object", ErrUnsupportedMessageShape)
			}
			out = append(out, m)
		}
		return out, nil
	case []map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("%w: unsupported anthropic content type %T", ErrUnsupportedContentPart, v)
	}
}

func stringifyJSONish(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func parseDataImageURL(u string) (mediaType string, data string, ok bool) {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "data:image/") {
		return "", "", false
	}
	parts := strings.SplitN(u, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	meta := parts[0]
	data = parts[1]
	if !strings.Contains(meta, ";base64") {
		return "", "", false
	}
	mt := strings.TrimPrefix(meta, "data:")
	mt = strings.TrimSuffix(mt, ";base64")
	mt = strings.TrimSpace(mt)
	if mt == "" || data == "" {
		return "", "", false
	}
	return mt, data, true
}
