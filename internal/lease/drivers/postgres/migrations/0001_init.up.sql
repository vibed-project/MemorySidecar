CREATE TABLE IF NOT EXISTS leases (
    namespace   text        NOT NULL,
    key         text        NOT NULL,
    holder_id   text        NOT NULL,
    acquired_at timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (namespace, key)
);

CREATE INDEX IF NOT EXISTS leases_expires_at_idx ON leases (expires_at);
