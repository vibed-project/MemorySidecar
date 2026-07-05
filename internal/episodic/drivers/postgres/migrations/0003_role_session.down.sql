DROP INDEX IF EXISTS episodic_events_namespace_timestamp_idx;
ALTER TABLE episodic_events
    DROP COLUMN IF EXISTS session_id,
    DROP COLUMN IF EXISTS role;
