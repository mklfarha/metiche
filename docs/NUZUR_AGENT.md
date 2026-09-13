# nuzur agent for metiche production data

**Status:** design. Nothing here has been done yet. This file is the only thing
written. No MySQL user, no view, no nuzur agent, no unit, no token exists yet.

**Scope:** let the owner browse metiche's production MySQL in the nuzur data
manager, read-only, with secret columns removed. It runs through a nuzur agent
installed on the box as a host systemd service.

Citations use `path:line`. Unless a path says otherwise, the roots are:

| prefix | location on the owner's laptop |
|---|---|
| `cli/` | `~/Dropbox/nuzur-24/code/nuzur-cli` (release v1.9.2, `constants/constants.go:3`) |
| `go/` | `~/Dropbox/nuzur-24/code/nuzur-go` |
| `web/` | `~/Dropbox/nuzur-24/code/nuzur-web` |
| `kr/` | `~/go/pkg/mod/github.com/99designs/keyring@v1.2.2` (the keyring library nuzur-cli pins, `cli/go.mod:6`) |
| (none) | this repository |

Box facts marked *(observed)* came from read-only `get` and `describe` commands
and `information_schema` queries on 2026-09-12. No values were read.

---

## 1. Context

metiche's production data lives in `metiche-mysql`, which is:

- a MySQL 8.0.46 StatefulSet in namespace `metiche`;
- reachable only through the ClusterIP Service `metiche-mysql` (10.152.183.239:3306) *(observed)*;
- holding database `metiche`, with 25 tables *(observed)*.

Today there is no way to look at that data except by `kubectl exec` as root. The
owner wants the nuzur data manager: browsing, search, and ad-hoc SELECTs.

The nuzur agent (`nuzur-cli agent start`) is a process next to the database. It
dials **out** to `cm.nuzur.com:443` and holds a gRPC stream. The cloud sends it
SQL and it runs that SQL locally (`cli/agent/daemon.go:1-11`, `:184-238`).
Nothing connects in. On this box, `cm.nuzur.com` and `product.nuzur.com` both
resolve to the box's own address, 69.164.192.246 *(observed)*.

**What the agent can do is the problem this design solves.** It runs anything it
is sent:

- queries (`cli/agent/handlers.go:33-94`);
- writes (`handleExec`, `:97-120`);
- transactions (`:126-185`).

It has no read-only mode and no column filter. connection-manager's PII masker
applies only to queries that carry a nuzur project context and a project that
declares PII (`go/connection-manager/module/sql-query-manager/policy.go:53-79`,
`masker.go:41-55`). Its guard blocks only hidden tables, never writes
(`guard.go:27-49`). **Neither is a security boundary for this database.** The
only boundary is what the MySQL account can do. So the whole exposure model
lives in MySQL.

The metiche repository is **public**. Scripts, the unit template, the
column policy and SQL without credentials may be committed. Passwords, tokens,
DSNs and agent credentials live only on the box.

## 2. Decisions

The owner confirmed these. They are not open.

| # | Decision |
|---|---|
| D1 | **Only the owner queries this connection.** It is never shared with a team. |
| D2 | **Secret columns are NULL in what nuzur sees:** `invite.code`, `notification_channel.target_url`, `team_event.response_snapshot`, `account.token_hash`, `agent.token_hash`. `account.email` stays visible, because the owner is the only viewer. |
| D3 | **The agent runs as a host systemd service**, not a pod. |
| D4 | **A restart never creates a new agent or a new connection.** This is property **P1**, tested in section 9. |

These decisions follow from the source research:

| # | Decision | Why |
|---|---|---|
| D5 | nuzur reads a separate database `metiche_nuzur` of views, through a MySQL account `nuzur_ro` that has `SELECT` on `metiche_nuzur.*` and nothing on `metiche`. | Section 4. |
| D6 | Views are an **allowlist with total classification**. Every column of every table is explicitly `expose` or `redact`. Anything unclassified stops the generator. | Section 4.2. |
| D7 | **Registration uses the CLI's own login-free path.** Pair with a provisioning token, then `agent connection add` *without* `--no-publish`, which publishes using the agent's own credentials. No `nuzur-cli deploy`. No laptop-side `UpdateLocalAgentConnections`. | Section 3.A. |
| D8 | The connection UUID is generated once, recorded on the box, and always passed as `--uuid`. | Section 3.B. |
| D9 | **`nuzur-cli connect`, `agent install`, `agent pair --force` and `nuzur-cli login` are never run on the box.** | Each can create a second agent, connection or daemon. See section 3.B. |
| D10 | The DSN and the pairing token go in through the CLI's **masked prompts**, never through argv or environment. | Section 6. |
| D11 | Pairing and `connection add` happen **at most once**, from an operator shell, never from the unit. The unit refuses to start unless the paired state is exactly the recorded one. | Sections 5 and 7. |

---

## 3. What the source says

### A. Registering the connection without `nuzur-cli deploy`

**The owner's hint was right.** nuzur-cli has a standalone, login-free path for
a headless box. Publishing uses the paired agent's own token.

**1. `agent pair` persists agent credentials, from a login or from a provisioning token.**

- `--provisioning-token` (env `NUZUR_PROVISIONING_TOKEN`) exchanges a single-use token (`cli/app/command_agent.go:56-60`, `:82-85`).
- The exchange calls `ExchangeProvisioningToken` (`cli/app/command_agent.go:146-168`).
- On a machine with no display, `agent pair` with no token asks for it in a **masked prompt** (`:89-92` → `cli/app/command_connect.go:127-158`). Headless detection is `cli/app/headless.go:28-35`.
- The server consumes the token atomically (`go/product/server/local_agent.go:151-184`). The token lives 15 minutes (`go/product/server/provisioning_token.go:23`).
- The server creates **one new `local_agent` row per exchange** (`local_agent.go:72-115`).
- The CLI writes `local_agent_uuid.txt` and `local_agent_token.txt` into a 0700 directory, each file 0600 (`cli/app/command_agent.go:270-281`). The directory is `~/.config/nuzur/agent` (`cli/files/local_agent.go:33-38`).

**The only place a user login is needed is minting the provisioning token.**
The owner does that in a browser at `app.nuzur.com/pair` ("Pair a server"), on
the laptop. The box never logs in.

**2. `agent connection add` without `--no-publish` publishes with the agent's credentials.**

1. After saving locally, add calls `publishCatalog` (`cli/app/command_agent_connection.go:139-144`).
2. `publishCatalog` chooses how to sign (`:267-288`, `:236-245`):
   - a user token file present → sign as the user;
   - otherwise, agent uuid and token present → **sign with the agent's token**.
3. The agent-signed call is `PublishLocalAgentCatalog` (`:314-332`). The server authenticates it by the agent token alone. It is exempt from the user auth interceptor (`go/product/server/local_agent.go:272-288`, `:361-396`).
4. What `--no-publish` suppresses is exactly that call, and nothing else (`cli/app/command_agent_connection.go:134-138`). nuzur's deploy passes it only because its laptop CLI publishes afterwards with the user token (`cli/deploy/templates/bootstrap.sh.tmpl:467-471`).

The server's comment on `PublishLocalAgentCatalog` describes our case exactly:
"A machine paired headlessly … has no user session, so this is the only way it
can make its databases visible in nuzur."

**3. Replace-the-list semantics are harmless here.**

