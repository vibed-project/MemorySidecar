-- First-class conversation grouping (R2): role and session_id on each event,
-- plus a (namespace, timestamp) index backing time-windowed Range queries. The
-- columns are NOT NULL DEFAULT '' so existing rows read as empty and appends
-- that omit them behave exactly as before.
ALTER TABLE episodic_events
    ADD COLUMN IF NOT EXISTS role       text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS session_id text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS episodic_events_namespace_timestamp_idx
    ON episodic_events (namespace, timestamp);
