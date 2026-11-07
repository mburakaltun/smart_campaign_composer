// Package llm wraps Claude behind a small interface and, more importantly,
// makes its output trustworthy: every response is parsed as JSON, validated
// against the segment / content rules, and sent back for repair (with the
// concrete problems listed) before the caller ever sees it.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"campaigncomposer/internal/content"
	"campaigncomposer/internal/segment"
)

// Feedback is attached to a retry so the model can see what was wrong.
type Feedback struct {
	Previous string
	Problems []string
}

type SegmentInput struct {
	Brief  string
	Schema segment.Schema
}

type MessageInput struct {
	Brief        string
	AudienceSize int
	MaxLen       map[string]int
}

// Client is the raw model access. Implementations return the tool/JSON payload
// exactly as the model produced it; they do no validation.
type Client interface {
	// GenerateSegment returns {"rule": <Node>, "explanation": "..."}.
	GenerateSegment(ctx context.Context, in SegmentInput, fb *Feedback) (json.RawMessage, error)
	// GenerateMessages returns {"sms_text","email_subject","email_body"}. If
	// onDelta is non-nil it receives raw JSON fragments as they stream in.
	GenerateMessages(ctx context.Context, in MessageInput, fb *Feedback, onDelta func(string)) (json.RawMessage, error)
}

const MaxRepairs = 2

type Event struct {
	Type     string            `json:"type"` // partial | retry
	Fields   map[string]string `json:"fields,omitempty"`
	Problems []string          `json:"problems,omitempty"`
	Attempt  int               `json:"attempt,omitempty"`
}

type Service struct{ Client Client }

type SegmentResult struct {
	Rule        segment.Node
	Explanation string
	Attempts    int
}

var ErrInvalidOutput = errors.New("model output failed validation after repairs")

// BuildSegment asks Claude for a rule and only returns one that passed strict validation.
func (s *Service) BuildSegment(ctx context.Context, brief string, sc segment.Schema) (SegmentResult, error) {
	var fb *Feedback
	var lastProblems []string
	for attempt := 1; attempt <= MaxRepairs+1; attempt++ {
		raw, err := s.Client.GenerateSegment(ctx, SegmentInput{Brief: brief, Schema: sc}, fb)
		if err != nil {
			return SegmentResult{}, err
		}
		var out struct {
			Rule        json.RawMessage `json:"rule"`
			Explanation string          `json:"explanation"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Rule) == 0 {
			lastProblems = []string{`output must be a JSON object like {"rule": {...}, "explanation": "..."}`}
		} else if node, err := segment.Parse(out.Rule, sc); err != nil {
			lastProblems = problemsOf(err)
		} else {
			return SegmentResult{Rule: node, Explanation: strings.TrimSpace(out.Explanation), Attempts: attempt}, nil
		}
		fb = &Feedback{Previous: truncate(string(raw), 2000), Problems: lastProblems}
	}
	return SegmentResult{}, fmt.Errorf("%w: %s", ErrInvalidOutput, strings.Join(lastProblems, "; "))
}

// ComposeMessages streams the first attempt to onEvent; if the result breaks a
// rule (length, placeholders, shape) it is repaired, never silently truncated.
func (s *Service) ComposeMessages(ctx context.Context, in MessageInput, onEvent func(Event)) (content.Messages, int, error) {
	emit := func(e Event) {
		if onEvent != nil {
			onEvent(e)
		}
	}
	var fb *Feedback
	var lastProblems []string
	for attempt := 1; attempt <= MaxRepairs+1; attempt++ {
		var acc strings.Builder
		raw, err := s.Client.GenerateMessages(ctx, in, fb, func(d string) {
			acc.WriteString(d)
			if f := ExtractPartial(acc.String()); len(f) > 0 {
				emit(Event{Type: "partial", Fields: f, Attempt: attempt})
			}
		})
		if err != nil {
			return content.Messages{}, attempt, err
		}
		var m content.Messages
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			lastProblems = []string{`output must be a JSON object with exactly sms_text, email_subject and email_body: ` + err.Error()}
		} else {
			m = content.Normalize(m)
			if lastProblems = content.Validate(m, in.MaxLen); len(lastProblems) == 0 {
				return m, attempt, nil
			}
		}
		fb = &Feedback{Previous: truncate(string(raw), 3000), Problems: lastProblems}
		if attempt <= MaxRepairs {
			emit(Event{Type: "retry", Problems: lastProblems, Attempt: attempt + 1})
		}
	}
	return content.Messages{}, MaxRepairs + 1, fmt.Errorf("%w: %s", ErrInvalidOutput, strings.Join(lastProblems, "; "))
}

func problemsOf(err error) []string {
	var ve *segment.ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	return []string{err.Error()}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ExtractPartial pulls the string values of the three known keys out of an
// incomplete JSON document so the UI can show text as it is being written.
func ExtractPartial(partial string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{"sms_text", "email_subject", "email_body"} {
		if v, ok := extractString(partial, key); ok {
			out[key] = v
		}
	}
	return out
}

func extractString(s, key string) (string, bool) {
	needle := `"` + key + `"`
	i := strings.Index(s, needle)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(needle):]
	j := 0
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\n' || rest[j] == '\t') {
		j++
	}
	if j >= len(rest) || rest[j] != ':' {
		return "", false
	}
	j++
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\n' || rest[j] == '\t') {
		j++
	}
	if j >= len(rest) || rest[j] != '"' {
		return "", false
	}
	j++
	var b strings.Builder
	runes := []rune(rest[j:])
	for k := 0; k < len(runes); k++ {
		c := runes[k]
		switch c {
		case '"':
			return b.String(), true
		case '\\':
			if k+1 >= len(runes) {
				return b.String(), true // incomplete escape at the tail
			}
			k++
			switch runes[k] {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			case 'r':
				b.WriteRune('\r')
			case '"', '\\', '/':
				b.WriteRune(runes[k])
			case 'u':
				if k+4 >= len(runes) {
					return b.String(), true
				}
				var code rune
				if _, err := fmt.Sscanf(string(runes[k+1:k+5]), "%04x", &code); err == nil {
					b.WriteRune(code)
				}
				k += 4
			default:
				b.WriteRune(runes[k])
			}
		default:
			b.WriteRune(c)
		}
	}
	return b.String(), true
}
