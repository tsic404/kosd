-- Application tokens (KOSD-C-75, KOSD-C-86): the control plane keeps the
-- window tokens it issued and the binding each one carries. Only the digest of
-- a token value is stored (KOSD-C-14); the binding is the account, the
-- application, the window instance, the entry origin, the read authorization
-- and the deadline the token was issued with. A revoked window, account or
-- application leaves no row, and a row past its deadline answers as an unknown
-- token.

CREATE TABLE app_tokens (
    digest     TEXT PRIMARY KEY,
    account    TEXT NOT NULL,
    app_id     TEXT NOT NULL,
    extension  TEXT NOT NULL,
    window_id  TEXT NOT NULL,
    binding    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX app_tokens_account ON app_tokens(account);
CREATE INDEX app_tokens_app ON app_tokens(app_id);
CREATE INDEX app_tokens_window ON app_tokens(account, window_id);
