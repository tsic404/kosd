-- Upgrade preflight records (C-88): one row per platform or extension upgrade
-- preflight, carrying the request together with the result the control plane
-- computed. The newest row is the current record; a later preflight marks the
-- older rows superseded without deleting them, so an old record stays readable.
-- Payload is the domain record as JSON; the columns beside it are what the
-- read path filters and orders by.

CREATE TABLE upgrade_preflights (
    id          TEXT PRIMARY KEY,
    sequence    INTEGER NOT NULL UNIQUE,
    signature   TEXT NOT NULL,
    payload     TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    superseded  INTEGER NOT NULL DEFAULT 0 CHECK (superseded IN (0, 1))
);

CREATE INDEX upgrade_preflights_superseded ON upgrade_preflights(superseded);
