package api

import (
	"encoding/json"
	"strings"
)

type OpenAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function OpenAIToolCallFunction `json:"function"`
}

type OpenAIChatMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// openAIChatMessageAlias avoids infinite recursion into UnmarshalJSON below.
type openAIChatMessageAlias OpenAIChatMessage

// UnmarshalJSON accepts content as a plain string (standard OpenAI shape),
// null, or an array of content parts (e.g. Open WebUI's
// [{"type":"text","text":"..."}, {"type":"image_url",...}]). Non-text parts
// (images, etc.) are dropped; text parts are concatenated in order.
func (m *OpenAIChatMessage) UnmarshalJSON(data []byte) error {
	aux := struct {
		Content json.RawMessage `json:"content"`
		*openAIChatMessageAlias
	}{
		openAIChatMessageAlias: (*openAIChatMessageAlias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if len(aux.Content) == 0 || string(aux.Content) == "null" {
		m.Content = nil
		return nil
	}

	var s string
	if err := json.Unmarshal(aux.Content, &s); err == nil {
		m.Content = &s
		return nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(aux.Content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
		joined := sb.String()
		m.Content = &joined
		return nil
	}

	// Unknown shape: leave Content nil rather than failing the whole request.
	m.Content = nil
	return nil
}

type OpenAIChatCompletionRequest struct {
	Model      string              `json:"model"`
	Messages   []OpenAIChatMessage `json:"messages"`
	Tools      []map[string]any    `json:"tools,omitempty"`
	ToolChoice any                 `json:"tool_choice,omitempty"`
	Stream     bool                `json:"stream"`
	User       *string             `json:"user,omitempty"`
}

type OpenAIChoice struct {
	Index        int               `json:"index"`
	Message      OpenAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIChatCompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

type OpenAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int         `json:"index"`
	Delta        OpenAIDelta `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type OpenAIChatCompletionChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
}

type ImageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	UserID         string `json:"user_id"`
}

type ImageData struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

type ImageGenerationResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}
