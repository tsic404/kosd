-- Roll back the application-token table. Tokens are short-lived and their
-- rows are what a restart and a reinstall discard together.

DROP TABLE app_tokens;