- Both server RPCs **replace** the agent's whole catalog (`go/product/server/local_agent.go:248-270`, `:290-338`, `:323`).
- The CLI always sends its whole local registry (`cli/app/command_agent_connection.go:262-266`, `:249-260`).
- The catalog belongs to **one agent**. This agent is dedicated to metiche, and its registry holds exactly one entry. So every publish sends `[metiche-prod]` and replaces `[metiche-prod]`.
- No laptop publishes to this agent's uuid. A laptop's `agent connection` commands publish to the laptop's own agent (`cli/app/command_agent.go:183-193`).
- The server keeps team shares server-side and strips any the client sends (`local_agent.go:434-439`). A republish can never share the connection.

**The supported sequence** (the full idempotent version is section 5):

```sh
# laptop, browser, signed in:  app.nuzur.com/pair → "Pair a server" → copy token (15 min, single use)
# box, as the dedicated user, clean environment:
nuzur-cli agent pair                                        # masked prompt: paste the token
nuzur-cli agent connection add metiche-prod --uuid <UUID>   # masked prompts: mysql / host / port / user / password
                                                            # → "Published — the connection now appears … under "Via agent""
systemctl enable --now nuzur-agent.service
# laptop, browser: data manager → Via agent → this agent → metiche-prod → schema metiche_nuzur
```

**Existing agents are left untouched.** `listLocalAgents` today *(observed)*:

- 2 agents, 1 connection each, so 2 connections;
- one is `darwin`, the owner's laptop;
- the other is `linux` with machine name `localhost`. It is **an agent serving another project**: its connection belongs to that project and is shared with another team. **This design never revokes, unpairs, re-publishes or otherwise touches it.** Revoking it would cut that project's data access.

The metiche agent is a **separate, new agent**, created by step 8's single pairing.

**Starter plan cap: no headroom after metiche-prod.** On the Starter plan the
server counts connections across all of the owner's non-revoked agents, and
rejects a publish when the total would exceed **3** (`local_agent.go:29-31`,
`:300-321`, `:398-411`). Today there are 2, so `metiche-prod` becomes the **3rd**.
That is allowed, and it is the last one.

- **A 4th connection anywhere** fails at publish time with gRPC `FailedPrecondition`: "starter plan is limited to 3 database connections — upgrade to Pro for unlimited connections" (`local_agent.go:316-320`).
  - "Anywhere" means on this agent, the laptop's, or the other project's agent.
  - `agent connection add` has already saved the entry locally. It then prints "Saved on this machine but publishing the connection to nuzur failed" and returns success (`cli/app/command_agent_connection.go:139-143`).
  - The new connection **does not appear** in the data manager. The existing three are unaffected, because the rejected publish writes nothing.
- **Republishing metiche's own one-entry catalog stays allowed at the cap.** The target agent is counted by its incoming list (1), not added on top of its stored list (`local_agent.go:402-411`). So the same-uuid re-seal (section 3.C) and the password rotation still work.
- **Before step 9, confirm the count is still 2.** If it is already 3, stop: the metiche publish would be the 4th. Resolve that first, by upgrading the plan or removing a connection the owner no longer needs. Never touch the other project's agent.

**Two agents will be named `localhost`.** nuzur-cli has **no way to set an
agent name**:

- `agent pair` accepts only `--force`, `--provisioning-token` and `--headless` (`cli/app/command_agent.go:51-65`).
- The machine name is always `os.Hostname()` (`:122`, `:150-156`), and the box's static hostname is `localhost` *(observed)*.
- The server sets `machine_name` only when it creates the row (`go/product/server/local_agent.go:89-100`), and no product RPC renames an agent.

So the design identifies the metiche agent **only by its uuid**:

- Step 8 snapshots the owner's agent uuids **before** pairing. The metiche agent is the one uuid that is new afterwards, and it must equal `local_agent_uuid.txt`.
- It is recorded as `AGENT_UUID` in `/etc/nuzur-agent/ids.env`.
- Every later action, including P1, rotation, recovery and revocation, takes that uuid and **never a name**.
- Do not rename the box to get a distinct name. The hostname feeds the keyring passphrase (section 3.C) and is the Kubernetes node name.

### B. What a restart does, and every way a duplicate can arise

**`agent start` never registers, pairs or publishes.** Its full startup path:

1. **`Before` hook.** It migrates legacy files from `/tmp/nuzur-cli` only if they exist, and never overwrites (`cli/app/command_agent.go:26-31`, `cli/files/local_agent.go:84-127`). The unit uses `PrivateTmp=yes`, so there is nothing to migrate.
2. **Fallback DSN resolution** (`cli/app/command_agent_start.go:116-133`). With no `--dsn` or `--driver`, no env vars and no saved fallback files, it loads the registry. The registry has entries, so it returns empty **without prompting** (`:127-130`).
3. **`agent.Run`** (`cli/agent/daemon.go:76-172`):
   - reads the uuid and token files, or exits "agent not paired" (`:77-80`, `:346-358`);
   - loads the registry read-only (`cli/agent/connections/connections.go:67-116`), which rewrites only for a pre-keychain legacy file (`:94-98`);
   - opens the keyring; on Linux this writes and deletes a probe item (`cli/agent/connections/keyring.go:78-85`, `:119-130`);
   - dials, and sends `Hello{uuid, token, cli_version}` (`daemon.go:193-203`).
4. **The server** validates the token (`go/connection-manager/server/local_agent_channel.go:137-174`), registers an **in-memory** session (`:69-70`), and runs one column-scoped `UPDATE local_agent SET status, last_seen_at, updated_at` (`:186-227`). That write cannot touch `connections`, `token_hash` or `revoked_at`.
5. **The read loop.** On stream loss it reconnects with backoff, reusing the same uuid and token (`daemon.go:129-171`).

None of these calls exist on that path: `RegisterLocalAgent`,
`ExchangeProvisioningToken`, `PublishLocalAgentCatalog`,
`UpdateLocalAgentConnections`.

**Where connection identity lives, and why it is stable.**

- The uuid is a field of the entry in `local_agent_connections.json` (`connections.go:42-55`). The file is written only by `Save` (`:118-129`), which only `connection add` and `connection remove` call.
- The DSN is keyed by that uuid in the keyring as `dsn-<uuid>` (`keyring.go:189-191`).
- On the server, the uuid sits in `local_agent.connections`, which only a catalog publish replaces.
- Requests arrive carrying the uuid, and the daemon resolves them against the registry (`cli/agent/dbpool.go:46-89`).
- Nothing on the start path writes any of these, so the uuid is stable across restarts, reboots and binary upgrades.
- `updated_at` and `last_seen_at` on the agent row **do** change on every connect. P1 excludes them.

**Every duplicate path, and what prevents it.**

