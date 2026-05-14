CREATE TABLE IF NOT EXISTS episodic_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   text        NOT NULL,
    cursor      bigint      NOT NULL,
    timestamp   timestamptz NOT NULL DEFAULT now(),
    type        text        NOT NULL,
    payload     bytea       NOT NULL DEFAULT ''::bytea,
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (namespace, cursor)
);

CREATE INDEX IF NOT EXISTS episodic_events_namespace_cursor_idx
    ON episodic_events (namespace, cursor);

-- pgcrypto provides gen_random_uuid on older Postgres versions.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
