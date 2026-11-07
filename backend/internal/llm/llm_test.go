package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"campaigncomposer/internal/segment"
)

var sc = segment.Schema{Cities: []string{"İstanbul", "Ankara"}, Languages: []string{"tr", "en"}}

const exampleBrief = "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."

func TestMockSegmentForExampleBriefPassesValidation(t *testing.T) {
	svc := &Service{Client: &Mock{}}
	res, err := svc.BuildSegment(context.Background(), exampleBrief, sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 1 {
		t.Errorf("want first-try success, attempts=%d", res.Attempts)
	}
	b, _ := json.Marshal(res.Rule)
	for _, want := range []string{"İstanbul", "days_since_last_cart_add", "days_since_last_purchase"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("rule %s missing %s", b, want)
		}
	}
}

func TestSegmentRepairLoopFeedsProblemsBack(t *testing.T) {
	var sawFeedback []string
	m := &Mock{SegmentFn: func(call int, _ SegmentInput, fb *Feedback) (json.RawMessage, error) {
		if call == 1 {
			// hostile / hallucinated output
			return json.RawMessage(`{"rule":{"field":"password","operator":"eq","value":"x"},"explanation":"x"}`), nil
		}
		sawFeedback = fb.Problems
		return json.RawMessage(`{"rule":{"field":"city","operator":"eq","value":"ankara"},"explanation":"Ankara"}`), nil
	}}
	res, err := (&Service{Client: m}).BuildSegment(context.Background(), "x", sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts=%d", res.Attempts)
	}
	if len(sawFeedback) == 0 || !strings.Contains(sawFeedback[0], "password") {
		t.Errorf("repair prompt did not carry the validator problem: %v", sawFeedback)
	}
}

func TestSegmentGivesUpAfterRepairs(t *testing.T) {
	calls := 0
	m := &Mock{SegmentFn: func(int, SegmentInput, *Feedback) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`not json at all`), nil
	}}
	_, err := (&Service{Client: m}).BuildSegment(context.Background(), "x", sc)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
	if calls != MaxRepairs+1 {
		t.Errorf("calls=%d want %d", calls, MaxRepairs+1)
	}
}

func TestSegmentPropagatesClientError(t *testing.T) {
	boom := errors.New("api down")
	m := &Mock{SegmentFn: func(int, SegmentInput, *Feedback) (json.RawMessage, error) { return nil, boom }}
	if _, err := (&Service{Client: m}).BuildSegment(context.Background(), "x", sc); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestComposeRepairsOverlongSMS(t *testing.T) {
	var events []Event
	in := MessageInput{Brief: exampleBrief + " [mock:long]", AudienceSize: 10, MaxLen: map[string]int{"first_name": 12}}
	msgs, attempts, err := (&Service{Client: &Mock{}}).ComposeMessages(context.Background(), in, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts=%d, want 2 (long first, repaired second)", attempts)
	}
	if len([]rune(msgs.SMSText)) > 160 {
		t.Errorf("sms still too long: %d", len([]rune(msgs.SMSText)))
	}
	var retries, partials int
	for _, e := range events {
		switch e.Type {
		case "retry":
			retries++
			if len(e.Problems) == 0 {
				t.Error("retry event without problems")
			}
		case "partial":
			partials++
		}
	}
	if retries != 1 || partials == 0 {
		t.Errorf("events: retries=%d partials=%d", retries, partials)
	}
}

func TestComposeRejectsUnknownKeysAndPlaceholders(t *testing.T) {
	m := &Mock{MessagesFn: func(call int, _ MessageInput, _ *Feedback) (json.RawMessage, error) {
		return json.RawMessage(`{"sms_text":"Hi {{phone}}","email_subject":"s","email_body":"b","extra":"x"}`), nil
	}}
	_, _, err := (&Service{Client: m}).ComposeMessages(context.Background(), MessageInput{}, nil)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("want ErrInvalidOutput, got %v", err)
	}
}

func TestExtractPartial(t *testing.T) {
	cases := []struct {
		in, key, want string
		ok            bool
	}{
		{`{"sms_text":"Merhaba {{first_na`, "sms_text", "Merhaba {{first_na", true},
		{`{"sms_text":"a\"b\nc","email_su`, "sms_text", "a\"b\nc", true},
		{`{"sms_text":"x","email_subject":"Konu \u00fc`, "email_subject", "Konu ü", true},
		{`{"sms_text":"x","email_subject":"Konu \u00f`, "email_subject", "Konu ", true},
		{`{"sms_te`, "sms_text", "", false},
		{`{"sms_text":`, "sms_text", "", false},
	}
	for _, c := range cases {
		got := ExtractPartial(c.in)
		v, ok := got[c.key]
		if ok != c.ok || v != c.want {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", c.in, v, ok, c.want, c.ok)
		}
	}
}

func TestAnthropicClientStreamsToolInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad headers", 401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true || req["tool_choice"] == nil {
			http.Error(w, "bad body", 400)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		frag := []string{`{"sms_text":"Merha`, `ba","email_subject":"S","email_`, `body":"B"}`}
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		for _, f := range frag {
			b, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": f}})
			fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", b)
		}
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	a := NewAnthropic("test-key", "claude-test")
	a.BaseURL = srv.URL
	var deltas int
	raw, err := a.GenerateMessages(context.Background(), MessageInput{MaxLen: map[string]int{}}, nil, func(string) { deltas++ })
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"sms_text":"Merhaba","email_subject":"S","email_body":"B"}` || deltas != 3 {
		t.Fatalf("raw=%s deltas=%d", raw, deltas)
	}
}

func TestAnthropicSegmentParsesToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"},{"type":"tool_use","input":{"rule":{"field":"city","operator":"eq","value":"Ankara"},"explanation":"e"}}]}`)
	}))
	defer srv.Close()
	a := NewAnthropic("k", "m")
	a.BaseURL = srv.URL
	res, err := (&Service{Client: a}).BuildSegment(context.Background(), "b", sc)
	if err != nil || res.Explanation != "e" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestAnthropicHTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "overloaded", 529) }))
	defer srv.Close()
	a := NewAnthropic("k", "m")
	a.BaseURL = srv.URL
	if _, err := a.GenerateSegment(context.Background(), SegmentInput{}, nil); err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("got %v", err)
	}
}