| # | How a duplicate arises | Source | Prevention in this design |
|---|---|---|---|
| 1 | Re-running `agent pair` | Refused if `local_agent_uuid.txt` exists (`cli/app/command_agent.go:73-80`). `--force` inserts a second agent row, because every exchange inserts (`go/product/server/local_agent.go:72-115`). | Setup skips pairing when the uuid file exists, and stops if the recorded uuid exists but the file does not. `--force` is never used. A provisioning token is single-use, so an accidental re-run cannot pair without a fresh token. |
| 2 | `nuzur-cli connect` | Removes any entry with the same name and adds one **without a uuid**, which mints a new connection uuid on every run (`cli/app/command_connect.go:182-184`, `:230-236`, `connections.go:162-164`). It also installs a `systemd --user` unit (`command_connect.go:263-281`, `cli/agent/install.go:172-211`), which is a **second daemon**. | Never run on the box (D9). |
| 3 | `agent install` | Writes `~/.config/systemd/user/nuzur-agent.service` and enables it (`cli/agent/install.go:164-211`): a second daemon with the same credentials. | Never run (D9). Setup asserts no user unit exists. |
| 4 | `agent connection add` again with the same name | **Interactive** mode refuses a duplicate name (`command_agent_connection.go:80-84`, `:347-353`). **Scripted** mode (any of `--dsn`, `--driver`, `--non-interactive`) removes the existing entry and re-adds it; without `--uuid` it mints a **new** uuid (`:62`, `:70-78`, `:118-125`). | Setup skips when the entry exists. `--uuid` from `/etc/nuzur-agent/ids.env` is always passed, even interactively (`:119`). |
| 5 | A user login on the box | With a user token file present, publish switches to user mode. User mode **re-pairs** when the stored agent is NotFound, creating a new agent (`command_agent_connection.go:271-281`, `:290-309`). `agent unpair` without `--keep-remote` forces a login (`cli/app/command_agent_unpair.go:41-47`). | Never `nuzur-cli login` as `nuzur-agent` (D9). The unit's preflight refuses to start if a user token file exists in the agent's config dir. |
| 6 | Laptop-side `UpdateLocalAgentConnections` against this agent | Replaces the catalog with whatever the laptop sends (`local_agent.go:256-270`). | Not used. The box is the only publisher of its own catalog. |
| 7 | Keyring unreadable after a hostname, machine-id or `$USER` change | The passphrase is derived from those three (section 3.C). A decode failure makes `connections.Load` fail (`connections.go:109-112`). The daemon logs it and continues with an **empty** registry (`daemon.go:92-97`). `agent start` would try to prompt for a fallback DSN (`command_agent_start.go:128-132`, `:143-174`), which fails with stdin `/dev/null` **before** anything is saved (`:176`). **No duplicate, but an outage.** | Preflight compares hostname and a machine-id fingerprint to recorded values and refuses to start, with a precise message. Recovery keeps the same uuid (section 3.C). |
| 8 | Loss of the config directory | uuid and token are gone, and the daemon exits "not paired" (`daemon.go:77-80`) in a restart loop. It creates nothing. The server stores only the token's hash (`local_agent.go:33-45`), so the pairing **cannot** be recovered. | This is the **one** case where the agent uuid legitimately changes, and it is manual: revoke the old metiche agent **by the recorded `AGENT_UUID` only**, pair once, record the new uuid, add the connection with the **recorded** connection uuid, and re-point the data manager. There is no automatic re-pair anywhere. |
| 9 | A HOME or `XDG_CONFIG_HOME` mismatch between setup and the unit | The config dir is `os.UserConfigDir()/nuzur/agent`, falling back to `/tmp/nuzur-cli` (`cli/files/local_agent.go:33-38`). A different HOME looks "not paired", which tempts someone to re-pair. | The same explicit `HOME`, `USER` and `XDG_CONFIG_HOME` in the unit and in every setup invocation (`env -i`). Preflight asserts the paired files exist at the expected path. |
| 10 | Two daemons with one pairing | The server registers sessions by agent uuid (`local_agent_channel.go:69-70`), so the two fight over the session. No new row, but flapping. | Only the system unit runs `agent start`. Setup checks `pgrep -u nuzur-agent -f 'agent start'` before enabling. |

### C. The keyring on headless Linux under systemd

- **Backend.** The allowed backends on Linux are Secret Service, KWallet, then File (`keyring.go:146-151`). If the chosen backend cannot write, a probe write falls back to File (`:68-85`).
  - On this box `dbus-launch` and `gnome-keyring-daemon` are absent *(observed)*.
  - A system service has no session bus.
  - So **File** is what the CLI uses both at setup and in the daemon.
  - Its directory is `~/.config/nuzur/agent/keyring` (`keyring.go:159-163`). The directory is 0700 (`kr/file.go:46`) and each item file is 0600 (`kr/file.go:146`).
- **Passphrase inputs, confirmed** (`keyring.go:170-187`): `sha256( os.Hostname() ‖ contents of /etc/machine-id ‖ $USER ‖ "nuzur-cli-keyring-v1" )`.
  - An unreadable input is **silently skipped** rather than raising an error, so a missing `/etc/machine-id` also changes the passphrase.
  - It is an **environment variable**, `$USER`, not the uid.
- **What a fixed `User=` guarantees.** systemd sets `$USER`, `$LOGNAME` and `$HOME` from `User=` for system services. The unit also sets them explicitly, as nuzur's own bootstrap does (`cli/deploy/templates/bootstrap.sh.tmpl:7-12`, `:487-492`). So the daemon always derives the passphrase with `USER=nuzur-agent`.
  - The guarantee is only as good as setup doing the same. Every setup call therefore runs `runuser -u nuzur-agent -- env -i HOME=… USER=nuzur-agent …`, never a bare `sudo -u` that inherits the caller's `$USER`.
- **If the hostname changes.** The box's static hostname is `localhost` *(observed)*, which is also its Kubernetes node name. Renaming the host would disturb microk8s as well, so it is unlikely to happen casually. If it does, the item no longer decrypts, and preflight stops the unit. There are two recoveries, and **neither creates a connection**:
  1. **Preferred:** restore the hostname. Nothing else changes.
  2. **Re-seal under the new passphrase, keeping the uuid:**
     1. `systemctl stop nuzur-agent`.
     2. Delete `~nuzur-agent/.config/nuzur/agent/keyring/dsn-<CONNECTION_UUID>`. The file backend's `Remove` is a plain unlink that needs no passphrase (`kr/file.go:158-165`). A missing item is a soft state (`kr/file.go:78-80`, `keyring.go:211-224`), so the registry loads again.
     3. `agent connection remove metiche-prod --no-publish`. This drops the entry locally only; the server catalog is untouched (`command_agent_connection.go:178-210`).
     4. `agent connection add metiche-prod --uuid <same>`, interactive. This publishes a catalog identical to the one already on the server.
     5. Update `/etc/nuzur-agent/host.fingerprint`.
     6. Start the unit and run P1.

  A **machine-id change** has the same symptom and the same recovery. It happens when a VM is cloned or `systemd-machine-id-setup` is re-run.

### D. Default schema, `USE`, and `SELECT *`

**The agent issues `USE`, and the schema comes from the cloud, not from the box's registry.**

- Every non-transaction query goes through `resolveQueryerWithSchema` (`cli/agent/handlers.go:34`, `:98`, `:197-222`). With a schema set, it takes a dedicated connection and runs ``USE `schema` `` with the identifier quoted (`:228-247`). Transactions apply it once at `BeginTx` (`:137-143`).
- The schema is `userConnection.DbSchema`, the data manager's saved connection (`go/connection-manager/module/sql-connection-manager/connection_local_agent.go:112`, `:136`).
- If that is empty, it falls back to the catalog's `default_schema` (`manager.go:199-203`).
- The daemon itself never reads the registry's `DefaultSchema` (`cli/agent/dbpool.go:54-82`).
- For a MySQL connection added interactively, `default_schema` stays empty (`cli/app/command_agent_connection.go:107-115`), and the DSN has **no database** (`cli/app/command_agent_start.go:244-249`).

The consequence is **fail-closed**:

| Data manager schema | What happens |
|---|---|
| `metiche_nuzur` | Works. |
| Empty | "No database selected". |
| `metiche` | Access denied, because `nuzur_ro` has no grant there. |

**Data-manager queries use `SELECT *`, so column-level GRANTs are unusable.**

- The default table action is `"SELECT * FROM " + entity.identifier + " LIMIT 100"` (`web/src/project-data-manager/entities_list.tsx:108`).
- Fetching records by key is `"SELECT * FROM " + this.identifier + " WHERE "` (`web/src/domain/entity.ts:507-508`).
- Search names its display fields explicitly (`web/src/project-data-manager/search_query.ts:35-38`).

