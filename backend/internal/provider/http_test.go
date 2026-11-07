package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"campaigncomposer/internal/sender"
)

func TestSendSetsIdempotencyKeyAndParsesID(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("Idempotency-Key"), r.URL.Path
		w.Write([]byte(`{"id":"sms_1"}`))
	}))
	defer srv.Close()
	id, err := NewHTTP(srv.URL).Send(context.Background(), sender.Message{Channel: "sms", Recipient: "+90", Body: "hi", IdempotencyKey: "k1"})
	if err != nil || id != "sms_1" || gotKey != "k1" || gotPath != "/v1/sms" {
		t.Fatalf("id=%q err=%v key=%q path=%q", id, err, gotKey, gotPath)
	}
}

func TestSendMapsStatusesToProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  string
		want   time.Duration
	}{{429, "3", 3 * time.Second}, {503, "", 0}, {400, "", 0}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			http.Error(w, "nope", tc.status)
		}))
		_, err := NewHTTP(srv.URL).Send(context.Background(), sender.Message{Channel: "email", Recipient: "a@b", Body: "x"})
		srv.Close()
		var pe *sender.ProviderError
		if !errors.As(err, &pe) || pe.Status != tc.status || pe.RetryAfter != tc.want {
			t.Errorf("status %d: got %+v", tc.status, err)
		}
	}
}

func TestSendTimeoutIsAPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewHTTP(srv.URL).Send(ctx, sender.Message{Channel: "sms", Body: "x"})
	var pe *sender.ProviderError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("timeout should surface as a non-ProviderError (retryable), got %v", err)
	}
}
