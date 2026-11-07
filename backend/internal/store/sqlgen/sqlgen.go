// Package sqlgen builds the larger SQL statements. It has no database driver
// dependency so the generated text can be checked against a real Postgres in
// isolation (and the placeholders / casts reviewed in one place).
package sqlgen

import (
	"fmt"

	"campaigncomposer/internal/content"
)

// RenderExpr substitutes every personalisation placeholder inside parameter
// $param (the template) with the matching users column.
func RenderExpr(param int) string {
	expr := fmt.Sprintf("$%d::text", param)
	for _, name := range content.PlaceholderNames() {
		expr = fmt.Sprintf("replace(%s, '{{%s}}', %s)", expr, name, content.Placeholders[name])
	}
	return expr
}

// EnqueueSMS expects $1 = campaign id, $2 = sms template, $3.. = segment args.
// Only users with sms_opt_in are selected.
func EnqueueSMS(where string) string {
	return fmt.Sprintf(`INSERT INTO messages (campaign_id, user_id, channel, recipient, subject, body, status, last_error, idempotency_key)
SELECT $1::uuid, s.id, 'sms', s.phone, '', s.rendered,
       CASE WHEN char_length(s.rendered) <= %[1]d THEN 'pending' ELSE 'skipped' END,
       CASE WHEN char_length(s.rendered) <= %[1]d THEN '' ELSE 'content_exceeds_limit' END,
       ($1::uuid)::text || ':' || s.id::text || ':sms'
FROM (SELECT u.id, u.phone, %[2]s AS rendered FROM users u WHERE (%[3]s) AND u.sms_opt_in) s
ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`, content.SMSMax, RenderExpr(2), where)
}

// EnqueueEmail expects $1 = campaign id, $2 = subject, $3 = body, $4.. = segment args.
// Only users with email_opt_in are selected.
func EnqueueEmail(where string) string {
	return fmt.Sprintf(`INSERT INTO messages (campaign_id, user_id, channel, recipient, subject, body, status, last_error, idempotency_key)
SELECT $1::uuid, s.id, 'email', s.email, s.subject, s.body,
       CASE WHEN char_length(s.subject) <= %[1]d AND char_length(s.body) <= %[2]d THEN 'pending' ELSE 'skipped' END,
       CASE WHEN char_length(s.subject) <= %[1]d AND char_length(s.body) <= %[2]d THEN '' ELSE 'content_exceeds_limit' END,
       ($1::uuid)::text || ':' || s.id::text || ':email'
FROM (SELECT u.id, u.email, %[3]s AS subject, %[4]s AS body FROM users u WHERE (%[5]s) AND u.email_opt_in) s
ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`, content.SubjectMax, content.BodyMax, RenderExpr(2), RenderExpr(3), where)
}

// ClaimSQL expects $1 = channel, $2 = limit.
const ClaimSQL = `
WITH picked AS (
    SELECT id FROM messages
    WHERE channel = $1 AND status = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at, id
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE messages m SET status = 'sending', locked_at = now(), updated_at = now()
FROM picked, users u
WHERE m.id = picked.id AND u.id = m.user_id
RETURNING m.id, m.campaign_id::text, m.user_id, m.channel, m.recipient, m.subject, m.body, m.idempotency_key, m.attempts, m.throttled,
          CASE WHEN m.channel = 'sms' THEN u.sms_opt_in ELSE u.email_opt_in END`

// ReapSQL expects $1 = stale seconds.
const ReapSQL = `UPDATE messages SET status = 'pending', locked_at = NULL, updated_at = now()
WHERE status = 'sending' AND locked_at < now() - make_interval(secs => $1::float8)`

const AudienceSQL = `SELECT count(*), count(*) FILTER (WHERE u.sms_opt_in), count(*) FILTER (WHERE u.email_opt_in),
       COALESCE(max(char_length(u.first_name)), 0), COALESCE(max(char_length(u.last_name)), 0), COALESCE(max(char_length(u.city)), 0)
FROM users u WHERE `