MySQL refuses `SELECT *` when the user lacks the privilege on any column, so
per-column grants would break the first two outright. Views that keep **every
column name** serve both styles. A redacted column simply reads as NULL. That is
why the views NULL columns rather than dropping them.

---

## 4. Data exposure model

```
metiche (database)          untouched; no grants to nuzur_ro
   ▲ SELECT only
nuzur_views@localhost       ACCOUNT LOCK; SELECT ON metiche.*; owns (DEFINER) every view
   ▲ SQL SECURITY DEFINER
metiche_nuzur (database)    one view per exposed table, same name as the table
   ▲ SELECT only
nuzur_ro@'69.164.192.246'   SELECT ON metiche_nuzur.*; MAX_USER_CONNECTIONS 5
   ▲ DSN in the agent's keyring
nuzur-agent (host unit) ──► cm.nuzur.com:443 ──► data manager (owner only)
```

- **Two accounts.** The definer can read `metiche` but cannot log in. The login account can read only views.
  - MySQL checks the definer's privileges on the base tables and the invoker's privileges on the view.
  - A write through a view would need INSERT, UPDATE or DELETE on both, and neither account has them.
  - So even a mistaken future `GRANT ALL ON metiche_nuzur.*` could not write to `metiche`.
- **Host restriction.** The agent reaches the ClusterIP from the host.
  - `ip route get 10.152.183.239` gives `src 69.164.192.246` *(observed)*.
  - kube-proxy, running with `--cluster-cidr=10.1.0.0/16`, masquerades non-pod sources to Services *(observed)*. On this single-node box that resolves to the same eth0 address.
  - The account is therefore `'nuzur_ro'@'69.164.192.246'`. Setup step 1 **measures** the source before creating it.
  - `skip_name_resolve=1` *(observed)*, so the host part must be an IP.
  - Pods get 10.1.0.0/16 addresses and never match.
- **Limits.**
  - `MAX_USER_CONNECTIONS 5` equals the agent's own pool maximum (`cli/agent/dbpool.go:116`), so the agent never trips it under its own load.
  - Five of the server's 200 *(observed)* is the most nuzur can ever hold.
  - `PASSWORD EXPIRE NEVER`.
  - No `FAILED_LOGIN_ATTEMPTS`, so a mistyped prompt cannot lock the account.
- **Query timeout.** See section 6 and Q1. With the pinned CLI, an interactively entered DSN cannot carry `readTimeout`. `max_execution_time` is 0 globally *(observed)* and cannot be set per account. Changing it globally would also cap the backend, so this design does not.

### 4.1 Allowlist, not denylist

**Decision: allowlist with total classification.**

| Option | On a new column (as v4 added `agent.token_hash`, `deploy/sql/2026-09-agent-token-hash.sql`) | Verdict |
|---|---|---|
| `SELECT *` views, or denylist by omission | The new column is exposed automatically, secret or not. | **Rejected.** This is how a secret leaks silently. |
| Plain allowlist that silently omits unknown columns | Never exposed, but the view drifts from the model. Explicit-field queries break with "unknown column", and nobody decided anything. | Rejected: stale in silence. |
| **Allowlist with total classification** | The generator **refuses to run** until someone writes `expose` or `redact` for the column. Existing views stay as they were: stale but safe. | **Chosen.** It never exposes by default, and it turns staleness into a loud failure. |

### 4.2 The column policy and the view generator

**Policy file.** `deploy/nuzur-agent/columns.policy` is committed and contains no
secrets. It has one line per column of `metiche`:

```
# table.column                     decision   note
account.token_hash                 redact     D2
agent.token_hash                   redact     D2 (added v4)
invite.code                        redact     D2 join code
notification_channel.target_url    redact     D2 webhook URL is a credential (docs/MODEL.md:63-66)
team_event.response_snapshot       redact     D2
account.email                      expose     D2 owner-only viewer
agent.client_key                   expose!    identifier, not a bearer (docs/IDENTITY.md:9-11,38)
project.repo_url                   expose!    see Q3
...every other column...           expose
```

**Decisions.**

- `expose`: the column appears by name.
- `redact`: the column appears as a typed NULL with the same name. `CAST(NULL AS CHAR)` for char and text types, `CAST(NULL AS JSON)` for json, plain `NULL` otherwise.
- `expose!`: an **acknowledged** exposure of a column whose name looks risky.
- A table with no lines gets no view.

**Risky-name check.** Column names matching
`/(token|secret|code|key|pass|pwd|url|uri|snapshot|hash|salt|credential|cookie|signature|private|webhook|dsn|auth)/i`
**must** be `redact` or `expose!`. A plain `expose` on such a name fails.

Today's matches, other than the five redacted columns, must be written as `expose!`:

- `*.key` on plan, account, agent, member, project, contract (also `key_norm`), decision, notification_channel, session, intent, claim, conflict and instruction;
- `agent.client_key`, `conflict.dedupe_key`, `judgement.pair_key`;
- `team_event.subject_key`, `team_event.idempotency_key`;
- `decision_token.token` and `intent_token.token` (tokenized wording, `docs/MODEL.md:35,46`);
- `contract_assertion.shape_hash`;
- `project.repo_url`.

Some columns do not match the pattern but deserve a conscious decision anyway;
the policy notes them:

- `notification_channel.last_error` is capped because a response body can echo the URL (`docs/MODEL.md:66`);
- `team_event.payload`, `team.settings`;
- `account.identity_subject` and `account.identity_handle`.

**Generator.** `deploy/nuzur-agent/gen-views.sh [--check|--apply]` runs as root on
the box, in the style of `deploy/scripts/apply-schema.sh`. It runs `mysql` inside
the pod with the root password taken from the pod's environment, SQL on stdin,
and never selects a row value.

1. Read `information_schema.columns` for `table_schema='metiche'`: table, column, ordinal position, data type. Names only.
2. **Fail with no DDL** if any of these is true, printing each offending `table.column` and the fix:
   - a column exists in MySQL but not in the policy (**new column**);
   - a table exists that has no policy lines (**new table**);
   - a policy line names a column that no longer exists, because a view naming it would error with 1356;
   - a risky name is marked plain `expose`;
   - a line is malformed or duplicated.
3. Build, in ordinal order, one statement per table:
   ``CREATE OR REPLACE ALGORITHM=MERGE DEFINER=`nuzur_views`@`localhost` SQL SECURITY DEFINER VIEW `metiche_nuzur`.`<t>` AS SELECT `<c1>`, CAST(NULL AS CHAR) AS `<secret>`, … FROM `metiche`.`<t>`;``
   Then add `DROP VIEW` for any view in `metiche_nuzur` that is no longer expected.
4. `--check` stops here. It exits 0 only if the policy is total and the generated SQL's sha256 matches `/etc/nuzur-agent/views.sha256`.
5. `--apply` executes the SQL in one `mysql` session and writes the new sha256. It is idempotent: `CREATE OR REPLACE` produces the same result on re-run, and when the hash is unchanged it skips.

**When it runs.**

- **After every schema change**, as the last step of any `deploy/sql/*.sql` migration and of `deploy/scripts/apply-schema.sh`. apply-schema never adds columns (`deploy/scripts/apply-schema.sh:12-16`), so migrations are hand-applied, and the runbook line is "then `gen-views.sh --apply`".
- **In CI, repo-side, with no database:** the same classification check against the committed, codegen-generated `code/backend/metiche/core/repository/sql/schema/create.sql`. A pull request that adds a column without a policy line fails before it merges.
- **Daily on the box**, via `nuzur-views-check.timer` running `--check`. A failure leaves a failed unit visible in `systemctl --failed`, which catches a migration applied without the policy step.

