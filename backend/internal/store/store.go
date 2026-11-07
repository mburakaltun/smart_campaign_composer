// Package store is the only package that talks to Postgres.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"campaigncomposer/internal/content"
	"campaigncomposer/internal/segment"
	"campaigncomposer/internal/sender"
	"campaigncomposer/internal/store/sqlgen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	StatusSegmentReady = "segment_ready"
	StatusContentReady = "content_ready"
	StatusSending      = "sending"
	StatusCompleted    = "completed"
)

var (
	ErrNotFound = errors.New("not found")
	ErrBadState = errors.New("campaign is not in a state that allows this action")
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool            { return s.pool }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

type Audience struct {
	Total  int
	SMS    int
	Email  int
	MaxLen map[string]int // longest value per personalisation placeholder
}

type Campaign struct {
	ID          string
	Brief       string
	Status      string
	Rule        segment.Node
	Explanation string
	Audience    Audience
	Content     content.Messages
	CreatedAt   time.Time
	ApprovedAt  *time.Time
	CompletedAt *time.Time
}

type ApproveResult struct {
	AlreadyApproved bool
	Queued          int // rows enqueued as pending
	Skipped         int // rows recorded as skipped (e.g. content over limit)
}

// ---- migrations --------------------------------------------------------------

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlText, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- audience ----------------------------------------------------------------

// Schema returns the values that exist in the data (used to validate and to prompt).
func (s *Store) Schema(ctx context.Context) (segment.Schema, error) {
	var sc segment.Schema
	for _, q := range []struct {
		sql string
		dst *[]string
	}{
		{`SELECT DISTINCT city FROM users ORDER BY city`, &sc.Cities},
		{`SELECT DISTINCT language FROM users ORDER BY language`, &sc.Languages},
	} {
		rows, err := s.pool.Query(ctx, q.sql)
		if err != nil {
			return sc, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return sc, err
			}
			*q.dst = append(*q.dst, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return sc, err
		}
	}
	return sc, nil
}

func (s *Store) AudienceStats(ctx context.Context, rule segment.Node) (Audience, error) {
	where, args, err := segment.Compile(rule, 1)
	if err != nil {
		return Audience{}, err
	}
	q := sqlgen.AudienceSQL + where
	var a Audience
	var fn, ln, city int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&a.Total, &a.SMS, &a.Email, &fn, &ln, &city); err != nil {
		return Audience{}, err
	}
	a.MaxLen = map[string]int{"first_name": fn, "last_name": ln, "city": city}
	return a, nil
}

// Sample returns a few recipients' placeholder values for the preview.
func (s *Store) Sample(ctx context.Context, rule segment.Node, n int) ([]map[string]string, error) {
	where, args, err := segment.Compile(rule, 1)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT u.first_name, u.last_name, u.city FROM users u WHERE `+where+` ORDER BY u.id LIMIT `+fmt.Sprint(n), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var f, l, c string
		if err := rows.Scan(&f, &l, &c); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"first_name": f, "last_name": l, "city": c})
	}
	return out, rows.Err()
}

// ---- campaigns ---------------------------------------------------------------

const campCols = `id::text, brief, status, segment_rule, segment_explanation, audience_total, audience_sms, audience_email,
                  sms_text, email_subject, email_body, created_at, approved_at, completed_at`

func scanCampaign(row pgx.Row) (Campaign, error) {
	var c Campaign
	var rule []byte
	err := row.Scan(&c.ID, &c.Brief, &c.Status, &rule, &c.Explanation, &c.Audience.Total, &c.Audience.SMS, &c.Audience.Email,
		&c.Content.SMSText, &c.Content.EmailSubject, &c.Content.EmailBody, &c.CreatedAt, &c.ApprovedAt, &c.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(rule, &c.Rule); err != nil {
		return c, fmt.Errorf("stored rule is corrupt: %w", err)
	}
	return c, nil
}

func (s *Store) CreateCampaign(ctx context.Context, brief string, rule segment.Node, explanation string, a Audience) (Campaign, error) {
	rj, err := json.Marshal(rule)
	if err != nil {
		return Campaign{}, err
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO campaigns (brief, status, segment_rule, segment_explanation, audience_total, audience_sms, audience_email)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7) RETURNING `+campCols, brief, StatusSegmentReady, string(rj), explanation, a.Total, a.SMS, a.Email)
	c, err := scanCampaign(row)
	c.Audience.MaxLen = a.MaxLen
	return c, err
}

func (s *Store) GetCampaign(ctx context.Context, id string) (Campaign, error) {
	if !uuidRe.MatchString(id) {
		return Campaign{}, ErrNotFound
	}
	return scanCampaign(s.pool.QueryRow(ctx, `SELECT `+campCols+` FROM campaigns WHERE id = $1::uuid`, id))
}

func (s *Store) ListCampaigns(ctx context.Context, limit int) ([]Campaign, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+campCols+` FROM campaigns ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveContent stores validated copy. Only allowed before approval.
func (s *Store) SaveContent(ctx context.Context, id string, m content.Messages) error {
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE campaigns SET sms_text = $2, email_subject = $3, email_body = $4, status = $5
		WHERE id = $1::uuid AND status IN ($6, $5)`, id, m.SMSText, m.EmailSubject, m.EmailBody, StatusContentReady, StatusSegmentReady)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.GetCampaign(ctx, id); gerr != nil {
			return gerr
		}
		return ErrBadState
	}
	return nil
}

// ---- approval: materialise the queue -----------------------------------------

