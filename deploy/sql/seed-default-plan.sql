-- metiche — the instance default plan.
--
-- WHY THIS FILE EXISTS AT ALL
--
-- create_team refuses to run without it. A team with no plan row has no
-- limits, and "no limits" arrived at by accident is indistinguishable from
-- "unlimited" arrived at on purpose — so the server will not guess. It fails
-- with a message naming this file rather than quietly creating a team nobody
-- can put a ceiling on later.
--
-- WHAT IT SEEDS
--
-- One plan named "self-hosted", with every limit NULL. On this schema NULL
-- means UNLIMITED, not zero. That is the whole point: metiche is open source,
-- and an open-source build that ships crippled so a hosted one can look
-- better is a bait and switch. Out of the box a self-hoster gets no cap on
-- agents, members, projects or retention, and enforcement is off.
--
-- Limits are OPT IN. To impose one, set the column to a number. To go back to
-- unlimited, set it back to NULL. Nothing else changes.
--
-- retention_days NULL is load-bearing in a second way: the sweeper deletes
-- nothing at all unless retention is both enabled in config AND given a
-- number here. Two independent switches, because deleting a self-hoster's
-- only copy of their history by default would be inexcusable.
--
-- IDEMPOTENT. Safe to run on every deploy. `key` is uniquely indexed, so a
-- re-run updates the existing row instead of adding a second default.

INSERT INTO `plan` (
    `id`,
    `key`,
    `name`,
    `description`,
    `max_concurrent_agents`,   -- NULL = unlimited
    `retention_days`,          -- NULL = keep everything, forever
    `max_members`,             -- NULL = unlimited
    `max_projects`,            -- NULL = unlimited
    `is_instance_default`,
    `sort_order`,
    `status`                   -- 1 = active (enums.RECORD_STATUS_ACTIVE)
) VALUES (
    '8c03b22e-e4fb-4877-8c89-6b9fd9336a09',
    'self-hosted',
    'Self-hosted',
    'Everything unlimited. Set a column to a number to impose a limit; set it back to NULL to remove one.',
    NULL, NULL, NULL, NULL,
    1,
    0,
    1
)
ON DUPLICATE KEY UPDATE
    -- Deliberately narrow. A re-run repairs the two things that make the
    -- plan findable — it is the default, and it is active — and touches
    -- nothing else, so an operator who set a limit here does not lose it to
    -- the next deploy.
    `is_instance_default` = 1,
    `status`              = 1;