---

## 5. Idempotent setup

**Constants** (non-secret):

| name | value |
|---|---|
| `KUBECTL` | `microk8s kubectl` |
| `NS` | `metiche` |
| `SVC_IP` | read from `$KUBECTL -n metiche get svc metiche-mysql -o jsonpath='{.spec.clusterIP}'` (10.152.183.239 today) |
| `AGENT_USER` | `nuzur-agent` |
| `AGENT_HOME` | `/var/lib/nuzur-agent` |
| `CFG` | `$AGENT_HOME/.config/nuzur/agent` |
| `CONN_NAME` | `metiche-prod` |
| `CLI_VERSION` | `1.9.2`, the latest release, 2026-09-10 |

**Files on the box:**

| path | owner, mode | content |
|---|---|---|
| `/etc/metiche/nuzur_ro.password` | root:root 0600 | 32 alphanumeric characters, no newline. Lives in the existing root-only credential dir. |
| `/etc/nuzur-agent/ids.env` | root:root 0644 | `SOURCE_IP=`, `AGENT_UUID=`, `CONNECTION_UUID=`. Identifiers, not secrets. |
| `/etc/nuzur-agent/host.fingerprint` | root:root 0644 | `HOSTNAME=` plus `MACHINE_ID_SHA256=` |
| `/etc/nuzur-agent/views.sha256` | root:root 0644 | hash of the last applied view SQL |

**Two helpers** that every step uses:

```sh
as_agent() {   # identical environment to the unit; nothing inherited
    runuser -u nuzur-agent -- env -i HOME=/var/lib/nuzur-agent USER=nuzur-agent \
        LOGNAME=nuzur-agent XDG_CONFIG_HOME=/var/lib/nuzur-agent/.config \
        PATH=/usr/local/bin:/usr/bin:/bin TERM="${TERM:-xterm}" /usr/local/bin/nuzur-cli "$@"
}
mysql_root() { # SQL on stdin; password never leaves the pod's environment
    $KUBECTL -n metiche exec -i metiche-mysql-0 -c mysql -- \
        sh -c 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -u root --batch --skip-column-names'
}
```

Every step below is **check → skip or act → verify**. `setup.sh` stops at the
first STOP. The steps that need a human (6 and 7) print what to do and exit;
re-running continues from the first incomplete step.

### Step 0: preconditions (check only)

- It runs as root, from a checkout, on x86_64 (`deploy/scripts/lib.sh` conventions).
- `metiche-mysql-0` is Ready.
- **STOP** if any of these exist:
  - a `systemd --user` nuzur unit;
  - a process `pgrep -f 'nuzur-cli agent start'` not owned by our unit;
  - `/tmp/nuzur-cli`;
  - a user token file under `$CFG/..` (the login token; see duplicate path 5).

### Step 1: source IP

- **Check:** `ids.env` has `SOURCE_IP`, and `ip route get $SVC_IP` still reports that `src`. If so, skip.
- **Act:**
  1. Measure the source as MySQL sees it. Open one bare TCP connection with `timeout 5 sh -c 'exec 3<>/dev/tcp/$SVC_IP/3306; sleep 3'` (bash `/dev/tcp`).
  2. Meanwhile run `SELECT DISTINCT SUBSTRING_INDEX(host,':',1) FROM information_schema.processlist WHERE user='unauthenticated user'` through `mysql_root`.
  3. Expect `69.164.192.246`. Record it.
  4. One aborted handshake is far below `max_connect_errors`.

### Step 2: the password file

- **Check:** `/etc/metiche/nuzur_ro.password` exists, is 0600 root, and is non-empty. Skip.
- **Act:** `umask 077; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > …`. This is exactly `deploy/scripts/gen-credentials.sh`'s `gen_password`: alphanumeric because the value passes through SQL, a DSN and a prompt.
- It is never echoed and never an argument.

### Step 3: MySQL accounts and database

- **Check:**
  - `SELECT COUNT(*) FROM mysql.user WHERE (user,host) IN (('nuzur_ro',@src),('nuzur_views','localhost'))` returns 2;
  - `SHOW GRANTS` for each is exactly the expected set;
  - `SCHEMA_NAME='metiche_nuzur'` exists.
  - All true → skip.
  - Any **extra** grant → STOP. Never silently revoke.
- **Act:** SQL built with the shell's `printf` **builtin** (dash), piped to `mysql_root`. The password is interpolated inside the shell process only, never into an external command's argv.

```sql
CREATE DATABASE IF NOT EXISTS metiche_nuzur;
CREATE USER IF NOT EXISTS 'nuzur_views'@'localhost' ACCOUNT LOCK;
GRANT SELECT ON metiche.* TO 'nuzur_views'@'localhost';
CREATE USER IF NOT EXISTS 'nuzur_ro'@'69.164.192.246'
    IDENTIFIED BY '<from file>' PASSWORD EXPIRE NEVER;
ALTER USER 'nuzur_ro'@'69.164.192.246' WITH MAX_USER_CONNECTIONS 5;   -- idempotent; password untouched
GRANT SELECT ON metiche_nuzur.* TO 'nuzur_ro'@'69.164.192.246';
```

Rotating the password is a separate `--rotate-ro` flag, like `gen-credentials.sh --rotate-app`. It runs `ALTER USER … IDENTIFIED BY`, then the section 3.C re-seal (remove `--no-publish` plus add with the same `--uuid`).

### Step 4: views

`deploy/nuzur-agent/gen-views.sh --apply`. It skips when the hash matches and fails loudly when the policy is incomplete (section 4.2).

### Step 5: pinned nuzur-cli

- **Check:** `/usr/local/bin/nuzur-cli --version` prints `nuzur CLI version 1.9.2`, and the file is root:root 0755. Skip.
- **Act:** exactly nuzur's bootstrap (`cli/deploy/templates/bootstrap.sh.tmpl:407-453`):
  1. Download `nuzur-cli_Linux_x86_64.tar.gz` and `nuzur-cli_1.9.2_checksums.txt` from the `v1.9.2` GitHub release.
  2. **Verify sha256 before `tar`**, then `install -m 0755`.
- Not writable by `nuzur-agent`.
- An upgrade later is this step with a new version plus `systemctl restart`. It never re-pairs.

### Step 6: system user

- **Check:** `id nuzur-agent`. Skip creation.
- **Act:** `useradd --system --user-group --home-dir /var/lib/nuzur-agent --create-home --shell /usr/sbin/nologin nuzur-agent`.
- **Always enforce:** `chown nuzur-agent: /var/lib/nuzur-agent; chmod 0700 /var/lib/nuzur-agent`.
- Only `root` has a login shell on the box today *(observed)*.

### Step 7: host fingerprint

- **Check:** `/etc/nuzur-agent/host.fingerprint` exists. If it matches `hostname` and `sha256sum /etc/machine-id`, continue. If it differs, **STOP** and show the section 3.C recovery.
- **Act (first run only):** write it.

### Step 8: pair (at most once, by a human)

- **Check, in order:**
  1. `$CFG/local_agent_uuid.txt` exists → skip. If `ids.env` has `AGENT_UUID`, they must be equal, else STOP.
  2. `ids.env` has `AGENT_UUID` but the file is missing → **STOP**: credentials lost (duplicate path 8). Never pair automatically.
