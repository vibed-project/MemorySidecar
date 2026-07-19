-- Session reconstruction (get all messages for session X) is a core recall
-- primitive, so back the (namespace, session_id) Range predicate with an index.
-- The partial predicate keeps it small: rows with no session_id (empty string)
-- are never queried by a session filter, so they don't belong in the index.
CREATE INDEX IF NOT EXISTS episodic_events_namespace_session_id_idx
    ON episodic_events (namespace, session_id, cursor)
    WHERE session_id <> '';
