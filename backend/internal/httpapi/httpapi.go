// Package httpapi exposes the campaign workflow over HTTP + Server-Sent Events.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"campaigncomposer/internal/content"
	"campaigncomposer/internal/llm"
	"campaigncomposer/internal/metrics"
	"campaigncomposer/internal/segment"
	"campaigncomposer/internal/store"
)

// Store is the slice of persistence the API needs (the Postgres store, or a fake in tests).
type Store interface {
	Schema(ctx context.Context) (segment.Schema, error)
	AudienceStats(ctx context.Context, rule segment.Node) (store.Audience, error)
	Sample(ctx context.Context, rule segment.Node, n int) ([]map[string]string, error)
	CreateCampaign(ctx context.Context, brief string, rule segment.Node, explanation string, a store.Audience) (store.Campaign, error)
	GetCampaign(ctx context.Context, id string) (store.Campaign, error)
	ListCampaigns(ctx context.Context, limit int) ([]store.Campaign, error)
	SaveContent(ctx context.Context, id string, m content.Messages) error
	Approve(ctx context.Context, id string) (store.ApproveResult, error)
	Stats(ctx context.Context, id string) (map[string]map[string]int, error)
	Ping(ctx context.Context) error
}

type Handler struct {
	Store         Store
	LLM           *llm.Service
	Metrics       *metrics.Registry
	Log           *slog.Logger
	StatsInterval time.Duration
}

const maxBriefRunes = 2000

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("GET /metrics", h.metrics)
	mux.HandleFunc("GET /api/campaigns", h.list)
	mux.HandleFunc("POST /api/campaigns", h.create)
	mux.HandleFunc("GET /api/campaigns/{id}", h.get)
	mux.HandleFunc("POST /api/campaigns/{id}/compose", h.compose)
	mux.HandleFunc("PUT /api/campaigns/{id}/content", h.saveContent)
	mux.HandleFunc("POST /api/campaigns/{id}/approve", h.approve)
	mux.HandleFunc("GET /api/campaigns/{id}/events", h.events)
	return h.recoverer(h.logging(mux))
}

// ---- DTOs --------------------------------------------------------------------

type audienceDTO struct {
	Total int `json:"total"`
	SMS   int `json:"sms"`
	Email int `json:"email"`
}

type sampleDTO struct {
	FirstName string `json:"first_name"`
	SMS       string `json:"sms"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
}

type campaignDTO struct {
	ID          string           `json:"id"`
	Brief       string           `json:"brief"`
	Status      string           `json:"status"`
	Rule        segment.Node     `json:"segment_rule"`
	Explanation string           `json:"segment_explanation"`
	Audience    audienceDTO      `json:"audience"`
	Content     content.Messages `json:"content"`
	Limits      map[string]int   `json:"limits"`
	CreatedAt   time.Time        `json:"created_at"`
	ApprovedAt  *time.Time       `json:"approved_at,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	Samples     []sampleDTO      `json:"samples,omitempty"`
}

func toDTO(c store.Campaign) campaignDTO {
	return campaignDTO{ID: c.ID, Brief: c.Brief, Status: c.Status, Rule: c.Rule, Explanation: c.Explanation,
		Audience: audienceDTO{Total: c.Audience.Total, SMS: c.Audience.SMS, Email: c.Audience.Email},
		Content:  c.Content, Limits: map[string]int{"sms": content.SMSMax, "email_subject": content.SubjectMax},
		CreatedAt: c.CreatedAt, ApprovedAt: c.ApprovedAt, CompletedAt: c.CompletedAt}
}

func (h *Handler) withSamples(ctx context.Context, c store.Campaign) campaignDTO {
	dto := toDTO(c)
	if c.Content.SMSText == "" {
		return dto
	}
	users, err := h.Store.Sample(ctx, c.Rule, 3)
	if err != nil {
		h.log().Warn("sample users", "err", err)
		return dto
	}
	for _, u := range users {
		dto.Samples = append(dto.Samples, sampleDTO{FirstName: u["first_name"], SMS: content.Render(c.Content.SMSText, u),
			Subject: content.Render(c.Content.EmailSubject, u), Body: content.Render(c.Content.EmailBody, u)})
	}
	return dto
}

// ---- handlers ----------------------------------------------------------------

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Brief string `json:"brief"`
	}
	if err := decode(w, r, &in); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	brief := strings.TrimSpace(in.Brief)
	if brief == "" || len([]rune(brief)) > maxBriefRunes {
		writeErr(w, 400, fmt.Sprintf("brief must be between 1 and %d characters", maxBriefRunes))
		return
	}
	sc, err := h.Store.Schema(r.Context())
	if err != nil {
		h.serverErr(w, "schema", err)
		return
	}
	res, err := h.LLM.BuildSegment(r.Context(), brief, sc)
	if err != nil {
		h.llmErr(w, "segment", err)
		return
	}
	aud, err := h.Store.AudienceStats(r.Context(), res.Rule)
	if err != nil {
		h.serverErr(w, "audience", err)
		return
	}
	c, err := h.Store.CreateCampaign(r.Context(), brief, res.Rule, res.Explanation, aud)
	if err != nil {
		h.serverErr(w, "create campaign", err)
		return
	}
	h.count("composer_campaigns_created_total", "")
	writeJSON(w, http.StatusCreated, toDTO(c))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	cs, err := h.Store.ListCampaigns(r.Context(), 20)
	if err != nil {
		h.serverErr(w, "list", err)
		return
	}
	out := make([]campaignDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toDTO(c))
	}
	writeJSON(w, 200, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, h.withSamples(r.Context(), c))
}

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (store.Campaign, bool) {
	c, err := h.Store.GetCampaign(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "campaign not found")
		return c, false
	}
	if err != nil {
		h.serverErr(w, "get campaign", err)
		return c, false
	}
	return c, true
}

