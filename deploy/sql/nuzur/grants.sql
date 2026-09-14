-- grants.sql — the database and the two accounts behind the nuzur agent.
-- docs/NUZUR_AGENT.md §5. Apply BEFORE gen-views.sh apply: the views name
-- nuzur_views as their DEFINER, so that account must exist first.
--
-- THIS FILE HOLDS NO PASSWORD. __NUZUR_RO_PASSWORD__ is a placeholder that
-- deploy/scripts/nuzur-agent-setup.sh fills, in the shell's printf builtin,
-- from Kubernetes Secret nuzur-agent-db, and pipes straight into mysql on
-- stdin inside the MySQL pod. The filled SQL is never written to disk and the
-- password is never an argument of any process. The script refuses to run if
-- the placeholder would survive, or if the password is not [A-Za-z0-9]{32}.
--
-- __NUZUR_RO_HOST__ is filled the same way; production is the pod network,
-- 10.1.0.0/255.255.0.0 (§5 "Host part"). The name is the only thing it can
-- restrict: the real boundary is the password plus the metiche-mysql
-- NetworkPolicy.
--
-- Idempotent. Re-running it keeps the password equal to the Secret's.
--
-- Two accounts, deliberately:
--   nuzur_views@localhost  cannot log in (ACCOUNT LOCK). SELECT on metiche.*.
--                          It is the DEFINER of every view, so a view reads
--                          the base table with this account's rights.
--   nuzur_ro@<pod network> can log in. SELECT on metiche_nuzur.* only, so
--                          nothing on metiche, no write anywhere, no SHOW VIEW
--                          (it cannot read the view definitions either).
-- A write through a view needs write rights for both the invoker and the
-- definer. Neither has any, so even a mistaken future
-- GRANT ALL ON metiche_nuzur.* could not write to metiche.

CREATE DATABASE IF NOT EXISTS `metiche_nuzur` CHARACTER SET utf8mb4;

CREATE USER IF NOT EXISTS 'nuzur_views'@'localhost' ACCOUNT LOCK;
ALTER USER 'nuzur_views'@'localhost' ACCOUNT LOCK;
GRANT SELECT ON `metiche`.* TO 'nuzur_views'@'localhost';

CREATE USER IF NOT EXISTS 'nuzur_ro'@'__NUZUR_RO_HOST__'
    IDENTIFIED BY '__NUZUR_RO_PASSWORD__' PASSWORD EXPIRE NEVER;
ALTER USER 'nuzur_ro'@'__NUZUR_RO_HOST__'
    IDENTIFIED BY '__NUZUR_RO_PASSWORD__'
    WITH MAX_USER_CONNECTIONS 5
    PASSWORD EXPIRE NEVER ACCOUNT UNLOCK;
GRANT SELECT ON `metiche_nuzur`.* TO 'nuzur_ro'@'__NUZUR_RO_HOST__';