- **Act:**
  1. On the laptop, snapshot the owner's agent uuids with `listLocalAgents`. Today that is 2 agents, 2 connections. If the connection total is already 3, **STOP** (section 3.A cap).
  2. The owner opens `app.nuzur.com/pair` → "Pair a server" **immediately** before, because the token lives 15 minutes.
  3. Run `as_agent agent pair`. `env -i` has no `DISPLAY`, so headless detection triggers the masked "Pairing token" prompt (`cli/app/command_connect.go:127-158`), with 3 attempts.
  4. Paste.
  5. Record `AGENT_UUID` from `as_agent agent status`, which is local only (`cli/app/command_agent_install.go:52-66`).
- **Verify:** on the laptop, `listLocalAgents` shows exactly one uuid that was not in the snapshot. It equals `AGENT_UUID`, with `connections` empty. Every pre-existing agent, including the one serving another project, is unchanged. Its machine name will also be `localhost`, so compare uuids only.

### Step 9: connection (at most once, by a human)

- **Check:** `as_agent agent connection list` shows exactly one entry, named `metiche-prod`, with uuid `CONNECTION_UUID`. Skip.
  - Any other entry, or a different uuid → **STOP**.
- **Act:**
  1. If `ids.env` lacks `CONNECTION_UUID`, generate it **once** with `cat /proc/sys/kernel/random/uuid` and record it **before** adding.
  2. Inside `tmux`, which is present on the box *(observed)*, load the password into a paste buffer without displaying it: `tmux load-buffer -b nuzurro /etc/metiche/nuzur_ro.password`.
  3. Run `as_agent agent connection add metiche-prod --uuid "$CONNECTION_UUID"`. No `--driver`, `--dsn` or `--non-interactive`, so the command stays interactive (`cli/app/command_agent_connection.go:62`).
  4. Answer the prompts: engine `mysql`, host `$SVC_IP`, port `3306`, user `nuzur_ro`, password `C-b :paste-buffer -b nuzurro` then Enter. The password field is masked (`cli/app/command_agent_start.go:237`, `:270-272`).
  5. `tmux delete-buffer -b nuzurro`.
- **Verify:**
  - The output ends with "Published — the connection now appears in the nuzur data manager under "Via agent"" (`command_agent_connection.go:144`).
  - `listLocalAgents` shows this agent with `connections` = [one entry, `CONNECTION_UUID`, `db_type` 1, no `shared_team_uuids`].
- **If the save succeeded but the publish failed:** `connection list` does **not** retry, despite the CLI's message (`:139-142` vs `:150-175`). Run `as_agent agent connection remove metiche-prod --no-publish`, then re-run this step with the same uuid.

### Step 10: preflight, unit, enable

- **Check:** `cmp` the installed files against `deploy/nuzur-agent/`. Skip if identical. `systemctl is-enabled` and `is-active` → skip enabling or starting.
- **Act:** `install -m 0644` the unit, `install -m 0755` the preflight. `daemon-reload` only if something changed. `enable --now` only if not enabled or not active.
- Setup never restarts a healthy unit.

### Step 11: NetworkPolicy

Section 8. `kubectl diff` first, `apply` only if different.

### Step 12: data manager, once, on the laptop

- In nuzur: attach the "Via agent" connection → agent `AGENT_UUID` → `metiche-prod` → **schema `metiche_nuzur`**. The schema is what makes `USE` work (section 3.D).
- **Check first** that it is not already attached.
- Do not share it (D1). Sharing is only possible through `UpdateLocalAgentConnectionSharing` (`go/product/server/local_agent.go:514-583`).

---

## 6. Credential handling

| Secret | Created | Stored | How it moves | Never |
|---|---|---|---|---|
| `nuzur_ro` password | on the box, step 2 | `/etc/metiche/nuzur_ro.password` (root 0600), and inside the agent's keyring item as part of the DSN | into SQL through the shell builtin to `kubectl exec` stdin; into the CLI through a tmux paste buffer into a masked prompt | printed, argv, environment, repo, laptop |
| provisioning token | owner mints it in the browser just before step 8 | nowhere; single use, 15 min | pasted into the masked pairing prompt | argv, history, file |
| agent token | returned by the exchange | `$CFG/local_agent_token.txt` (0600 in a 0700 dir, `cli/app/command_agent.go:270-281`) | read by the daemon and sent in `Hello` | copied, backed up, hashed for display |
| DSN | assembled by the CLI from the prompts | `$CFG/keyring/dsn-<uuid>`: encrypted file, 0600 | read by the daemon only | the plaintext fallback files `local_agent_dsn.txt` and `local_agent_driver.txt` (`cli/app/command_agent_start.go:291-303`). Preflight fails if either exists. |

**Why the pairing token goes through the prompt, not the environment.** The brief
asked for the environment, and the CLI does support `NUZUR_PROVISIONING_TOKEN`
(`cli/app/command_agent.go:57-59`). But getting a value *into* a clean
`runuser … env -i` environment without putting it in `env`'s own argv needs
extra machinery. The masked prompt keeps the token out of argv, out of
`/proc/<pid>/environ` and out of history, and it is the CLI's own headless
path. If the environment form is ever scripted, the token must be exported by a
shell builtin in the final process, never passed as `env NAME=value`.

**The cost of entering the DSN interactively** (see Q1). The prompt builds
`user:pass@tcp(host:port)/?parseTime=true` (`cli/app/command_agent_start.go:248`).
It has no database and no timeout parameters, and v1.9.2 offers no non-argv way
to supply a full DSN to `agent connection add`: `--dsn` is a flag with no env or
file source (`cli/app/command_agent_connection.go:45`). This design accepts that
for v1:

- the schema is pinned by the data manager plus the grants, fail-closed (section 3.D);
- concurrency is capped by `MAX_USER_CONNECTIONS 5`;
- a runaway query is killed by hand: `mysql_root <<< "SELECT id FROM information_schema.processlist WHERE user='nuzur_ro' AND time>60"` then `KILL QUERY <id>`.

**Target DSN** once a non-argv input exists:

```
nuzur_ro:<pw>@tcp(10.152.183.239:3306)/metiche_nuzur?parseTime=true&timeout=5s&readTimeout=60s&writeTimeout=60s
```

together with `--schema metiche_nuzur`. Switching to it is a scripted add with
the **same `--uuid`**, which upserts in place (`:70-78`, `:118-125`). No new
connection.

**Nothing under `deploy/` contains a secret.** Everything in `deploy/nuzur-agent/`
is a template, a script or a policy. `.gitignore` already blocks
`credentials.env`, `prod.yaml`, `*.key` and `*.pem`. The files above live
outside the checkout.

---

## 7. The systemd unit

`deploy/nuzur-agent/nuzur-agent.service`, installed to `/etc/systemd/system/`:

