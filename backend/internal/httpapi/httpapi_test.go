package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"campaigncomposer/internal/content"
	"campaigncomposer/internal/llm"
	"campaigncomposer/internal/metrics"
	"campaigncomposer/internal/segment"
	"campaigncomposer/internal/store"
)

// fakeStore is a small in-memory Store: enough to drive the workflow state machine.
type fakeStore struct {
	mu        sync.Mutex
	campaigns map[string]*store.Campaign
	n         int
	audience  store.Audience
	approved  int
	pingErr   error
}

func newFake() *fakeStore {
	return &fakeStore{campaigns: map[string]*store.Campaign{},
		audience: store.Audience{Total: 100, SMS: 70, Email: 85, MaxLen: map[string]int{"first_name": 10, "last_name": 12, "city": 9}}}
}

func (f *fakeStore) Schema(context.Context) (segment.Schema, error) {
	return segment.Schema{Cities: []string{"İstanbul", "Ankara"}, Languages: []string{"tr", "en"}}, nil
}
func (f *fakeStore) AudienceStats(context.Context, segment.Node) (store.Audience, error) {
	return f.audience, nil
}
func (f *fakeStore) Sample(context.Context, segment.Node, int) ([]map[string]string, error) {
	return []map[string]string{{"first_name": "Ayşe", "last_name": "Kaya", "city": "İstanbul"}}, nil
}
func (f *fakeStore) CreateCampaign(_ context.Context, brief string, rule segment.Node, ex string, a store.Audience) (store.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.n)
	c := &store.Campaign{ID: id, Brief: brief, Status: store.StatusSegmentReady, Rule: rule, Explanation: ex, Audience: a, CreatedAt: time.Now()}
	f.campaigns[id] = c
	return *c, nil
}
func (f *fakeStore) GetCampaign(_ context.Context, id string) (store.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.campaigns[id]
	if !ok {
		return store.Campaign{}, store.ErrNotFound
	}
	return *c, nil
}
func (f *fakeStore) ListCampaigns(context.Context, int) ([]store.Campaign, error) { return nil, nil }
func (f *fakeStore) SaveContent(_ context.Context, id string, m content.Messages) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.campaigns[id]
	if c.Status != store.StatusSegmentReady && c.Status != store.StatusContentReady {
		return store.ErrBadState
	}
	c.Content, c.Status = m, store.StatusContentReady
	return nil
}
func (f *fakeStore) Approve(_ context.Context, id string) (store.ApproveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.campaigns[id]
	switch c.Status {
	case store.StatusSending, store.StatusCompleted:
		return store.ApproveResult{AlreadyApproved: true}, nil
	case store.StatusContentReady:
		c.Status = store.StatusSending
		f.approved++
		return store.ApproveResult{Queued: 155}, nil
	}
	return store.ApproveResult{}, store.ErrBadState
}
func (f *fakeStore) Stats(context.Context, string) (map[string]map[string]int, error) {
	return map[string]map[string]int{"sms": {"sent": 5}, "email": {}}, nil
}
func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func newServer(t *testing.T, client llm.Client) (*httptest.Server, *fakeStore) {
	t.Helper()
	fs := newFake()
	h := &Handler{Store: fs, LLM: &llm.Service{Client: client}, Metrics: metrics.New(), StatsInterval: 5 * time.Millisecond}
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, fs
}

