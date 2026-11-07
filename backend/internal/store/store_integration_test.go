package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"campaigncomposer/internal/content"
	"campaigncomposer/internal/segment"
	"campaigncomposer/internal/sender"
)

// These tests need a real Postgres: run `make test-integration`.
// Each test gets its own schema, so they never touch real data.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	st := New(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// 600 users: every 3rd lives in İstanbul; SMS opt-in on evens, email opt-in unless g%5=0.
	if _, err := pool.Exec(ctx, `INSERT INTO users (first_name,last_name,email,phone,city,language,purchase_count,total_spent,last_purchase_at,cart_items,last_cart_add_at,sms_opt_in,email_opt_in)
		SELECT 'Ayşe'||g, 'Yılmaz', 'a'||g||'@example.test', '+90555'||lpad(g::text,7,'0'),
		       CASE WHEN g%3=0 THEN 'İstanbul' WHEN g%3=1 THEN 'Ankara' ELSE 'İzmir' END, 'tr', 0, 0, NULL, 1, now(), g%2=0, g%5<>0
		FROM generate_series(1,600) g`); err != nil {
		t.Fatal(err)
	}
	return st
}

func istanbul(t *testing.T) segment.Node {
	n, err := segment.Parse([]byte(`{"field":"city","operator":"eq","value":"İstanbul"}`), segment.Schema{Cities: []string{"İstanbul"}, Languages: []string{"tr"}})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func approvedCampaign(t *testing.T, st *Store, msgs content.Messages) (Campaign, ApproveResult) {
	t.Helper()
	ctx := context.Background()
	rule := istanbul(t)
	aud, err := st.AudienceStats(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateCampaign(ctx, "brief", rule, "x", aud)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveContent(ctx, c.ID, msgs); err != nil {
		t.Fatal(err)
	}
	res, err := st.Approve(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, res
}

var okMsgs = content.Messages{SMSText: "Merhaba {{first_name}}", EmailSubject: "Konu {{first_name}}", EmailBody: "Gövde {{last_name}}"}

func count(t *testing.T, st *Store, q string, args ...any) int {
	var n int
	if err := st.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApproveEnqueuesOnlyOptedInUsersAndIsIdempotent(t *testing.T) {
	st := testStore(t)
	c, res := approvedCampaign(t, st, okMsgs)
	if c.Audience.Total != 200 {
		t.Fatalf("audience total=%d", c.Audience.Total)
	}
	smsRows := count(t, st, `SELECT count(*) FROM messages WHERE channel='sms'`)
	emailRows := count(t, st, `SELECT count(*) FROM messages WHERE channel='email'`)
	if smsRows != c.Audience.SMS || emailRows != c.Audience.Email || res.Queued != smsRows+emailRows {
		t.Fatalf("sms=%d/%d email=%d/%d queued=%d", smsRows, c.Audience.SMS, emailRows, c.Audience.Email, res.Queued)
	}
	if n := count(t, st, `SELECT count(*) FROM messages m JOIN users u ON u.id=m.user_id WHERE (m.channel='sms' AND NOT u.sms_opt_in) OR (m.channel='email' AND NOT u.email_opt_in)`); n != 0 {
		t.Fatalf("%d messages were queued for users without consent", n)
	}
	if n := count(t, st, `SELECT count(*) FROM messages WHERE body LIKE '%{{%'`); n != 0 {
		t.Fatalf("%d messages still contain raw placeholders", n)
	}
	again, err := st.Approve(context.Background(), c.ID)
	if err != nil || !again.AlreadyApproved {
		t.Fatalf("second approve: %+v %v", again, err)
	}
	if n := count(t, st, `SELECT count(*) FROM messages`); n != smsRows+emailRows {
		t.Fatalf("approving twice enqueued duplicates: %d", n)
	}
}

func TestApproveRecordsOverlongRenderedMessagesAsSkipped(t *testing.T) {
	st := testStore(t)
	// 155 chars + "İstanbul" (8) = 163 > 160 for Istanbul recipients
	long := content.Messages{SMSText: repeat("a", 155) + "{{city}}", EmailSubject: "s", EmailBody: "b"}
	_, res := approvedCampaign(t, st, long)
	if res.Skipped == 0 {
		t.Fatal("expected over-limit SMS rows to be recorded as skipped, not sent")
	}
	if n := count(t, st, `SELECT count(*) FROM messages WHERE channel='sms' AND status='pending'`); n != 0 {
		t.Fatalf("%d over-limit sms were left pending", n)
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestClaimBatchNeverHandsOutARowTwice(t *testing.T) {
	st := testStore(t)
	_, _ = approvedCampaign(t, st, okMsgs)
	want := count(t, st, `SELECT count(*) FROM messages WHERE channel='sms' AND status='pending'`)

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := st.ClaimBatch(context.Background(), "sms", 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, m := range batch {
					seen[m.ID]++
					if !m.ConsentOK {
						t.Errorf("claimed message %d without consent", m.ID)
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != want {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %d claimed %d times", id, n)
		}
	}
}

func TestRetryScheduleAndStaleRecovery(t *testing.T) {
	st := testStore(t)
	_, _ = approvedCampaign(t, st, okMsgs)
	ctx := context.Background()
	batch, err := st.ClaimBatch(ctx, "sms", 1, time.Minute)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim: %v %v", batch, err)
	}
	id := batch[0].ID

	// scheduled for the future: invisible to claimers, attempt counted
	if err := st.MarkRetry(ctx, id, time.Now().Add(time.Hour), "boom", true); err != nil {
		t.Fatal(err)
	}
	for _, m := range mustClaim(t, st, 1000) {
		if m.ID == id {
			t.Fatal("a retry scheduled in the future was claimed early")
		}
	}
	if a := count(t, st, `SELECT attempts FROM messages WHERE id=$1`, id); a != 1 {
		t.Fatalf("attempts=%d", a)
	}
	// 429 style retry: not counted as an attempt
	if _, err := st.pool.Exec(ctx, `UPDATE messages SET status='sending' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRetry(ctx, id, time.Now().Add(-time.Second), "429", false); err != nil {
		t.Fatal(err)
	}
	if a := count(t, st, `SELECT attempts FROM messages WHERE id=$1`, id); a != 1 {
		t.Fatalf("429 must not consume attempts, attempts=%d", a)
	}
	// worker crash: row stuck in 'sending' comes back after staleAfter
	again, _ := st.ClaimBatch(ctx, "sms", 1000, time.Minute)
	if !containsID(again, id) {
		t.Fatal("due retry should be claimable")
	}
	time.Sleep(20 * time.Millisecond)
	recovered, err := st.ClaimBatch(ctx, "sms", 1000, time.Millisecond)
	if err != nil || !containsID(recovered, id) {
		t.Fatal("stale 'sending' rows must be re-queued and claimed again")
	}
}

func mustClaim(t *testing.T, st *Store, n int) []sender.Message {
	t.Helper()
	b, err := st.ClaimBatch(context.Background(), "sms", n, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsID(ms []sender.Message, id int64) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestCampaignCompletesWhenNothingIsPending(t *testing.T) {
	st := testStore(t)
	c, _ := approvedCampaign(t, st, okMsgs)
	ctx := context.Background()
	if err := st.CompleteFinishedCampaigns(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCampaign(ctx, c.ID); got.Status != StatusSending {
		t.Fatalf("campaign with pending work must stay 'sending', got %s", got.Status)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE messages SET status='sent'`); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteFinishedCampaigns(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCampaign(ctx, c.ID); got.Status != StatusCompleted || got.CompletedAt == nil {
		t.Fatalf("want completed, got %+v", got.Status)
	}
}
