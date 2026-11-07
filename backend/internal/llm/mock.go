package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// Mock is a deterministic stand-in for Claude. It is used by the tests (no API
// key needed) and by `docker compose up` when no ANTHROPIC_API_KEY is set.
// Optional hooks let tests script bad outputs to exercise the repair loop.
type Mock struct {
	Delay      time.Duration // pause between streamed chunks
	SegmentFn  func(call int, in SegmentInput, fb *Feedback) (json.RawMessage, error)
	MessagesFn func(call int, in MessageInput, fb *Feedback) (json.RawMessage, error)

	segCalls, msgCalls atomic.Int32 // the API shares one Mock across concurrent requests
}

var (
	daysRe     = regexp.MustCompile(`(?i)son\s+(\d+)\s*g[üu]n`)
	discountRe = regexp.MustCompile(`%\s*(\d+)|(\d+)\s*%`)
)

func (m *Mock) GenerateSegment(_ context.Context, in SegmentInput, fb *Feedback) (json.RawMessage, error) {
	call := int(m.segCalls.Add(1))
	if m.SegmentFn != nil {
		return m.SegmentFn(call, in, fb)
	}
	brief := turkishLower(in.Brief)
	var conds []map[string]any
	for _, c := range in.Schema.Cities {
		if strings.Contains(brief, turkishLower(c)) {
			conds = append(conds, map[string]any{"field": "city", "operator": "eq", "value": c})
			break
		}
	}
	days := 30
	if mm := daysRe.FindStringSubmatch(brief); mm != nil {
		fmt.Sscanf(mm[1], "%d", &days)
	}
	if strings.Contains(brief, "sepet") {
		conds = append(conds, map[string]any{"field": "days_since_last_cart_add", "operator": "lte", "value": days})
		if strings.Contains(brief, "yapmayan") || strings.Contains(brief, "almayan") {
			conds = append(conds, map[string]any{"op": "or", "children": []map[string]any{
				{"field": "days_since_last_purchase", "operator": "is_null"},
				{"field": "days_since_last_purchase", "operator": "gt", "value": days},
			}})
		}
	}
	if len(conds) == 0 {
		conds = append(conds, map[string]any{"field": "purchase_count", "operator": "gte", "value": 0})
	}
	return json.Marshal(map[string]any{
		"rule":        map[string]any{"op": "and", "children": conds},
		"explanation": "Mock: brief'ten çıkarılan kitle kuralı.",
	})
}

func (m *Mock) GenerateMessages(ctx context.Context, in MessageInput, fb *Feedback, onDelta func(string)) (json.RawMessage, error) {
	call := int(m.msgCalls.Add(1))
	var raw json.RawMessage
	var err error
	if m.MessagesFn != nil {
		raw, err = m.MessagesFn(call, in, fb)
	} else {
		raw, err = m.defaultMessages(in, fb)
	}
	if err != nil {
		return nil, err
	}
	if onDelta != nil {
		runes := []rune(string(raw))
		for i := 0; i < len(runes); i += 6 {
			end := i + 6
			if end > len(runes) {
				end = len(runes)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			onDelta(string(runes[i:end]))
			if m.Delay > 0 {
				time.Sleep(m.Delay)
			}
		}
	}
	return raw, nil
}

// defaultMessages deliberately overshoots the SMS limit on the first call when
// the brief contains "[mock:long]", so the repair loop can be demonstrated.
func (m *Mock) defaultMessages(in MessageInput, fb *Feedback) (json.RawMessage, error) {
	discount := "özel"
	if mm := discountRe.FindStringSubmatch(in.Brief); mm != nil {
		d := mm[1]
		if d == "" {
			d = mm[2]
		}
		discount = "%" + d
	}
	sms := fmt.Sprintf("Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! %s indirim için bugün göz at.", discount)
	if strings.Contains(in.Brief, "[mock:long]") && fb == nil {
		sms = strings.Repeat("Sepetindeki ürünler seni bekliyor! ", 6) + "{{first_name}}"
	}
	return json.Marshal(map[string]string{
		"sms_text":      sms,
		"email_subject": fmt.Sprintf("{{first_name}}, sepetin seni bekliyor: %s indirim", discount),
		"email_body":    fmt.Sprintf("Merhaba {{first_name}},\n\nSepetine eklediğin ürünleri unutmadık. %s indirim fırsatı seni bekliyor.\n\nAlışverişini tamamlamak için hemen göz at.\n\nSevgiler", discount),
	})
}

func turkishLower(s string) string {
	return strings.ToLowerSpecial(unicode.TurkishCase, s)
}
