-- v5-browser-login: board login, a sign-in link from the terminal
-- (docs/BOARD_LOGIN.md §3, §3.4).
--
-- Two new tables, no ALTER. Apply BEFORE the backend binary that serves
-- open_board and /v1/browser/* ships. Order is load-bearing:
--   old binary + new schema  fine: nothing in it reads or writes these tables;
--   new binary + old schema  open_board fails, and every browser-session check
--                            fails closed (a signed-in member sees 404).
--
-- The two statements below are copied byte for byte from the generated
-- code/backend/metiche/core/repository/sql/schema/create.sql (codegen commit
-- 1fa2216). deploy/scripts/apply-schema.sh applies that whole file and so
-- creates these tables as well; this file is the reviewed record of exactly
-- what this deploy adds. If the two ever differ, create.sql is the truth.
--
-- Idempotent, unlike 2026-09-agent-token-hash.sql: CREATE TABLE IF NOT EXISTS
-- is what apply-schema.sh runs, so applying both is harmless. It also means a
-- run that "succeeds" proves nothing (an existing table of another shape is
-- skipped silently). Verify afterwards with:
--   SHOW CREATE TABLE `board_login_link`;
--   SHOW CREATE TABLE `browser_session`;
--
-- Depends on `account` and `agent` (foreign keys, ON DELETE CASCADE).

CREATE TABLE IF NOT EXISTS `board_login_link` (
    `id` CHAR(36) NOT NULL,
    `account_uuid` CHAR(36) NOT NULL,
    `agent_uuid` CHAR(36) NOT NULL,
    `secret_hash` VARCHAR(64) NOT NULL,
    `redirect_path` VARCHAR(120) NOT NULL,
    `requested_via` INT NOT NULL,
    `expires_at` DATETIME NOT NULL,
    `consumed_at` DATETIME,
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    INDEX `idx_board_login_link_account` (`account_uuid`, `consumed_at`, `expires_at`),
    INDEX `idx_board_login_link_expires` (`expires_at`),
    UNIQUE INDEX `uq_board_login_link_secret_hash` (`secret_hash`),
    CONSTRAINT `board_login_link_account`
        FOREIGN KEY (`account_uuid`)
        REFERENCES `account` (`id`)
        ON DELETE CASCADE,
    CONSTRAINT `board_login_link_agent`
        FOREIGN KEY (`agent_uuid`)
        REFERENCES `agent` (`id`)
        ON DELETE CASCADE
) ENGINE = InnoDB;

CREATE TABLE IF NOT EXISTS `browser_session` (
    `id` CHAR(36) NOT NULL,
    `key` VARCHAR(32) NOT NULL,
    `account_uuid` CHAR(36) NOT NULL,
    `secret_hash` VARCHAR(64) NOT NULL,
    `auth_method` INT NOT NULL,
    `created_from_agent_uuid` CHAR(36),
    `user_agent` VARCHAR(200),
    `ip_hint` VARCHAR(64),
    `expires_at` DATETIME NOT NULL,
    `last_seen_at` DATETIME,
    `revoked_at` DATETIME,
    `end_reason` INT,
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    INDEX `idx_browser_session_account` (`account_uuid`, `revoked_at`, `expires_at`),
    INDEX `idx_browser_session_agent` (`created_from_agent_uuid`),
    INDEX `idx_browser_session_expires` (`expires_at`),
    UNIQUE INDEX `uq_browser_session_secret_hash` (`secret_hash`),
    UNIQUE INDEX `uq_browser_session_key` (`key`),
    CONSTRAINT `browser_session_account`
        FOREIGN KEY (`account_uuid`)
        REFERENCES `account` (`id`)
        ON DELETE CASCADE,
    CONSTRAINT `browser_session_created_from_agent`
        FOREIGN KEY (`created_from_agent_uuid`)
        REFERENCES `agent` (`id`)
        ON DELETE CASCADE
) ENGINE = InnoDB;
