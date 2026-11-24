-- Terminal audit events (KOSD-C-92, KOSD-C-96): one row per establish, reject,
-- input receipt, renewal or end. Raw input, output and tokens never enter it;
-- the immutable target fields and the execution identity are what a reader
-- needs to correlate one session. Retention is the table's: ending a session,
-- uninstalling the component or revoking the target permission deletes no row.

CREATE TABLE terminal_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    actor              TEXT NOT NULL,
    extension          TEXT NOT NULL,
    session_id         TEXT NOT NULL,
    target_type        TEXT NOT NULL,
    target_id          TEXT NOT NULL,
    target_uid         TEXT NOT NULL,
    namespace          TEXT NOT NULL,
    container          TEXT NOT NULL,
    execution_identity TEXT NOT NULL,
    event              TEXT NOT NULL,
    result             TEXT NOT NULL,
    sequence           INTEGER,
    receipt            TEXT NOT NULL DEFAULT '',
    occurred_at        INTEGER NOT NULL
);

CREATE INDEX terminal_events_session ON terminal_events(session_id, id);
