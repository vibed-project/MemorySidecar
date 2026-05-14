CREATE TABLE IF NOT EXISTS kv_items (
    namespace    text        NOT NULL,
    key          text        NOT NULL,
    value        bytea       NOT NULL,
    content_type text        NOT NULL DEFAULT '',
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    version      bigint      NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    PRIMARY KEY (namespace, key)
);

CREATE INDEX IF NOT EXISTS kv_items_expires_at_idx
    ON kv_items (expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS kv_items_namespace_key_prefix_idx
    ON kv_items (namespace, key text_pattern_ops);