// Approve atomically turns an approved campaign into queue rows. It is safe to
// call twice: the row lock plus ON CONFLICT DO NOTHING make the second call a no-op.
// Only users who opted in to the specific channel are enqueued for it.
func (s *Store) Approve(ctx context.Context, id string) (ApproveResult, error) {
	if !uuidRe.MatchString(id) {
		return ApproveResult{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApproveResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	var rule []byte
	var m content.Messages
	err = tx.QueryRow(ctx, `SELECT status, segment_rule, sms_text, email_subject, email_body FROM campaigns WHERE id = $1::uuid FOR UPDATE`, id).
		Scan(&status, &rule, &m.SMSText, &m.EmailSubject, &m.EmailBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApproveResult{}, ErrNotFound
	}
	if err != nil {
		return ApproveResult{}, err
	}
	if status == StatusSending || status == StatusCompleted {
		return ApproveResult{AlreadyApproved: true}, nil
	}
	if status != StatusContentReady {
		return ApproveResult{}, ErrBadState
	}
	var node segment.Node
	if err := json.Unmarshal(rule, &node); err != nil {
		return ApproveResult{}, err
	}

	smsWhere, smsArgs, err := segment.Compile(node, 3)
	if err != nil {
		return ApproveResult{}, err
	}
	smsSQL := sqlgen.EnqueueSMS(smsWhere)
	smsTag, err := tx.Exec(ctx, smsSQL, append([]any{id, m.SMSText}, smsArgs...)...)
	if err != nil {
		return ApproveResult{}, fmt.Errorf("enqueue sms: %w", err)
	}

	emailWhere, emailArgs, err := segment.Compile(node, 4)
	if err != nil {
		return ApproveResult{}, err
	}
	emailSQL := sqlgen.EnqueueEmail(emailWhere)
	emailTag, err := tx.Exec(ctx, emailSQL, append([]any{id, m.EmailSubject, m.EmailBody}, emailArgs...)...)
	if err != nil {
		return ApproveResult{}, fmt.Errorf("enqueue email: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE campaigns SET status = $2, approved_at = now() WHERE id = $1::uuid`, id, StatusSending); err != nil {
		return ApproveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApproveResult{}, err
	}
	total := int(smsTag.RowsAffected() + emailTag.RowsAffected())
	res := ApproveResult{Queued: total}
	var skipped int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE campaign_id = $1::uuid AND status = 'skipped'`, id).Scan(&skipped)
	res.Skipped = skipped
	res.Queued = total - skipped
	return res, nil
}

// Stats returns channel -> status -> count for a campaign.
func (s *Store) Stats(ctx context.Context, id string) (map[string]map[string]int, error) {
	out := map[string]map[string]int{"sms": {}, "email": {}}
	rows, err := s.pool.Query(ctx, `SELECT channel, status, count(*) FROM messages WHERE campaign_id = $1::uuid GROUP BY channel, status`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch, st string
		var n int
		if err := rows.Scan(&ch, &st, &n); err != nil {
			return nil, err
		}
		out[ch][st] = n
	}
	return out, rows.Err()
}

// QueueDepth counts rows by channel/status across all campaigns (for /metrics).
func (s *Store) QueueDepth(ctx context.Context) (map[[2]string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT channel, status, count(*) FROM messages GROUP BY channel, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]int{}
	for rows.Next() {
		var ch, st string
		var n int
		if err := rows.Scan(&ch, &st, &n); err != nil {
			return nil, err
		}
		out[[2]string{ch, st}] = n
	}
	return out, rows.Err()
}

// ---- queue (implements sender.Store) -----------------------------------------

var _ sender.Store = (*Store)(nil)

// ClaimBatch re-queues rows whose worker vanished, then atomically takes up to
// `limit` due rows. FOR UPDATE SKIP LOCKED lets several workers (or API
// instances) claim concurrently without ever taking the same row.
func (s *Store) ClaimBatch(ctx context.Context, channel string, limit int, staleAfter time.Duration) ([]sender.Message, error) {
	if _, err := s.pool.Exec(ctx, sqlgen.ReapSQL, staleAfter.Seconds()); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, sqlgen.ClaimSQL, channel, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sender.Message
	for rows.Next() {
		var m sender.Message
		if err := rows.Scan(&m.ID, &m.CampaignID, &m.UserID, &m.Channel, &m.Recipient, &m.Subject, &m.Body, &m.IdempotencyKey, &m.Attempts, &m.Throttled, &m.ConsentOK); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id int64, providerID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE messages SET status = 'sent', provider_message_id = $2, sent_at = now(), locked_at = NULL, last_error = '', updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, providerID)
	return err
}

func (s *Store) MarkRetry(ctx context.Context, id int64, next time.Time, lastErr string, countAttempt bool) error {
	a, t := 0, 1
	if countAttempt {
		a, t = 1, 0
	}
	_, err := s.pool.Exec(ctx, `UPDATE messages SET status = 'pending', next_attempt_at = $2, last_error = $3,
		attempts = attempts + $4::int, throttled = throttled + $5::int, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, next, lastErr, a, t)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, id int64, lastErr string) error {
	_, err := s.pool.Exec(ctx, `UPDATE messages SET status = 'failed', last_error = $2, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, lastErr)
	return err
}

func (s *Store) MarkSkipped(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx, `UPDATE messages SET status = 'skipped', last_error = $2, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, reason)
	return err
}

func (s *Store) CompleteFinishedCampaigns(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE campaigns c SET status = 'completed', completed_at = now()
		WHERE c.status = 'sending'
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.campaign_id = c.id AND m.status IN ('pending', 'sending'))`)
	return err
}