```ini
[Unit]
Description=nuzur agent: read-only metiche views for the nuzur data manager
Documentation=https://github.com/mklfarha/metiche/blob/main/docs/NUZUR_AGENT.md
Wants=network-online.target
After=network-online.target snap.microk8s.daemon-kubelite.service
# Never start half-configured: no pairing or connection → the unit is skipped, not failed.
ConditionPathExists=/var/lib/nuzur-agent/.config/nuzur/agent/local_agent_uuid.txt
ConditionPathExists=/var/lib/nuzur-agent/.config/nuzur/agent/local_agent_token.txt
ConditionPathExists=/var/lib/nuzur-agent/.config/nuzur/agent/local_agent_connections.json
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
Type=simple
User=nuzur-agent
Group=nuzur-agent
# Must equal setup's as_agent(): HOME finds the config dir, USER seeds the keyring passphrase.
Environment=HOME=/var/lib/nuzur-agent
Environment=USER=nuzur-agent
Environment=LOGNAME=nuzur-agent
Environment=XDG_CONFIG_HOME=/var/lib/nuzur-agent/.config
# No fallback DSN, ever: a set driver/DSN would bypass the registry and prompt/save plaintext.
UnsetEnvironment=NUZUR_AGENT_DSN NUZUR_AGENT_DRIVER NUZUR_PROVISIONING_TOKEN NUZUR_CONNECTION_MANAGER_ADDRESS NUZUR_AGENT_INSECURE
# Read-only checks: hostname + machine-id fingerprint, USER, ids.env agent/connection uuids,
# exactly one registry entry, keyring item present, no user token file, no fallback DSN files.
ExecStartPre=/usr/local/lib/nuzur-agent/preflight.sh
ExecStart=/usr/local/bin/nuzur-cli agent start
Restart=on-failure
RestartSec=10
StandardInput=null
UMask=0077
# Sandbox
NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/nuzur-agent
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

Notes:

- **No secrets in the unit.** No `EnvironmentFile`, unlike nuzur's deploy (`cli/deploy/templates/bootstrap.sh.tmpl:473-480`, `:493`), because this design has no fallback DSN.
- **`Restart=on-failure`, not `always`.** A missing pairing ends in a start-limit stop rather than an endless loop, and it creates nothing either way (section 3.B).
- **`PrivateTmp=yes`** neutralises the legacy `/tmp/nuzur-cli` migration.
- **`ProtectHostname=yes`** only stops the service from *changing* the hostname. It does not pin the name the service reads, so preflight is what catches a rename.
- **A clean stop logs as a failure.** On SIGTERM, `agent start` returns the context error (`cli/agent/daemon.go:133-135`) and `main.go:17` calls `log.Fatal`, so the process exits 1. `systemctl stop` or `restart` therefore logs "status=1/FAILURE". A requested stop is never restarted, and the P1 test expects this log line.
- **Preflight** (`deploy/nuzur-agent/preflight.sh`) only reads. It exits non-zero with one precise sentence per failed check.

---

## 8. Network

### 8.1 NetworkPolicy for metiche-mysql

There is none in `metiche` today *(observed)*, so any pod in the cluster can reach
3306. `deploy/nuzur-agent/networkpolicy-mysql.yaml`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: metiche-mysql-ingress
  namespace: metiche
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: metiche-mysql
      app.kubernetes.io/instance: metiche-mysql
  policyTypes: [Ingress]
  ingress:
    - from:            # the backend
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: metiche
              app.kubernetes.io/instance: metiche
      ports: [{ protocol: TCP, port: 3306 }]
    - from:            # the host agent (post-SNAT source, measured in step 1)
        - ipBlock: { cidr: 69.164.192.246/32 }
      ports: [{ protocol: TCP, port: 3306 }]
```

- **Probes cannot break.** `metiche-mysql`'s readiness and liveness probes are `exec` (`mysqladmin ping -h 127.0.0.1` inside the container) *(observed)*. They never cross the pod's network interface.
- **Schema scripts are unaffected.** `apply-schema.sh` and `gen-views.sh` use `kubectl exec`, not the network.
- **Calico may or may not police host-to-local-pod traffic.** The `ipBlock` rule makes the policy correct either way, and the test below settles which.

**Test** (reversible in one command: `kubectl -n metiche delete networkpolicy metiche-mysql-ingress`):

1. `kubectl apply --dry-run=server -f …`, then `apply`.
2. **Backend still works:** backend logs show no DB errors, and the board loads and updates.
3. **Pod restarts:** `kubectl -n metiche get pod metiche-mysql-0 -w` shows no restart, and `READY` stays `1/1`.
4. **Agent still works:** a data-manager query succeeds.
5. **Others are blocked:** `kubectl run np-probe -n default --rm -it --image=busybox:1.36 --restart=Never -- nc -zvw3 10.152.183.239 3306` times out. Before the policy, it connects.
6. **Isolate the host rule:** temporarily remove the `ipBlock` rule and repeat step 4.
   - Fails → Calico polices host traffic and the rule is required.
   - Passes → it is belt and braces.
   - Restore the rule.

### 8.2 Agent egress (feasible; recommended as a second step, see Q4)

A Kubernetes NetworkPolicy does not apply to host processes. A Calico
HostEndpoint would, but auto host endpoints impose a default deny on the whole
shared box. **Rejected.**

What works is an **nftables table scoped by socket owner**, loaded by a small
oneshot unit `nuzur-agent-egress.service`. The agent unit declares
`Requires=` and `After=` on it, so if the rules fail to load, the agent does not
start. The file `deploy/nuzur-agent/egress.nft`:

```
destroy table inet nuzur_agent
table inet nuzur_agent {
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid != "nuzur-agent" accept
    ct state established,related accept
    ip daddr 127.0.0.53 meta l4proto { udp, tcp } th dport 53 accept          # systemd-resolved stub
    ct original ip daddr 69.164.192.246 ct original proto-dst 443 accept     # cm.nuzur.com, product.nuzur.com (both resolve here)
    ct original ip daddr 10.152.183.239 ct original proto-dst 3306 accept    # metiche-mysql ClusterIP
    counter reject
  }
}
```

- **Match with `ct original`.** Local DNAT, meaning kube-proxy for the ClusterIP and the ingress hostPort for 443, runs at `nat output`, before `filter output`. A plain `ip daddr` would already see the pod IP.
- **Never enable Ubuntu's `nftables.service` with its default config.** It starts with `flush ruleset`, which would wipe kube-proxy's and Calico's rules.
- ufw is inactive and `OUTPUT` policy is ACCEPT *(observed)*, so nothing else conflicts.

**Test:**

- `runuser -u nuzur-agent -- curl -sS https://example.com` is rejected.
- The agent reconnects after `systemctl restart nuzur-agent`, and the data manager still queries.
- `nft list table inet nuzur_agent` counters rise only on `reject` for stray traffic.

**Caveat:** if nuzur moves `cm` or `product` off this box's IP, the agent goes
offline. That fails closed, and the fix is to update one address.

---

## 9. Verification

### 9.1 Data through the data manager

- On the `metiche_nuzur` connection, `SELECT * FROM account LIMIT 100` and `SELECT * FROM agent LIMIT 100` return rows.
  - `token_hash` is NULL on every row.
  - `email` is populated.
- `SELECT COUNT(*) FROM invite WHERE code IS NOT NULL` returns 0. Repeat the same check for `notification_channel.target_url`, `team_event.response_snapshot`, `account.token_hash` and `agent.token_hash`.
- Entity search in the data manager works. It uses explicit fields (section 3.D), which proves the view column names line up with the nuzur project.
- `SHOW DATABASES` lists only `information_schema` and `metiche_nuzur`, plus possibly `performance_schema`. `metiche` must not appear.
- `USE metiche` fails with 1044.

### 9.2 Writes refused

Run through the data manager's SQL editor. Each must fail with MySQL 1142, command denied:

- `INSERT INTO plan (id, \`key\`, name, status) VALUES (UUID(), 'x', 'x', 1)`;
- `UPDATE account SET display_name = display_name`;
- `DELETE FROM team_event WHERE 1=0`;
- `CREATE TABLE t (i INT)`.

Also create a record through the data manager UI: it must fail.

**Independent of nuzur**, from the box:

1. Write a temporary `[client]` defaults file on tmpfs (`umask 077`, under `/run`) from `/etc/metiche/nuzur_ro.password`.
2. Run `docker run --rm --network host -v <file>:/c.cnf:ro mysql:8.0 mysql --defaults-extra-file=/c.cnf -h 10.152.183.239 -u nuzur_ro -e "<statement>"` for each statement above. Each fails. `SELECT 1` succeeds.
3. `shred -u` the file.

