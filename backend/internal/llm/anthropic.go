package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic talks to the Messages API over plain HTTP (no SDK dependency).
// Structured output is forced with tool use, so the model must answer with a
// JSON object matching the tool's input schema.
type Anthropic struct {
	APIKey  string
	Model   string
	BaseURL string // default https://api.anthropic.com
	HTTP    *http.Client
}

func NewAnthropic(apiKey, model string) *Anthropic {
	return &Anthropic{APIKey: apiKey, Model: model, BaseURL: "https://api.anthropic.com", HTTP: &http.Client{Timeout: 90 * time.Second}}
}

var segmentTool = map[string]any{
	"name":        "create_segment",
	"description": "Return the audience rule for the campaign brief.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rule":        map[string]any{"type": "object", "description": "Condition tree, see system prompt."},
			"explanation": map[string]any{"type": "string"},
		},
		"required": []string{"rule", "explanation"},
	},
}

var messagesTool = map[string]any{
	"name":        "write_messages",
	"description": "Return the SMS and email copy.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sms_text":      map[string]any{"type": "string"},
			"email_subject": map[string]any{"type": "string"},
			"email_body":    map[string]any{"type": "string"},
		},
		"required": []string{"sms_text", "email_subject", "email_body"},
	},
}

func (a *Anthropic) GenerateSegment(ctx context.Context, in SegmentInput, fb *Feedback) (json.RawMessage, error) {
	body := a.request(SegmentSystemPrompt(in.Schema), SegmentUserPrompt(in.Brief, fb), segmentTool, false)
	resp, err := a.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	for _, c := range out.Content {
		if c.Type == "tool_use" {
			return c.Input, nil
		}
	}
	return nil, fmt.Errorf("anthropic returned no tool_use block")
}

func (a *Anthropic) GenerateMessages(ctx context.Context, in MessageInput, fb *Feedback, onDelta func(string)) (json.RawMessage, error) {
	body := a.request(MessagesSystemPrompt(in.MaxLen), MessagesUserPrompt(in, fb), messagesTool, true)
	resp, err := a.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var acc strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "input_json_delta" {
				acc.WriteString(ev.Delta.PartialJSON)
				if onDelta != nil {
					onDelta(ev.Delta.PartialJSON)
				}
			}
		case "error":
			return nil, fmt.Errorf("anthropic stream error: %s: %s", ev.Error.Type, ev.Error.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if acc.Len() == 0 {
		return nil, fmt.Errorf("anthropic stream produced no tool input")
	}
	return json.RawMessage(acc.String()), nil
}

func (a *Anthropic) request(system, user string, tool map[string]any, stream bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":       a.Model,
		"max_tokens":  1500,
		"stream":      stream,
		"system":      system,
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "tool", "name": tool["name"]},
		"messages":    []any{map[string]any{"role": "user", "content": user}},
	})
	return b
}

func (a *Anthropic) do(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("anthropic API %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}
