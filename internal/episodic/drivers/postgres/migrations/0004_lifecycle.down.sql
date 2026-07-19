DROP INDEX IF EXISTS episodic_events_namespace_cursor_live_idx;
DROP INDEX IF EXISTS episodic_events_namespace_dedup_key_idx;
ALTER TABLE episodic_events
    DROP COLUMN IF EXISTS dedup_key,
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS supersedes,
    DROP COLUMN IF EXISTS source;
