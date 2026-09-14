-- v6-session-parent: a subagent's session names the session that supervises it.
--
-- Apply BEFORE the binary that reads session.parent_session_uuid ships. Order
-- is load-bearing:
--   old binary + new schema  fine: its INSERT/UPDATE on `session` does not name
--                            the column, so every session it starts is left
--                            NULL (no supervisor), and nothing it reads
--                            selects it;
--   new binary + old schema  every start_session, snapshot, session page and
--                            run history read fails on the unknown column.
--
-- Zero data migration. Existing sessions keep parent_session_uuid NULL, which
-- is exactly what they are: sessions nobody delegated.
--
-- ON DELETE SET NULL: deleting a supervisor's session row orphans its
-- subagents' sessions rather than deleting them; each one keeps its own lane
-- and its own run history.
--
-- idx_session_team_started serves the run history list (newest first, per
-- team); idx_session_parent serves "which runs did this one supervise".
--
-- deploy/scripts/apply-schema.sh only runs CREATE TABLE IF NOT EXISTS and will
-- never add this column to an existing table; this file is how production gets
-- it. The names, types and column order match
-- core/repository/sql/schema/create.sql. Verify afterwards with:
-- SHOW CREATE TABLE `session`;
--
-- Not idempotent on purpose: a second run fails on the duplicate column rather
-- than silently doing nothing, so "was it applied?" always has a clear answer.

ALTER TABLE `session`
  ADD COLUMN `parent_session_uuid` CHAR(36) NULL AFTER `updated_at`,
  ADD INDEX `idx_session_team_started` (`team_uuid`, `started_at` DESC),
  ADD INDEX `idx_session_parent` (`parent_session_uuid`),
  ADD CONSTRAINT `session_parent_session`
    FOREIGN KEY (`parent_session_uuid`)
    REFERENCES `session` (`id`)
    ON DELETE SET NULL;