// compose streams Claude's copy as Server-Sent Events:
//
//	partial {fields}        text written so far (for the typing effect)
//	retry   {problems}      the draft broke a rule and is being rewritten
//	final   {content,...}   validated copy, already saved on the campaign
//	error   {message}
func (h *Handler) compose(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	if c.Status != store.StatusSegmentReady && c.Status != store.StatusContentReady {
		writeErr(w, 409, "messages can only be (re)generated before the campaign is approved")
		return
	}
	aud, err := h.Store.AudienceStats(r.Context(), c.Rule)
	if err != nil {
		h.serverErr(w, "audience", err)
		return
	}
	if aud.Total == 0 {
		writeErr(w, 422, "the audience is empty; adjust the brief")
		return
	}
	s, ok := newSSE(w)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	msgs, attempts, err := h.LLM.ComposeMessages(r.Context(), llm.MessageInput{Brief: c.Brief, AudienceSize: aud.Total, MaxLen: aud.MaxLen},
		func(e llm.Event) { s.send(e.Type, e) })
	if err != nil {
		h.count("composer_llm_failures_total", `kind="messages"`)
		h.log().Error("compose", "campaign", c.ID, "err", err)
		s.send("error", map[string]string{"message": publicLLMError(err)})
		return
	}
	if err := h.Store.SaveContent(r.Context(), c.ID, msgs); err != nil {
		h.log().Error("save content", "err", err)
		s.send("error", map[string]string{"message": "could not save the generated messages"})
		return
	}
	c.Content = msgs
	c.Status = store.StatusContentReady
	h.count("composer_llm_attempts_total", fmt.Sprintf(`kind="messages",attempt="%d"`, attempts))
	s.send("final", map[string]any{"campaign": h.withSamples(r.Context(), c), "attempts": attempts})
}

func (h *Handler) saveContent(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	var in content.Messages
	if err := decode(w, r, &in); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	in = content.Normalize(in)
	aud, err := h.Store.AudienceStats(r.Context(), c.Rule)
	if err != nil {
		h.serverErr(w, "audience", err)
		return
	}
	if problems := content.Validate(in, aud.MaxLen); len(problems) > 0 {
		writeJSON(w, 422, map[string]any{"error": "content does not meet the channel rules", "problems": problems})
		return
	}
	if err := h.Store.SaveContent(r.Context(), c.ID, in); err != nil {
		if errors.Is(err, store.ErrBadState) {
			writeErr(w, 409, "content can no longer be edited after approval")
			return
		}
		h.serverErr(w, "save content", err)
		return
	}
	c.Content, c.Status = in, store.StatusContentReady
	writeJSON(w, 200, h.withSamples(r.Context(), c))
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	if c.Status == store.StatusContentReady {
		// Last line of defence: re-check the stored copy against the live audience.
		aud, err := h.Store.AudienceStats(r.Context(), c.Rule)
		if err != nil {
			h.serverErr(w, "audience", err)
			return
		}
		if problems := content.Validate(c.Content, aud.MaxLen); len(problems) > 0 {
			writeJSON(w, 422, map[string]any{"error": "content does not meet the channel rules", "problems": problems})
			return
		}
	}
	res, err := h.Store.Approve(r.Context(), c.ID)
	switch {
	case errors.Is(err, store.ErrBadState):
		writeErr(w, 409, "generate and save the messages before approving")
		return
	case err != nil:
		h.serverErr(w, "approve", err)
		return
	}
	if !res.AlreadyApproved {
		h.count("composer_campaigns_approved_total", "")
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": store.StatusSending, "queued": res.Queued, "skipped": res.Skipped, "already_approved": res.AlreadyApproved})
}

// events pushes live delivery counters once per interval until the campaign completes.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	s, ok := newSSE(w)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	interval := h.StatsInterval
	if interval <= 0 {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		stats, err := h.Store.Stats(r.Context(), c.ID)
		if err != nil {
			s.send("error", map[string]string{"message": "could not read stats"})
			return
		}
		cur, err := h.Store.GetCampaign(r.Context(), c.ID)
		if err != nil {
			return
		}
		s.send("stats", map[string]any{"status": cur.Status, "stats": stats})
		if cur.Status == store.StatusCompleted {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.Store.Ping(ctx); err != nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if h.Metrics != nil {
		h.Metrics.WritePrometheus(w)
	}
}

// ---- helpers -----------------------------------------------------------------

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) count(name, labels string) {
	if h.Metrics != nil {
		h.Metrics.Inc(name, labels)
	}
}

func (h *Handler) serverErr(w http.ResponseWriter, what string, err error) {
	h.log().Error(what, "err", err)
	writeErr(w, 500, "internal error")
}

// llmErr distinguishes "the model could not produce valid output" (502, safe to
// show) from other failures.
func (h *Handler) llmErr(w http.ResponseWriter, kind string, err error) {
	h.count("composer_llm_failures_total", fmt.Sprintf(`kind=%q`, kind))
	h.log().Error("llm", "kind", kind, "err", err)
	writeErr(w, http.StatusBadGateway, publicLLMError(err))
}

func publicLLMError(err error) string {
	if errors.Is(err, llm.ErrInvalidOutput) {
		return "The AI could not produce a valid result after several attempts. Please rephrase the brief and try again. (" + err.Error() + ")"
	}
	return "The AI service is unavailable right now. Please try again."
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) (*sse, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sse{w: w, f: f}, true
}

func (s *sse) send(event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	s.f.Flush()
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			h.log().Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (h *Handler) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				h.log().Error("panic", "err", rec)
				writeErr(w, 500, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
