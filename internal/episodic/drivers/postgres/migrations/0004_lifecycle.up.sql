-- Write-path parity with the semantic lifecycle work (U2/U3): provenance,
-- revisability, and client idempotency on the episodic log.
--
--   source      opaque provenance handle (nullable, no default — "unset").
--   supersedes  ids this event revises; carried for audit/inspection.
--   deleted_at  soft-delete tombstone; NULL = live. A superseding Append or an
--               Expire sets it. Range hides tombstoned rows unless
--               include_deleted is set. The log stays append-only; rows are
--               retained, not mutated in place, except for this tombstone.
--   dedup_key   optional client idempotency key. gRPC is at-least-once and this
--               log has no update/delete on the write path, so a retried Append
--               would otherwise duplicate an event. A partial UNIQUE index on
--               (namespace, dedup_key) lets Append INSERT ... ON CONFLICT DO
--               NOTHING and return the already-stored event on replay.
--
-- All columns are nullable so existing rows read as "live, no provenance, no
-- dedup" and appends that omit them behave exactly as before.
ALTER TABLE episodic_events
    ADD COLUMN IF NOT EXISTS source     text,
    ADD COLUMN IF NOT EXISTS supersedes text[],
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS dedup_key  text;

-- One live claim per (namespace, dedup_key); rows without a key are unconstrained.
CREATE UNIQUE INDEX IF NOT EXISTS episodic_events_namespace_dedup_key_idx
    ON episodic_events (namespace, dedup_key)
    WHERE dedup_key IS NOT NULL;

-- Range's default (live-only) scan is (namespace, cursor) filtered on
-- deleted_at IS NULL; index the live rows so the common path stays cheap.
CREATE INDEX IF NOT EXISTS episodic_events_namespace_cursor_live_idx
    ON episodic_events (namespace, cursor)
    WHERE deleted_at IS NULL;