func do(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

const brief = "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder."

func createCampaign(t *testing.T, srv *httptest.Server) string {
	resp, out := do(t, "POST", srv.URL+"/api/campaigns", fmt.Sprintf(`{"brief":%q}`, brief))
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	return out["id"].(string)
}

func mustResp(t *testing.T) func(*http.Response, error) *http.Response {
	return func(r *http.Response, err error) *http.Response {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

type sseEvent struct{ Name, Data string }

func readSSE(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	var evs []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.Name != "" {
				evs = append(evs, cur)
			}
			cur = sseEvent{}
		}
	}
	return evs
}

func TestFullWorkflow(t *testing.T) {
	srv, fs := newServer(t, &llm.Mock{})
	id := createCampaign(t, srv)

	// compose streams partial events then a validated final
	resp, err := http.Post(srv.URL+"/api/campaigns/"+id+"/compose", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	evs := readSSE(t, resp)
	partials := 0
	var final sseEvent
	for _, e := range evs {
		if e.Name == "partial" {
			partials++
		}
		if e.Name == "final" {
			final = e
		}
	}
	if partials < 2 || final.Name == "" {
		t.Fatalf("expected streamed partials and a final event, got %d events", len(evs))
	}
	if !strings.Contains(final.Data, "{{first_name}}") {
		t.Errorf("final should carry the template: %s", final.Data)
	}

	// edit: over-limit SMS is rejected with the concrete problem
	long := strings.Repeat("a", 161)
	r, out := do(t, "PUT", srv.URL+"/api/campaigns/"+id+"/content", fmt.Sprintf(`{"sms_text":%q,"email_subject":"s","email_body":"b"}`, long))
	if r.StatusCode != 422 || out["problems"] == nil {
		t.Fatalf("want 422 with problems, got %d %v", r.StatusCode, out)
	}
	// a valid edit is accepted and previewed
	r, out = do(t, "PUT", srv.URL+"/api/campaigns/"+id+"/content", `{"sms_text":"Merhaba {{ first_name }}","email_subject":"Konu","email_body":"Gövde"}`)
	if r.StatusCode != 200 {
		t.Fatalf("edit: %d %v", r.StatusCode, out)
	}
	if got := out["content"].(map[string]any)["sms_text"]; got != "Merhaba {{first_name}}" {
		t.Errorf("placeholder not normalised: %v", got)
	}

	// approve is idempotent
	for i := 0; i < 2; i++ {
		r, out = do(t, "POST", srv.URL+"/api/campaigns/"+id+"/approve", "")
		if r.StatusCode != 202 {
			t.Fatalf("approve #%d: %d %v", i, r.StatusCode, out)
		}
	}
	if fs.approved != 1 {
		t.Errorf("campaign enqueued %d times", fs.approved)
	}
	// content is frozen after approval
	r, _ = do(t, "PUT", srv.URL+"/api/campaigns/"+id+"/content", `{"sms_text":"x","email_subject":"s","email_body":"b"}`)
	if r.StatusCode != 409 {
		t.Errorf("edit after approval: %d", r.StatusCode)
	}
}

func TestApproveBeforeContentIsRejected(t *testing.T) {
	srv, _ := newServer(t, &llm.Mock{})
	id := createCampaign(t, srv)
	if r, _ := do(t, "POST", srv.URL+"/api/campaigns/"+id+"/approve", ""); r.StatusCode != 409 {
		t.Fatalf("got %d", r.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv, _ := newServer(t, &llm.Mock{})
	for name, body := range map[string]string{"empty": `{"brief":"  "}`, "bad json": `nope`, "too long": fmt.Sprintf(`{"brief":%q}`, strings.Repeat("a", 2001))} {
		if r, _ := do(t, "POST", srv.URL+"/api/campaigns", body); r.StatusCode != 400 {
			t.Errorf("%s: got %d", name, r.StatusCode)
		}
	}
}

func TestHostileModelOutputNeverReachesTheDatabase(t *testing.T) {
	bad := &llm.Mock{SegmentFn: func(int, llm.SegmentInput, *llm.Feedback) (json.RawMessage, error) {
		return json.RawMessage(`{"rule":{"field":"1=1; DROP TABLE users","operator":"eq","value":"x"},"explanation":"x"}`), nil
	}}
	srv, fs := newServer(t, bad)
	r, out := do(t, "POST", srv.URL+"/api/campaigns", fmt.Sprintf(`{"brief":%q}`, brief))
	if r.StatusCode != 502 {
		t.Fatalf("want 502, got %d %v", r.StatusCode, out)
	}
	if len(fs.campaigns) != 0 {
		t.Fatal("an invalid rule must not create a campaign")
	}
}

func TestComposeFailureIsReportedOnTheStream(t *testing.T) {
	bad := &llm.Mock{MessagesFn: func(int, llm.MessageInput, *llm.Feedback) (json.RawMessage, error) {
		return json.RawMessage(`{"sms_text":"` + strings.Repeat("x", 300) + `","email_subject":"s","email_body":"b"}`), nil
	}}
	srv, fs := newServer(t, bad)
	id := createCampaign(t, srv)
	resp := mustResp(t)(http.Post(srv.URL+"/api/campaigns/"+id+"/compose", "application/json", nil))
	defer resp.Body.Close()
	evs := readSSE(t, resp)
	if evs[len(evs)-1].Name != "error" {
		t.Fatalf("last event should be error, got %+v", evs[len(evs)-1])
	}
	retries := 0
	for _, e := range evs {
		if e.Name == "retry" {
			retries++
		}
	}
	if retries != llm.MaxRepairs {
		t.Errorf("retries=%d want %d", retries, llm.MaxRepairs)
	}
	if fs.campaigns[id].Content.SMSText != "" {
		t.Error("invalid copy must not be saved")
	}
}

func TestEmptyAudienceCannotBeComposed(t *testing.T) {
	srv, fs := newServer(t, &llm.Mock{})
	id := createCampaign(t, srv)
	fs.audience.Total = 0
	resp := mustResp(t)(http.Post(srv.URL+"/api/campaigns/"+id+"/compose", "application/json", nil))
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestNotFoundAndOps(t *testing.T) {
	srv, fs := newServer(t, &llm.Mock{})
	if r, _ := do(t, "GET", srv.URL+"/api/campaigns/not-a-uuid", ""); r.StatusCode != 404 {
		t.Errorf("bad id: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/healthz", ""); r.StatusCode != 200 {
		t.Errorf("healthz: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/readyz", ""); r.StatusCode != 200 {
		t.Errorf("readyz: %d", r.StatusCode)
	}
	fs.pingErr = fmt.Errorf("down")
	if r, _ := do(t, "GET", srv.URL+"/readyz", ""); r.StatusCode != 503 {
		t.Errorf("readyz with db down: %d", r.StatusCode)
	}
	createCampaign(t, srv)
	resp := mustResp(t)(http.Get(srv.URL + "/metrics"))
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	found := false
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "composer_campaigns_created_total 1") {
			found = true
		}
	}
	if !found {
		t.Error("metrics should expose composer_campaigns_created_total")
	}
}

func TestEventsStreamEndsWhenCampaignCompletes(t *testing.T) {
	srv, fs := newServer(t, &llm.Mock{})
	id := createCampaign(t, srv)
	fs.mu.Lock()
	fs.campaigns[id].Status = store.StatusCompleted
	fs.mu.Unlock()
	resp := mustResp(t)(http.Get(srv.URL + "/api/campaigns/" + id + "/events"))
	defer resp.Body.Close()
	evs := readSSE(t, resp)
	if len(evs) != 1 || evs[0].Name != "stats" || !strings.Contains(evs[0].Data, `"completed"`) {
		t.Fatalf("events: %+v", evs)
	}
}
