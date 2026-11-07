CREATE TABLE users (
    id               BIGSERIAL PRIMARY KEY,
    first_name       TEXT        NOT NULL,
    last_name        TEXT        NOT NULL,
    email            TEXT        NOT NULL,
    phone            TEXT        NOT NULL,
    city             TEXT        NOT NULL,
    language         TEXT        NOT NULL DEFAULT 'tr',
    purchase_count   INT         NOT NULL DEFAULT 0,
    total_spent      INT         NOT NULL DEFAULT 0,
    last_purchase_at TIMESTAMPTZ,
    cart_items       INT         NOT NULL DEFAULT 0,
    last_cart_add_at TIMESTAMPTZ,
    sms_opt_in       BOOLEAN     NOT NULL DEFAULT FALSE,
    email_opt_in     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX users_city_idx ON users (city);

CREATE TABLE campaigns (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    brief               TEXT        NOT NULL,
    status              TEXT        NOT NULL CHECK (status IN ('segment_ready', 'content_ready', 'sending', 'completed')),
    segment_rule        JSONB       NOT NULL,
    segment_explanation TEXT        NOT NULL DEFAULT '',
    audience_total      INT         NOT NULL DEFAULT 0,
    audience_sms        INT         NOT NULL DEFAULT 0,
    audience_email      INT         NOT NULL DEFAULT 0,
    sms_text            TEXT        NOT NULL DEFAULT '',
    email_subject       TEXT        NOT NULL DEFAULT '',
    email_body          TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_at         TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ
);

-- The queue. One row per (campaign, recipient, channel); the unique constraint
-- is what guarantees the same person is never enqueued the same message twice.
CREATE TABLE messages (
    id                  BIGSERIAL PRIMARY KEY,
    campaign_id         UUID        NOT NULL REFERENCES campaigns (id),
    user_id             BIGINT      NOT NULL REFERENCES users (id),
    channel             TEXT        NOT NULL CHECK (channel IN ('sms', 'email')),
    recipient           TEXT        NOT NULL,
    subject             TEXT        NOT NULL DEFAULT '',
    body                TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'skipped')),
    attempts            INT         NOT NULL DEFAULT 0,
    throttled           INT         NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at           TIMESTAMPTZ,
    last_error          TEXT        NOT NULL DEFAULT '',
    provider_message_id TEXT        NOT NULL DEFAULT '',
    idempotency_key     TEXT        NOT NULL UNIQUE,
    sent_at             TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, user_id, channel)
);
CREATE INDEX messages_queue_idx ON messages (channel, next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX messages_sending_idx ON messages (locked_at) WHERE status = 'sending';
CREATE INDEX messages_campaign_idx ON messages (campaign_id, channel, status);