The password never appears in argv.

### 9.3 Property P1: identity is restart-stable

**Claim.** Restarts, crashes and reboots never create an agent or a connection,
and never change their uuids.

**Snapshot S**, taken from the laptop via `listLocalAgents` and from the box:

- the set and count of the owner's non-revoked agent uuids;
- for `AGENT_UUID`: `created_at`, `machine_name`, and `connections` as a list of (`uuid`, `name`, `db_type`, `shared_team_uuids`), with count 1 and uuid equal to `CONNECTION_UUID`;
- on the box: `sha256sum $CFG/local_agent_uuid.txt $CFG/local_agent_connections.json`;
- `stat -c '%i %s %Y'` of `$CFG/local_agent_token.txt`. The token is not hashed: its hash is what the server treats as secret (`go/product/server/local_agent.go:240-243`);
- `ls $CFG/keyring` names plus the `%Y` mtime of `dsn-$CONNECTION_UUID`;
- `as_agent agent connection list`, with the DSN masked by the CLI.

**Excluded, because they change legitimately on every connect:** `status`,
`last_seen_at` and `updated_at` (`go/connection-manager/server/local_agent_channel.go:186-196`).

**Procedure:**

1. Take S0 with the agent ONLINE (`status` 1).
2. **Restarts:** repeat 5 times:
   1. `systemctl restart nuzur-agent`;
   2. wait until `status` is 1 and `last_seen_at` is newer than before the restart;
   3. take Si;
   4. assert Si equals S0.
3. **Crash:** `systemctl kill -s KILL nuzur-agent`. Expect a restart through `Restart=on-failure`. Snapshot and assert equal.
4. **Reboot:** `systemctl reboot`. After boot, check that the unit is active and `status` is 1, then snapshot and assert equal.
5. **Negative control:**
   1. Temporarily move `local_agent_connections.json` aside.
   2. `systemctl restart nuzur-agent` must be **skipped** (condition failed). No new connection appears in `listLocalAgents`.
   3. Restore the file, restart, snapshot and assert equal.

**Pass:** every snapshot equals S0. **Any** difference fails P1. Stop and
investigate before continuing.

`deploy/nuzur-agent/verify-p1.sh` automates the box half and prints only uuids,
counts, hashes of non-secret files and mtimes.

---

## 10. Revocation, in order

1. **Cut data access first.** `DROP USER 'nuzur_ro'@'69.164.192.246';` then kill any remaining sessions (`KILL <id>` for `user='nuzur_ro'` in `information_schema.processlist`), because DROP USER does not end open sessions. From this moment the agent can reach nothing.
2. **Revoke the metiche agent in nuzur, by uuid only.** From the signed-in laptop, run `. /etc/nuzur-agent/ids.env` (copied over, non-secret) or read `AGENT_UUID` from it. Then `nuzur-cli agent revoke "$AGENT_UUID"` (`cli/app/command_agent.go:235-267`).
   - Before running it, check that this uuid's `connections` in `listLocalAgents` is exactly [`metiche-prod`, `CONNECTION_UUID`]. If it is not, **STOP**.
   - **Never pick the agent by name in the web UI or anywhere else.** Two agents are named `localhost`, and the other one serves another project. Revoking it would cut that project's data access.
   - The server marks it REVOKED and clears shares (`go/product/server/local_agent.go:186-225`).
   - Reconnects are refused (`go/connection-manager/server/local_agent_channel.go:163-165`).
   - Remove the data manager's saved connection.
3. **Stop the service:** `systemctl disable --now nuzur-agent nuzur-agent-egress nuzur-views-check.timer`. Then remove the unit files from `/etc/systemd/system` and `daemon-reload`.
4. **Remove config and keyring:** `rm -rf /var/lib/nuzur-agent`, then `userdel nuzur-agent`.
   - This removes the uuid, the token, the registry and the keyring.
   - Do **not** use `agent unpair` on the box. It needs a login (`cli/app/command_agent_unpair.go:41-47`), and it leaves the registry and keyring behind (`:73-81`).
5. **Tidy up:**
   - `DROP DATABASE metiche_nuzur; DROP USER 'nuzur_views'@'localhost';`
   - `rm /etc/metiche/nuzur_ro.password`, `rm -r /etc/nuzur-agent /usr/local/lib/nuzur-agent`, `rm /usr/local/bin/nuzur-cli`.
   - `nft delete table inet nuzur_agent`.
   - Remove the `ipBlock` rule from the NetworkPolicy, and keep the backend rule.
6. **Confirm:** `listLocalAgents` shows the agent REVOKED (status 3), and no process runs as `nuzur-agent`.

---

## 11. Where the non-secret pieces live in the repo

Described here, not written yet. Everything goes under `deploy/nuzur-agent/`,
next to the existing `deploy/scripts/` conventions: POSIX sh, `lib.sh` helpers,
`need_root`, no `pipefail`.

| file | purpose |
|---|---|
| `setup.sh` | Section 5. Idempotent, root, stops at the first STOP or human step. Reuses `deploy/scripts/lib.sh`. |
| `columns.policy` | Total column classification (section 4.2). |
| `gen-views.sh` | `--check` / `--apply` view generator (section 4.2). |
| `check-policy-ci.sh` | The same classification check against `create.sql`, for CI; no database. |
| `nuzur-agent.service` | Section 7. |
| `preflight.sh` | Read-only `ExecStartPre` checks (section 7). |
| `nuzur-views-check.service` and `nuzur-views-check.timer` | Daily `gen-views.sh --check`. |
| `networkpolicy-mysql.yaml` | Section 8.1. |
| `egress.nft` and `nuzur-agent-egress.service` | Section 8.2. |
| `verify-p1.sh` | Box half of P1 (section 9.3). |
| `rotate-ro.sh` | Password rotation plus same-uuid re-seal. Could instead be `setup.sh --rotate-ro`. |

`deploy/README.md` gains one line in its migration runbook: "after any
`deploy/sql/*.sql`, run `deploy/nuzur-agent/gen-views.sh --apply`".

---

## 12. Open questions for the owner

1. **Q1: query timeout vs. keeping the DSN out of argv.** With CLI v1.9.2 you can have only one of these:
   - **(a)** the DSN via the masked prompt, as designed: no timeout and no database in the DSN;
   - **(b)** the DSN via `--dsn` from the 0600 file, which gets `readTimeout` and the database, but puts the password in the CLI's argv for about 2 seconds, readable through `/proc` by any local UID;
   - **(c)** a small nuzur-cli change first: an `EnvVar` or file source for `agent connection add --dsn`, in the style of `NUZUR_AGENT_DSN` on `agent start`. Then switch in place with the same `--uuid`.

   The design defaults to (a) now and (c) when it is released. Agree?
2. **Q2: plan headroom.** `metiche-prod` will be the owner's 3rd connection, which exhausts the Starter cap (section 3.A). A 4th connection anywhere will then fail to publish. Is the owner on Starter? If so, is having no headroom acceptable, or should the plan change before step 9? The agent serving another project stays untouched either way.
3. **Q3: three risky-name columns beyond the five redactions.**
   - `project.repo_url`: a URL someone typed can embed credentials. Options are `expose!`, `redact`, or expose with the userinfo stripped via `REGEXP_REPLACE`.
   - `agent.client_key`: it embeds a machine id.
   - `notification_channel.last_error`: capped, but it is next to a webhook.

   The default is `expose!` for all three, with the note in the policy.
4. **Q4: enable the owner-scoped egress rules (section 8.2)** together with the agent, or as a follow-up once the agent is proven? The default is follow-up.
