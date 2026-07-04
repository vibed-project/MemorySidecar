-- Per-namespace cursor allocator. Cursors are handed out by atomically
-- incrementing last_cursor with INSERT ... ON CONFLICT DO UPDATE, which takes a
-- row-level lock on the namespace's counter row. This avoids the illegal
-- `SELECT max(cursor) ... FOR UPDATE` (aggregates cannot be combined with
-- FOR UPDATE) while keeping appends atomic and monotonic per namespace.
CREATE TABLE IF NOT EXISTS episodic_cursors (
    namespace   text   PRIMARY KEY,
    last_cursor bigint NOT NULL
);

-- Seed counters from any events that already exist so cursors stay monotonic
-- across the migration (no-op on a fresh database).
INSERT INTO episodic_cursors (namespace, last_cursor)
     SELECT namespace, MAX(cursor)
       FROM episodic_events
      GROUP BY namespace
ON CONFLICT (namespace) DO NOTHING;
