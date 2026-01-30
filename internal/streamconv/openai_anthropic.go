package streamconv

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func OpenAIToAnthropic(w http.ResponseWriter, r io.Reader, model string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, r)
		return err
	}

	msgID := "msg_" + uuid.NewString()
	nextIndex := 0
	openBlocks := map[int]bool{}
	toolIndexByID := map[string]int{}
	finishReason := ""

	writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	flusher.Flush()

	br := bufio.NewReader(r)
	for {
		block, err := readSSEBlock(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		data := extractSSEData(block)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		if delta != nil {
			// Handle reasoning content (OpenAI/DeepSeek R1)
			if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
				idx := 0
				if _, opened := openBlocks[idx]; !opened {
					writeAnthropicEvent(w, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]any{
							"type":     "thinking",
							"thinking": "",
						},
					})
					openBlocks[idx] = true
					if nextIndex <= idx {
						nextIndex = idx + 1
					}
				}
				writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": reasoning,
					},
				})
				flusher.Flush()
			}

			// Handle regular content
			if text, ok := delta["content"].(string); ok && text != "" {
				idx := nextIndex
				// If index 0 was used for thinking, use index 1 for text
				if openBlocks[0] {
					idx = 1
				} else {
					idx = 0
				}

				if _, opened := openBlocks[idx]; !opened {
					writeAnthropicEvent(w, "content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]any{
							"type": "text",
							"text": "",
						},
					})
					openBlocks[idx] = true
					if nextIndex <= idx {
						nextIndex = idx + 1
					}
				}
				writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]any{
						"type": "text_delta",
						"text": text,
					},
				})
				flusher.Flush()
			}
			if tcRaw, ok := delta["tool_calls"].([]any); ok && len(tcRaw) > 0 {
				for _, tci := range tcRaw {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					id, _ := tc["id"].(string)
					if strings.TrimSpace(id) == "" {
						continue
					}
					idx, ok := toolIndexByID[id]
					if !ok {
						idx = nextIndex
						nextIndex++
						toolIndexByID[id] = idx
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						writeAnthropicEvent(w, "content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": idx,
							"content_block": map[string]any{
								"type":  "tool_use",
								"id":    id,
								"name":  name,
								"input": map[string]any{},
							},
						})
						openBlocks[idx] = true
						flusher.Flush()
					}
					fn, _ := tc["function"].(map[string]any)
					args, _ := fn["arguments"].(string)
					if args != "" {
						writeAnthropicEvent(w, "content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": idx,
							"delta": map[string]any{
								"type":         "input_json_delta",
								"partial_json": args,
							},
						})
						flusher.Flush()
					}
				}
			}
		}
		if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
			break
		}
	}

	stopReason := "end_turn"
	switch strings.TrimSpace(finishReason) {
	case "tool_calls":
		stopReason = "tool_use"
	case "length":
		stopReason = "max_tokens"
	case "stop":
		stopReason = "end_turn"
	case "":
		stopReason = "end_turn"
	default:
		stopReason = "end_turn"
	}

	for idx := 0; idx < nextIndex; idx++ {
		if !openBlocks[idx] {
			continue
		}
		writeAnthropicEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	}
	writeAnthropicEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason,
		},
	})
	writeAnthropicEvent(w, "message_stop", map[string]any{
		"type": "message_stop",
	})
	flusher.Flush()
	return nil
}

func AnthropicToOpenAI(w http.ResponseWriter, r io.Reader, model string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, r)
		return err
	}

	id := "chatcmpl_" + uuid.NewString()
	created := time.Now().Unix()
	sentRole := false
	finishReason := "stop"
	toolIDsByIndex := map[int]string{}

	br := bufio.NewReader(r)
	for {
		block, err := readSSEBlock(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		data := extractSSEData(block)
		if data == "" {
			continue
		}

		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if !sentRole {
			writeOpenAIChunk(w, map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"role": "assistant"},
				}},
			})
			flusher.Flush()
			sentRole = true
		}

		switch ev["type"] {
		case "content_block_start":
			idx, _ := ev["index"].(float64)
			contentBlock, _ := ev["content_block"].(map[string]any)
			if contentBlock == nil {
				continue
			}
			if contentBlock["type"] == "thinking" {
				// Initialize reasoning_content for OpenAI
				writeOpenAIChunk(w, map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"reasoning_content": ""},
					}},
				})
				flusher.Flush()
			}
			if contentBlock["type"] == "tool_use" {
				idv, _ := contentBlock["id"].(string)
				name, _ := contentBlock["name"].(string)
				if strings.TrimSpace(idv) == "" || strings.TrimSpace(name) == "" {
					continue
				}
				toolIDsByIndex[int(idx)] = idv
				writeOpenAIChunk(w, map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []any{map[string]any{
								"index": int(idx),
								"id":    idv,
								"type":  "function",
								"function": map[string]any{
									"name":      name,
									"arguments": "",
								},
							}},
						},
					}},
				})
				flusher.Flush()
			}
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			switch delta["type"] {
			case "thinking_delta":
				if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
					writeOpenAIChunk(w, map[string]any{
						"id":      id,
						"object":  "chat.completion.chunk",
						"created": created,
						"model":   model,
						"choices": []any{map[string]any{
							"index": 0,
							"delta": map[string]any{"reasoning_content": thinking},
						}},
					})
					flusher.Flush()
				}
			case "text_delta":
				if text, ok := delta["text"].(string); ok && text != "" {
					writeOpenAIChunk(w, map[string]any{
						"id":      id,
						"object":  "chat.completion.chunk",
						"created": created,
						"model":   model,
						"choices": []any{map[string]any{
							"index": 0,
							"delta": map[string]any{"content": text},
						}},
					})
					flusher.Flush()
				}
			case "input_json_delta":
				idx, _ := ev["index"].(float64)
				partial, _ := delta["partial_json"].(string)
				toolID := toolIDsByIndex[int(idx)]
				if strings.TrimSpace(toolID) == "" || partial == "" {
					continue
				}
				writeOpenAIChunk(w, map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []any{map[string]any{
								"index": int(idx),
								"id":    toolID,
								"type":  "function",
								"function": map[string]any{
									"arguments": partial,
								},
							}},
						},
					}},
				})
				flusher.Flush()
			}
		case "message_delta":
			d, _ := ev["delta"].(map[string]any)
			if d == nil {
				continue
			}
			if sr, ok := d["stop_reason"].(string); ok && sr != "" {
				switch sr {
				case "end_turn", "stop_sequence":
					finishReason = "stop"
				case "max_tokens":
					finishReason = "length"
				case "tool_use":
					finishReason = "tool_calls"
				default:
					finishReason = "stop"
				}
			}
		case "message_stop":
			break
		}
	}

	writeOpenAIChunk(w, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
	return nil
}

func writeAnthropicEvent(w http.ResponseWriter, name string, data any) {
	b, _ := json.Marshal(data)
	_, _ = w.Write([]byte("event: " + name + "\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

func writeOpenAIChunk(w http.ResponseWriter, chunk any) {
	b, _ := json.Marshal(chunk)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

func readSSEBlock(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && b.Len() > 0 {
				return b.String(), io.EOF
			}
			return "", err
		}
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
		b.WriteString(line)
	}
}

func extractSSEData(block string) string {
	lines := strings.Split(block, "\n")
	var dataLines []string
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
		}
	}
	return strings.TrimSpace(strings.Join(dataLines, "\n"))
}
