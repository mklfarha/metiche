-- v7-decisions: model support for record_decision, get_review_context and
-- report_judgement, plus the contract_field index fix found while building
-- contracts.
--
-- Apply BEFORE the binary generated from v7 ships. Order is load-bearing:
--   old binary + new schema  fine: every INSERT/UPDATE it runs on decision,
--                            judgement and conflict names its columns, so the
--                            new ones take NULL, or 0 for assignment_count, and
--                            nothing it reads selects them. Its contract_field
--                            writes still skip a repeated path, which the wider
--                            unique index accepts;
--   new binary + old schema  every decision, judgement and conflict read and
--                            write fails on the unknown columns, and a field in
--                            both request and response violates the old
--                            uq_contract_field_path.
--
-- Zero data migration. Existing rows keep recorded_by_session_uuid, judged_at
-- and escalated_at NULL (not recorded before v7), and assignment_count 0.
--
-- contract_field: uq_contract_field_path widens from (assertion_uuid, path) to
-- (assertion_uuid, direction, path), so a path present in both the request and
-- the response (an `id` sent and returned) gets one row per direction. Widening
-- a unique index cannot fail on existing rows. It takes three statements, not
-- one: uq_contract_field_path is the only index whose leftmost column is
-- assertion_uuid, so it is what the assertion_has_fields foreign key relies on,
-- and dropping it first fails with MySQL error 1553. The new index is added
-- under a temporary name, the old one dropped, then the new one renamed, so
-- the foreign key always has a supporting index. If a statement fails midway,
-- the table is left with both indexes or with only the new one under its
-- temporary name; SHOW CREATE TABLE says which, and the remaining statements
-- finish the job.
--
-- decision.recorded_by_session_uuid, FK session_has_recorded_decisions ON
-- DELETE SET NULL: deleting a session row keeps the decisions it recorded and
-- forgets only who recorded them. MySQL creates the foreign key's index under
-- the constraint's name, as it does for create.sql.
--
-- idx_judgement_subject serves "judgements about this subject pair" per team;
-- idx_judgement_team_open serves the team's open and expired-assignment scan.
--
-- deploy/scripts/apply-schema.sh only runs CREATE TABLE IF NOT EXISTS and will
-- never change an existing table; this file is how production gets it. The
-- names, types and column order match core/repository/sql/schema/create.sql.
-- Verify afterwards with:
-- SHOW CREATE TABLE `contract_field`;
-- SHOW CREATE TABLE `decision`;
-- SHOW CREATE TABLE `judgement`;
-- SHOW CREATE TABLE `conflict`;
--
-- The nuzur read-only views must follow it: deploy/sql/nuzur/gen-views.sh
-- apply refuses while the live columns differ from the model, and check-live
-- reports drift until it is run.
--
-- Not idempotent on purpose: a second run fails on the duplicate column
-- `recorded_by_session_uuid` (ERROR 1060) rather than silently doing nothing,
-- so "was it applied?" always has a clear answer. Before that, it repeats the
-- three contract_field statements, which end where they started (warning 1831,
-- duplicate index, while the temporary index exists), so the failure leaves
-- the schema exactly as the first run did.

ALTER TABLE `contract_field`
  ADD UNIQUE INDEX `uq_contract_field_path_v7` (`assertion_uuid`, `direction`, `path`);

ALTER TABLE `contract_field`
  DROP INDEX `uq_contract_field_path`;

ALTER TABLE `contract_field`
  RENAME INDEX `uq_contract_field_path_v7` TO `uq_contract_field_path`;

ALTER TABLE `decision`
  ADD COLUMN `recorded_by_session_uuid` CHAR(36) NULL AFTER `updated_at`,
  ADD CONSTRAINT `session_has_recorded_decisions`
    FOREIGN KEY (`recorded_by_session_uuid`)
    REFERENCES `session` (`id`)
    ON DELETE SET NULL;

ALTER TABLE `judgement`
  ADD COLUMN `judged_at` DATETIME NULL AFTER `updated_at`,
  ADD COLUMN `assignment_count` SMALLINT NOT NULL DEFAULT 0 AFTER `judged_at`,
  ADD INDEX `idx_judgement_subject` (`team_uuid`, `subject_a_uuid`, `subject_b_uuid`),
  ADD INDEX `idx_judgement_team_open` (`team_uuid`, `status`, `judging_expires_at`);

ALTER TABLE `conflict`
  ADD COLUMN `escalated_at` DATETIME NULL AFTER `updated_at`;
