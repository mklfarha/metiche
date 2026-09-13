-- v4-agent-tokens: the bearer token names the AGENT, not the person.
--
-- Apply BEFORE the binary that reads agent.token_hash ships. Order is
-- load-bearing:
--   old binary + new schema  fine: its INSERT/UPDATE on `agent` does not name
--                            the column, so it is left NULL;
--   new binary + old schema  every join_team, create_team and token lookup
--                            fails on the unknown column.
--
-- Zero data migration. Existing agents keep token_hash NULL and keep
-- authenticating with their account token (account.token_hash). A unique
-- index on a nullable column admits any number of NULLs in InnoDB, so those
-- legacy rows coexist with it.
--
-- deploy/scripts/apply-schema.sh only runs CREATE TABLE IF NOT EXISTS and will
-- never add this column to an existing table; this file is how production gets
-- it. Verify afterwards with: SHOW COLUMNS FROM `agent`;
--
-- Not idempotent on purpose: a second run fails on the duplicate column rather
-- than silently doing nothing, so "was it applied?" always has a clear answer.

ALTER TABLE `agent`
  ADD COLUMN `token_hash` VARCHAR(64) NULL AFTER `client_key`,
  ADD UNIQUE INDEX `uq_agent_token_hash` (`token_hash`);
