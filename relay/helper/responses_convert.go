package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/dto"
)

// GeneralToResponses converts a chat/completions style request into a /v1/responses request.
// It keeps common params (model, temperature, top_p, tools/tool_choice, metadata, max_tokens)
// and maps messages/prompt into responses input messages.
func GeneralToResponses(g *dto.GeneralOpenAIRequest) (*dto.OpenAIResponsesRequest, error) {
	if g == nil {
		return nil, errors.New("request is nil")
	}

	// Collect system messages and inject them into the first non-system message as plain text,
	// because Codex `/v1/responses` rejects `role=system` inputs.
	systemTexts := make([]string, 0)
	for _, m := range g.Messages {
		if m.Role != "system" {
			continue
		}

		switch v := m.Content.(type) {
		case string:
			if v != "" {
				systemTexts = append(systemTexts, v)
			}
		case []any:
			for _, item := range v {
				if obj, ok := item.(map[string]any); ok {
					if text, ok := obj["text"].(string); ok && text != "" {
						systemTexts = append(systemTexts, text)
					}
				}
			}
		case map[string]any:
			if text, ok := v["text"].(string); ok && text != "" {
				systemTexts = append(systemTexts, text)
			}
		default:
			// Keep it simple: best-effort stringification
			systemTexts = append(systemTexts, fmt.Sprintf("%v", v))
		}
	}

	systemPayload := strings.Join(systemTexts, "\n\n")
	systemInjected := false

	inputs := make([]map[string]any, 0)

	// Prefer messages
	if len(g.Messages) > 0 {
		for _, m := range g.Messages {
			if m.Role == "system" {
				// Already folded above
				continue
			}

			contentItems := make([]map[string]any, 0)
			// For assistant role, Responses expects output_text instead of input_text
			defaultType := "input_text"
			if m.Role == "assistant" {
				defaultType = "output_text"
			}
			switch v := m.Content.(type) {
			case string:
				contentItems = append(contentItems, map[string]any{"type": defaultType, "text": v})
			case []any:
				for _, item := range v {
					if obj, ok := item.(map[string]any); ok {
						if _, hasType := obj["type"]; hasType {
							contentItems = append(contentItems, obj)
							continue
						}
						// if no type, apply default type wrapper
						if text, ok := obj["text"].(string); ok {
							contentItems = append(contentItems, map[string]any{"type": defaultType, "text": text})
							continue
						}
					}
				}
			case map[string]any:
				if _, hasType := v["type"]; hasType {
					contentItems = append(contentItems, v)
				} else if text, ok := v["text"].(string); ok {
					contentItems = append(contentItems, map[string]any{"type": defaultType, "text": text})
				}
			default:
				contentItems = append(contentItems, map[string]any{"type": defaultType, "text": fmt.Sprintf("%v", v)})
			}
			if len(contentItems) == 0 {
				contentItems = append(contentItems, map[string]any{"type": defaultType, "text": ""})
			}

			// Inject folded system instructions ahead of the first non-system message.
			if !systemInjected && systemPayload != "" {
				contentItems = append([]map[string]any{{"type": "input_text", "text": systemPayload}}, contentItems...)
				systemInjected = true
			}

			inputs = append(inputs, map[string]any{
				"type":    "message",
				"role":    m.Role,
				"content": contentItems,
			})
		}
	}

	// Fallback to prompt if no messages
	if len(inputs) == 0 && g.Prompt != nil {
		switch v := g.Prompt.(type) {
		case string:
			inputs = append(inputs, map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{{
					"type": "input_text",
					"text": v,
				}},
			})
		case []any:
			for _, item := range v {
				inputs = append(inputs, map[string]any{
					"type": "message",
					"role": "user",
					"content": []map[string]any{{
						"type": "input_text",
						"text": fmt.Sprintf("%v", item),
					}},
				})
			}
		default:
			inputs = append(inputs, map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{{
					"type": "input_text",
					"text": fmt.Sprintf("%v", v),
				}},
			})
		}
	}

	// If we only had system messages, still send them as a single user message.
	if len(inputs) == 0 && systemPayload != "" {
		inputs = append(inputs, map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": systemPayload,
			}},
		})
	}

	if len(inputs) == 0 {
		return nil, errors.New("input is empty after conversion")
	}

	marshalOrNil := func(v any) json.RawMessage {
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return b
	}

	// Normalize tools:
	// - If []ToolCallRequest (typed), construct the expected shape with top-level name.
	// - If []any (already map-like), ensure name is copied from function.name when missing.
	normalizeTools := func(raw any) any {
		switch v := raw.(type) {
		case []dto.ToolCallRequest:
			tools := make([]map[string]any, 0, len(v))
			for _, t := range v {
				fn := map[string]any{}
				if t.Function.Name != "" {
					fn["name"] = t.Function.Name
				}
				if t.Function.Description != "" {
					fn["description"] = t.Function.Description
				}
				if t.Function.Parameters != nil {
					fn["parameters"] = t.Function.Parameters
				}
				if t.Function.Arguments != "" {
					fn["arguments"] = t.Function.Arguments
				}

				obj := map[string]any{
					"type":     t.Type,
					"name":     t.Function.Name,
					"function": fn,
				}
				if len(t.Custom) > 0 {
					obj["custom"] = t.Custom
				}
				if t.ID != "" {
					obj["id"] = t.ID
				}

				tools = append(tools, obj)
			}
			return tools

		case []any:
			toolsSlice := v
			for i, t := range toolsSlice {
				obj, ok := t.(map[string]any)
				if !ok {
					continue
				}
				if _, hasName := obj["name"]; hasName {
					continue
				}
				if fn, ok := obj["function"].(map[string]any); ok {
					if fnName, ok := fn["name"].(string); ok && fnName != "" {
						obj["name"] = fnName
						toolsSlice[i] = obj
					}
				}
			}
			return toolsSlice
		default:
			return raw
		}
	}

	respReq := &dto.OpenAIResponsesRequest{
		Model:      g.Model,
		Input:      marshalOrNil(inputs),
		Stream:     g.Stream,
		TopP:       g.TopP,
		ToolChoice: marshalOrNil(g.ToolChoice),
		Tools:      marshalOrNil(normalizeTools(g.Tools)),
		Metadata:   g.Metadata,
		Store:      g.Store,
		User:       g.User,
	}

	if g.MaxCompletionTokens > 0 {
		respReq.MaxOutputTokens = g.MaxCompletionTokens
	} else {
		respReq.MaxOutputTokens = g.MaxTokens
	}

	if g.Temperature != nil {
		respReq.Temperature = *g.Temperature
	}

	if g.ReasoningEffort != "" {
		respReq.Reasoning = &dto.Reasoning{Effort: g.ReasoningEffort}
	}

	return respReq, nil
}
