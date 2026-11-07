// Package provider is the HTTP client for the (mock) SMS / email gateway.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"campaigncomposer/internal/sender"
)

type HTTP struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTP(baseURL string) *HTTP {
	return &HTTP{BaseURL: strings.TrimRight(baseURL, "/"), Client: &http.Client{}}
}

type request struct {
	To         string `json:"to"`
	Subject    string `json:"subject,omitempty"`
	Body       string `json:"body"`
	CampaignID string `json:"campaign_id"`
}

type response struct {
	ID string `json:"id"`
}

// Send posts one message. The Idempotency-Key header is the message's stable
// key, so the gateway can recognise a retry of a request it already processed.
func (h *HTTP) Send(ctx context.Context, m sender.Message) (string, error) {
	body, _ := json.Marshal(request{To: m.Recipient, Subject: m.Subject, Body: m.Body, CampaignID: m.CampaignID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+"/v1/"+m.Channel, bytes.NewReader(body))
	if err != nil {
		return "", &sender.ProviderError{Status: 400, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	resp, err := h.Client.Do(req)
	if err != nil {
		return "", err // network error / timeout: classified as retryable
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out response
		if err := json.Unmarshal(data, &out); err != nil || out.ID == "" {
			return "", fmt.Errorf("provider returned %d with unreadable body", resp.StatusCode)
		}
		return out.ID, nil
	}
	pe := &sender.ProviderError{Status: resp.StatusCode, Err: errors.New(strings.TrimSpace(string(data)))}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			pe.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return "", pe
}
