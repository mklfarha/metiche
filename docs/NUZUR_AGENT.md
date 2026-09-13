# nuzur agent for metiche production data

**Status:** design only. Nothing in this document exists yet: no MySQL user, no
views, no image, no chart, no pairing, no Secret. This file is the only thing
written.

**Scope:** let the owner browse metiche's production MySQL in the nuzur data
manager. Access is read-only and secret columns are removed. The nuzur agent
runs as a pod in the `metiche` namespace.

Citations use `path:line`. The roots are:

| prefix | location on the owner's laptop |
|---|---|
| `cli/` | `~/Dropbox/nuzur-24/code/nuzur-cli` (release v1.9.2, `constants/constants.go:3`) |
| `go/` | `~/Dropbox/nuzur-24/code/nuzur-go` |
| `web/` | `~/Dropbox/nuzur-24/code/nuzur-web` |
| `kr/` | `~/go/pkg/mod/github.com/99designs/keyring@v1.2.2` (pinned at `cli/go.mod:6`) |
| `dbus/` | `~/go/pkg/mod/github.com/godbus/dbus@v0.0.0-20190726142602-4481cbc300e2` (`cli/go.mod:26`) |
| `gostd/` | Go standard library source, `$(go env GOROOT)/src` (go1.26.2) |
| (none) | this repository |

Facts marked *(observed)* come from the box and nuzur on 2026-09-12. They were
gathered read-only: `get`/`describe`, `information_schema`, `listLocalAgents`,
and no values.

---

## 1. Context

metiche's data lives in `metiche-mysql`:

- a MySQL 8.0.46 StatefulSet in namespace `metiche`, with 25 tables in database `metiche`;
- reached only through the ClusterIP Service `metiche-mysql`, with no host port *(observed)*.

Today the only way to look at it is `kubectl exec` as root. The owner wants the
nuzur data manager for browsing, search and ad-hoc SELECTs.

The nuzur agent (`nuzur-cli agent start`) runs next to the database and dials
**out** to `cm.nuzur.com:443` over a gRPC stream. The cloud sends SQL down that
stream (`cli/agent/daemon.go:1-11`, `:184-238`). Nothing connects in.

**The agent is the thing to contain.** It executes whatever it is sent:

- queries (`cli/agent/handlers.go:33-94`)
- writes (`:97-120`)
- transactions (`:126-185`)

It has no read-only mode and no column filter. connection-manager does not help
either:

- Its PII masker applies only to queries with a nuzur project context on a
  project that declares PII
  (`go/connection-manager/module/sql-query-manager/policy.go:53-79`,
  `masker.go:41-55`).
- Its guard blocks hidden tables, not writes (`guard.go:27-49`).

The boundary is therefore **the MySQL account**, and section 5 builds it.

The repository is **public**. Scripts, the chart, the Dockerfile, the column
policy and SQL without credentials may be committed. Passwords, tokens, the
DSN, the machine-id and agent credentials never are.

---

## 2. Decisions

### Owner decisions (final)

| # | Decision |
|---|---|
| D1 | **Only the owner queries this connection.** It is never shared with a team. |
| D2 | **NULL in what nuzur sees:** `invite.code`, `notification_channel.target_url`, `team_event.response_snapshot`, `account.token_hash`, `agent.token_hash`. <br>**Visible:** `account.email`, `project.repo_url`, `agent.client_key`, `notification_channel.last_error`. The last three are acknowledged risky-name exposures (`expose!`). |
| D3 | **The agent runs in-cluster** as StatefulSet `nuzur-agent` with `replicas: 1`, in namespace `metiche`, from a Helm chart under `deploy/.helm/` like the other releases. |
| D4 | **A pod restart, reschedule, deletion or image upgrade never pairs and never creates a connection.** This is property **P1**, tested in section 10. |
| D5 | **No nuzur plan limit applies to this owner.** Setup has no connection-count stop. |
| D6 | **Restricting the agent's egress is a follow-up**, not part of the first rollout (section 7.3). |
| D7 | **The DSN, including the password, is passed once, as an argument, to `agent connection add --uuid <recorded> --dsn …`**, during the one-time manual `kubectl exec`. <br>That argument is visible only inside the pod's PID namespace and to root on the host, who can already read the Kubernetes Secret it came from. <br>**The password comes from a Kubernetes Secret, generated once at random, and is never typed by a person.** |
| D8 | **An agent serving another project already exists** in the owner's nuzur account. It is never revoked, unpaired, republished or touched by anything here. The metiche agent is identified **only by its recorded uuid**. |

### Decisions derived from the source

| # | Decision | Where |
|---|---|---|
| D9 | nuzur reads a separate database `metiche_nuzur` of views, through `nuzur_ro`. That account has `SELECT` on `metiche_nuzur.*` and nothing on `metiche`. | §5 |
| D10 | Views are an **allowlist with total classification.** An unclassified column stops the generator. | §5.1–5.2 |
| D11 | Registration uses the CLI's login-free path: provisioning-token pairing, then `agent connection add` *without* `--no-publish`, signed by the agent. No `nuzur-cli deploy`, and no laptop-side `UpdateLocalAgentConnections`. | §4.A |
| D12 | The whole nuzur config dir lives on the StatefulSet's PVC. `XDG_CONFIG_HOME`, `HOME` and `USER` are set explicitly. The pod hostname is `nuzur-agent-0`, and `/etc/machine-id` is a fixed file mounted from a Secret. | §4.C–4.D |
| D13 | The chart has two modes. **`setup`** keeps the pod alive for the one-time exec. **`run`** puts a preflight init container in front of `agent start` that refuses to start on any identity or fingerprint mismatch. Nothing ever pairs automatically. | §6.2 |
| D14 | `NUZUR_AGENT_DSN` and `NUZUR_AGENT_DRIVER` are **never** set on the pod. | §4.E |
| D15 | These are never run in the pod: `nuzur-cli connect`, `agent install`, `agent pair --force`, `nuzur-cli login`. | §4.B |

---

## 3. Why a pod, and what nuzur's own deploy does differently

`nuzur-cli deploy` provisions a VM and runs the agent as a **host systemd
service**. Its bootstrap (`cli/deploy/templates/bootstrap.sh.tmpl`) does five
things:

1. Installs the CLI on the host from the GitHub release, verified by checksum (`:407-453`).
2. Pairs the host if `agent status` shows no uuid (`:455-465`).
3. Registers the database with `agent connection add '<name>' --uuid … --dsn "<user>:${DB_PASSWORD}@tcp(…)" --no-publish --non-interactive` (`:467-471`). The password is expanded into the argv of a **host** process.
4. Writes the same full DSN, password included, as `NUZUR_AGENT_DSN` into `/etc/nuzur/agent.env` (`:473-480`). This feeds the legacy fallback DSN that §4.E forbids here.
5. Installs a root unit with `Restart=always`, `HOME=/root` and `USER=root` that loads that env file (`:481-499`), and restarts it on every deploy (`:500-503`).

That shape fits a VM nuzur owns end to end. This design is **the in-cluster
variant**, for a database that already lives in Kubernetes:

| | nuzur deploy (host) | this design (pod) |
|---|---|---|
| Lifecycle | bootstrap script plus systemd | Helm release next to `metiche`, `metiche-web` and `metiche-mysql` |
| Identity | root, `HOME=/root` | uid 10001, read-only root filesystem, no service-account token |
| State | `/root/.config/nuzur` | PVC `data-nuzur-agent-0` |
| Keyring passphrase inputs | host hostname, host machine-id, `USER=root` | pod hostname `nuzur-agent-0`, chart-mounted machine-id, `USER=nuzur` |
| Fallback DSN | plaintext `NUZUR_AGENT_DSN` in an env file | never set; preflight refuses if present |
| Password in argv | host process namespace | pod process namespace only (D7) |
| Reaching MySQL | local socket or TCP on the host | Service DNS name; the MySQL NetworkPolicy selects the agent **by pod label** |
| Database account | the application user | `nuzur_ro`, SELECT on views only |

A host agent would reach the ClusterIP from the node's address, and every other
host process shares that address. A pod can be singled out by label.

---

## 4. What the source says

### A. Registering without `nuzur-cli deploy`

**Pairing needs no login on the machine.**

- `agent pair` accepts `--provisioning-token` (env `NUZUR_PROVISIONING_TOKEN`) (`cli/app/command_agent.go:56-60`, `:82-85`). The token is exchanged with `ExchangeProvisioningToken` (`:146-168`).
- With no display, `agent pair` without a token asks for it in a **masked** prompt (`:89-92` → `cli/app/command_connect.go:127-158`; masking at `:137` and `cli/app/command_agent_start.go:264-272`).
- A container has no `DISPLAY` on Linux, which counts as headless (`cli/app/headless.go:28-35`). So `kubectl exec -it … nuzur-cli agent pair` prompts, and **the token is never in argv**.
- The server consumes the token atomically. It lives 15 minutes and is single use (`go/product/server/provisioning_token.go:23`, `go/product/server/local_agent.go:151-184`).
- Each exchange inserts **one new** `local_agent` row (`local_agent.go:72-115`). The CLI writes the uuid and token files at 0600 in a 0700 directory (`cli/app/command_agent.go:270-281`).
- **The only login is the owner's**, in a browser on the laptop, to mint the token at `app.nuzur.com/pair` ("Pair a server").

**Publishing without `--no-publish` uses the agent's own credentials.**

- `agent connection add` saves the entry, then calls `publishCatalog` (`cli/app/command_agent_connection.go:139-144`).
- With no user token file present, `publishCatalog` signs with the agent's uuid and token (`:236-245`, `:262-288`) through `PublishLocalAgentCatalog` (`:314-332`).
- The server authenticates that call by the agent token alone (`go/product/server/local_agent.go:272-288`, `:361-396`).
- `--no-publish` suppresses exactly that call (`command_agent_connection.go:134-138`).

**Catalog replacement is harmless here.**

- Both server RPCs replace the agent's whole catalog (`local_agent.go:248-270`, `:290-338`).
- The CLI always sends its whole registry (`command_agent_connection.go:249-266`).
- This agent holds exactly one connection, `metiche-prod`, so every publish replaces `[metiche-prod]` with `[metiche-prod]`.
- Team shares are server-owned. The server strips any the client sends (`local_agent.go:434-439`), so a republish can never share the connection.

**Agent name.**

- There is no flag to set one. `agent pair` accepts only `--force`, `--provisioning-token` and `--headless` (`cli/app/command_agent.go:51-65`).
- The machine name is `os.Hostname()` (`:150-156`), so in the pod it is **`nuzur-agent-0`** (§4.D). That is distinct from the existing agents, whose names are `localhost` and the laptop's name *(observed)*.
- The server writes the name only when it creates the row (`local_agent.go:89-100`).
- Every action still uses the **recorded uuid**, never a name (D8).

### B. Restart semantics and every duplicate path

**The container's command is `nuzur-cli agent start`. At startup it does this, and nothing else:**

1. **CLI construction.**
   - The config is an embedded YAML (`cli/config/config.go:13-14`, `:36-49`).
   - The auth client only populates a struct (`cli/auth/auth.go:22-29`).
   - The gRPC clients are created with `grpc.NewClient`, which is lazy (`cli/productclient/client.go:48`, `cli/cmclient/client.go:62`).
   - No files and no network.
2. **The `Before` hook.** It migrates `/tmp/nuzur-cli` files only when that directory exists, and never overwrites (`cli/app/command_agent.go:26-31`, `cli/files/local_agent.go:84-127`). `/tmp` is a fresh emptyDir on every pod start, so the migration returns at `:90-95`.
3. **Fallback resolution** (`cli/app/command_agent_start.go:116-133`). No flag or env is set and there are no saved fallback files. The registry has an entry, so it returns empty **without prompting** (`:127-130`).
4. **`agent.Run`** (`cli/agent/daemon.go:76-172`):
   1. Read the uuid and token files, or exit with "agent not paired: … (run `nuzur-cli agent pair` first)" (`:77-80`, `:346-358`).
   2. Load the registry read-only (`cli/agent/connections/connections.go:67-116`). It rewrites the file only to migrate a pre-keychain legacy file (`:94-98`).
   3. Open the keyring. This writes and deletes one probe item (`cli/agent/connections/keyring.go:78-85`, `:119-130`).
   4. Dial and send `Hello{uuid, token}` (`daemon.go:193-203`).
5. **The server side.**
   - It validates the token (`go/connection-manager/server/local_agent_channel.go:137-174`).
   - It registers an in-memory session (`:69-70`).
   - It runs one `UPDATE local_agent SET status, last_seen_at, updated_at` (`:186-227`). That statement cannot touch `connections` or `token_hash`.
6. **Reconnects** reuse the same uuid and token with backoff (`daemon.go:129-171`).

None of `RegisterLocalAgent`, `ExchangeProvisioningToken`, `PublishLocalAgentCatalog` or `UpdateLocalAgentConnections` is reachable from this path.

**Why identity survives restarts.**

- The connection uuid is a field of the entry in `local_agent_connections.json` (`connections.go:42-55`).
- That file is written only by `Save` (`:118-129`), which only `connection add` and `connection remove` call.
- The DSN is keyed by that uuid, as keyring item `dsn-<uuid>` (`keyring.go:189-191`).
- Everything sits on the PVC (§4.C).
- The same pod name, the same mounted machine-id and the same `USER` give the same passphrase (§4.D).
- A restart, reschedule, delete or new image therefore finds identical state, and step 5's `UPDATE` changes only status and timestamps.

**Duplicate paths and their prevention:**

| # | How it arises | Source | Prevention |
|---|---|---|---|
| 1 | Re-running `agent pair` | Refused when the uuid file exists (`cli/app/command_agent.go:73-80`). `--force` inserts a second row (`local_agent.go:72-115`). | Setup checks the uuid file on the PVC first. `--force` is never used. The token is single use. |
| 2 | `nuzur-cli connect` | Replaces by name and adds **without** a uuid, so a new uuid on every run (`cli/app/command_connect.go:182-184`, `:230-236`, `connections.go:162-164`). It also tries to install a service (`:263-281`). | Never run (D15). |
| 3 | `agent install` | Writes a `systemd --user` unit (`cli/agent/install.go:164-211`). Meaningless in a container. | Never run (D15). |
| 4 | Scripted `connection add` without `--uuid` | Scripted mode (`--dsn`, `--driver` or `--non-interactive`, `command_agent_connection.go:62`) upserts by name. Without `--uuid` it mints a new uuid (`:70-78`, `:118-125`). | `--uuid` from ConfigMap `nuzur-agent-ids` is always passed. Setup skips when the entry exists. |
| 5 | A user login in the pod | A user token file makes publishing use the user path, which **re-pairs** on NotFound (`command_agent_connection.go:271-281`, `:290-309`). `agent unpair` without `--keep-remote` also logs in (`cli/app/command_agent_unpair.go:41-47`). | Never log in (D15). Preflight fails if `$XDG_CONFIG_HOME/nuzur/token.txt` exists (`cli/files/token.go:15-17`). |
| 6 | Laptop `UpdateLocalAgentConnections` | Replaces the catalog (`local_agent.go:256-270`). | Not used. |
| 7 | The passphrase changes | Changing the hostname, machine-id or `USER` makes the keyring item undecryptable, so `connections.Load` fails (`connections.go:109-112`). The daemon continues with an empty registry (`daemon.go:92-97`). `agent start` would try to prompt (`command_agent_start.go:128-132`) and fail with no TTY, before saving anything (`:176`). **An outage, not a duplicate.** | Preflight compares the recorded fingerprint and decrypts once before `agent start` runs. Recovery keeps the uuid (§4.D). |
| 8 | **PVC loss** | uuid and token are gone. The server stores only the token's hash (`local_agent.go:33-45`), so the pairing is unrecoverable. | The one documented **manual re-pair** (§8, "PVC loss"): new agent uuid, same connection uuid, old agent revoked **by recorded uuid**. Never automatic. |
| 9 | The config dir resolves off the PVC | If `XDG_CONFIG_HOME` and `HOME` are unset, or `XDG_CONFIG_HOME` is relative, `UserConfigDir` errors (`gostd/os/file.go:582-591`). The CLI then falls back to `/tmp/nuzur-cli` (`cli/files/local_agent.go:20-25`, `:33-38`): ephemeral, lost on restart, "not paired", and a temptation to re-pair. | The chart sets an absolute `XDG_CONFIG_HOME` on the PVC. Setup checks it before pairing, and preflight checks it on every start. |
| 10 | Two daemons with one pairing | Sessions are keyed by agent uuid (`local_agent_channel.go:69-70`), so two daemons flap. | `replicas: 1` is hard-coded in the template. StatefulSet at-most-one semantics. Never force-delete the pod. Never exec `agent start`. |
| 11 | The legacy fallback env is set | A DSN in env applies to any uuid the registry lacks (`cli/agent/dbpool.go:84-86`). It is also saved in **plaintext** on the PVC (`command_agent_start.go:118-119`, `:176`, `:291-303`). | Never set (D14). Preflight fails if either env var or either saved file is present. |

### C. Persistence: exactly which path, and set explicitly

- **`os.UserConfigDir` on Linux** (`gostd/os/file.go:581-591`):
  - returns `$XDG_CONFIG_HOME` if it is set;
  - errors if that value is relative;
  - otherwise returns `$HOME/.config`;
  - errors if both are unset.
- **The CLI** uses `<UserConfigDir>/nuzur` as its base (`cli/files/local_agent.go:20-25`) and `<UserConfigDir>/nuzur/agent` for agent state (`:33-38`). On error, both fall back to `/tmp/nuzur-cli`.
- **The chart sets:**
  - `HOME=/var/lib/nuzur`
  - `XDG_CONFIG_HOME=/var/lib/nuzur/config`
  - the PVC mounted at `/var/lib/nuzur`

  `HOME` is then never consulted for config.
- **Layout on the PVC** (file names from `cli/constants/constants.go:4-9`):

| path | content | written by |
|---|---|---|
| `/var/lib/nuzur/config/nuzur/agent/local_agent_uuid.txt` | agent uuid | pair (`cli/app/command_agent.go:270-281`) |
| `…/agent/local_agent_token.txt` | agent token, 0600 | pair |
| `…/agent/local_agent_connections.json` | registry: uuid, name, driver, db_type, default_schema. No DSN. | `connection add`/`remove` (`connections.go:118-129`) |
| `…/agent/keyring/dsn-<CONNECTION_UUID>` | encrypted DSN, 0600, in a 0700 dir | `PutDSN` (`keyring.go:159-163`, `:193-206`; `kr/file.go:46`, `:146`) |
| `/var/lib/nuzur/config/nuzur/token.txt` | user login token. **Must never exist.** | `nuzur-cli login` (`cli/files/token.go:15-17`) |

- **Storage.** PVC `data-nuzur-agent-0` comes from `volumeClaimTemplates`: 1Gi, `ReadWriteOnce`, storage class `microk8s-hostpath`.
  - That class has reclaim policy **Delete** and uses the `microk8s.io/hostpath` provisioner, which creates hostPath directories mode 0777 root:root *(observed)*. So uid 10001 can write, and the CLI creates its own 0700 subdirectories.
  - `fsGroup` is set, but hostPath volumes ignore it.
  - `ReadWriteOncePod` is not available on this non-CSI provisioner, so single-writer rests on StatefulSet semantics.
  - `persistentVolumeClaimRetentionPolicy` is `Retain` on both delete and scale, so `helm uninstall` keeps the PVC.
- **PVC loss is the documented manual re-pair case** (duplicate path 8). Deleting the PVC deletes the pairing, because the reclaim policy is Delete.

### D. A stable keyring passphrase in a pod

**Inputs** (`cli/agent/connections/keyring.go:170-187`): `sha256( os.Hostname() ‖ bytes of /etc/machine-id ‖ $USER ‖ "nuzur-cli-keyring-v1" )`.

An input that cannot be read is **silently skipped**. That changes the
passphrase, so each input must be guaranteed present.

**Hostname.**

- Go's `os.Hostname` on Linux returns `uname().nodename` (`gostd/os/sys_linux.go:12-30`). In a pod, that is the pod's UTS hostname.
- Kubernetes sets it to the pod name when `spec.hostname` is unset. For a StatefulSet the pod name is `<statefulset>-<ordinal>`, so **`nuzur-agent-0`**.
- **Setting `hostname` or `subdomain` is not needed.** `subdomain` affects only DNS, not the short nodename.
- The chart must **not** set `spec.hostname`, `setHostnameAsFQDN: true` (which would put the FQDN into nodename) or `hostNetwork: true` (which would use the node's hostname).
- The StatefulSet name is fixed in the template, not derived from the release name. Renaming it is a passphrase change.
- `kubectl exec` runs in the same UTS namespace, so pairing and adding from an exec see the same hostname as the daemon.

**Machine-id.**

- A random 128-bit value is generated **once at setup** as 32 lowercase hex characters followed by `\n`.
- It is stored root-only at `/etc/metiche/nuzur-agent.machine-id` (0600) and loaded into Secret `nuzur-agent-machine-id` (key `machine-id`).
- It is mounted read-only with `subPath` at `/etc/machine-id` (`defaultMode 0444`).
- The code calls it "NOT a cryptographic secret" (`keyring.go:165-169`). But hostname and `USER` are public, so the machine-id is the only non-public input. It therefore lives in a Secret, not in values (Helm stores values in the release and `helm get values` prints them), not in a ConfigMap, and never in the repo.
- It is never regenerated while the file or Secret exists. Setup stops if the two differ.
- The trailing newline is part of the passphrase, so the value must never be retyped.

**`USER`.**

- The chart sets `USER=nuzur` and `LOGNAME=nuzur`.
- The code reads the environment variable (`keyring.go:182-184`), not the uid.
- `kubectl exec` processes inherit the container's env, so the one-time commands use the same value.

**The backend in a container is the encrypted file backend.** From source:

1. Linux allows, in order, Secret Service, KWallet, File (`keyring.go:146-151`).
2. Secret Service and KWallet register themselves only if `dbus.SessionBus()` succeeds at package init (`kr/secretservice.go:17-24`, `kr/kwallet.go:18-29`).
3. godbus looks for a session bus in three places (`dbus/conn.go:79-88`, `:91-98`):
   - `DBUS_SESSION_BUS_ADDRESS` (not set);
   - a bus socket under the runtime dir (`dbus/conn_other.go:51-71`; there is no `/run/user/10001` in the image);
   - running `dbus-launch` (`dbus/conn_other.go:19-38`; not in the image).
4. All three fail, so neither dbus backend is registered.
5. The File backend is always registered (`kr/file.go:14-21`).
6. `keyring.Open` returns the first registered allowed backend (`kr/keyring.go:58-74`), which is **File**.
7. The Linux write probe (`keyring.go:78-85`) confirms it. On a read-only mount, such as the preflight's, the probe fails, and the code reopens File without probing (`:84`). Reads then decrypt normally (`kr/file.go:71-97`).

**If the passphrase changes, recover with the same uuid.**

1. **Preferred:** restore the input. Put the original machine-id back into the Secret, from `/etc/metiche/nuzur-agent.machine-id`, and restore the StatefulSet name or `USER`.
2. **Otherwise, re-seal:**
   1. `helm upgrade --set-string agent.mode=setup`.
   2. In the pod, delete `keyring/dsn-<uuid>`. The file backend's `Remove` is a plain unlink that needs no passphrase (`kr/file.go:158-165`), and a missing item is a soft state (`keyring.go:211-224`), so the registry loads again.
   3. Run §8 step 10 again (a scripted add with **the same `--uuid`**, which upserts in place and publishes the same one-entry catalog).
   4. Update the fingerprint in ConfigMap `nuzur-agent-ids`.
   5. Switch back to `run` and run P1.

### E. The legacy fallback DSN is never set

- `agent start` reads `--dsn`/`NUZUR_AGENT_DSN` and `--driver`/`NUZUR_AGENT_DRIVER` (`cli/app/command_agent_start.go:35-44`).
- If either is set, it wins regardless of the registry. It is **saved in plaintext** to `local_agent_dsn.txt` and `local_agent_driver.txt` (`:117-119`, `:143-180`, `:291-303`).
- The daemon then serves that DSN for any connection uuid the registry does not know (`cli/agent/dbpool.go:84-86`).
- With neither set and a non-empty registry, the fallback is skipped silently (`:121-130`).
- **Rule:** the chart never sets either variable. Preflight fails if either is present in the environment, or if either saved file exists on the PVC.
- For contrast, nuzur's own deploy sets it (§3).

### F. Default schema, `USE`, and `SELECT *`

**`USE` per request.**

- The agent issues ``USE `schema` `` on a dedicated connection whenever the request carries a schema (`cli/agent/handlers.go:197-247`), and once at `BeginTx` (`:137-143`).
- The schema is the data manager's saved connection schema (`go/connection-manager/module/sql-connection-manager/connection_local_agent.go:112`, `:136`).
- If that is empty, it is the catalog's `default_schema` (`manager.go:199-203`). The scripted add sets that with `--schema` (`cli/app/command_agent_connection.go:96`).
- The daemon itself ignores the registry's default schema (`cli/agent/dbpool.go:54-82`).
- With `--schema metiche_nuzur` and a DSN pointing at database `metiche_nuzur`, every path lands on the views.
- Selecting `metiche` fails closed, because `nuzur_ro` has no grant there.

**`SELECT *`.**

- The data manager's default table action is `"SELECT * FROM " + entity.identifier + " LIMIT 100"` (`web/src/project-data-manager/entities_list.tsx:108`).
- Fetching records by key is `SELECT * FROM … WHERE` (`web/src/domain/entity.ts:507-508`).
- Search names its fields explicitly (`web/src/project-data-manager/search_query.ts:35-38`).
- MySQL rejects `SELECT *` when any column is not granted, so **column-level GRANTs cannot work**.
- Views that keep every column name, with redacted columns as typed NULLs, serve both query styles.

---

## 5. Data exposure model

```
metiche                         untouched; nuzur_ro has no grant here
  ▲ SELECT
nuzur_views@localhost           ACCOUNT LOCK; SELECT ON metiche.*; DEFINER of every view
  ▲ SQL SECURITY DEFINER
metiche_nuzur                   one view per table, same name, every column (secrets as NULL)
  ▲ SELECT
nuzur_ro@'10.1.0.0/255.255.0.0' SELECT ON metiche_nuzur.*; MAX_USER_CONNECTIONS 5
  ▲ DSN (encrypted on the PVC)
pod nuzur-agent-0 ──► cm.nuzur.com:443 ──► data manager (owner only)
```

**Two accounts.**

- The definer can read `metiche` but cannot log in.
- The login account can read only the views.
- A write through a view needs write privileges for both the invoker (on the view) and the definer (on the base table). Neither account has them, so even a future mistaken `GRANT ALL ON metiche_nuzur.*` could not write to `metiche`.

**Host part.**

- The agent connects from its **pod IP**. Pod-to-Service traffic is not masqueraded: kube-proxy masquerades only sources outside `--cluster-cidr=10.1.0.0/16` *(observed)*.
- Pod IPs change on reschedule, so the host part is the pod pool `10.1.0.0/255.255.0.0` (Calico IPPool `10.1.0.0/16`) *(observed)*.
- `skip_name_resolve=1` *(observed)*, so a hostname cannot be used.
- The effective restriction is **password plus the NetworkPolicy** (§7.1), which lets only the agent pod and the backend reach 3306.

**DSN host.** The Service DNS name `metiche-mysql.metiche.svc.cluster.local`, not the ClusterIP. It survives a reinstall of the mysql release.

**Limits.**

- `MAX_USER_CONNECTIONS 5`, equal to the agent's pool maximum (`cli/agent/dbpool.go:116`). The server allows 200 *(observed)*.
- A query timeout in the DSN: `timeout=5s&readTimeout=60s&writeTimeout=60s` (go-sql-driver v1.9.3, `cli/go.mod:7`).
- `max_execution_time` is 0 globally *(observed)*. It cannot be set per account, and setting it globally would also cap the backend, so this design leaves it alone.

### 5.1 Allowlist, not denylist

| Option | When a column is added (as v4 added `agent.token_hash`, `deploy/sql/2026-09-agent-token-hash.sql`) | Verdict |
|---|---|---|
| `SELECT *` views, or a denylist | New columns are exposed automatically, secrets included. | **Rejected** |
| Allowlist that silently omits unknown columns | Never exposed, but silently stale, and explicit-field queries fail. | Rejected |
| **Allowlist with total classification** | The generator refuses until a human writes `expose` or `redact`. Existing views stay, stale but safe. | **Chosen** |

### 5.2 Column policy and view generator

**Policy file.** `deploy/nuzur-agent/columns.policy` is committed and holds no
secrets. One line per column of `metiche`:

```
account.token_hash                 redact     D2
agent.token_hash                   redact     D2 (added v4)
invite.code                        redact     D2 join code
notification_channel.target_url    redact     D2 webhook URL is a credential (docs/MODEL.md:63-66)
team_event.response_snapshot       redact     D2
account.email                      expose     D2 owner-only viewer
project.repo_url                   expose!    D2 owner decision
agent.client_key                   expose!    D2 identifier, not a bearer (docs/IDENTITY.md:9-11,38)
notification_channel.last_error    expose     D2 capped status line (docs/MODEL.md:66)
...every other column...           expose | expose!
```

**Decisions.**

- `expose`: the column appears as is.
- `redact`: the column appears as a typed NULL under the same name. `CAST(NULL AS CHAR)` for char and text types, `CAST(NULL AS JSON)` for json, plain `NULL` otherwise.
- `expose!`: an acknowledged exposure of a risky-looking name.
- A table with no lines gets no view.

**Risky-name check.**

- Any name matching `/(token|secret|code|key|pass|pwd|url|uri|snapshot|hash|salt|credential|cookie|signature|private|webhook|dsn|auth)/i` must be `redact` or `expose!`.
- Today, besides the five redactions, that means:
  - every `*.key` column, plus `contract.key_norm`;
  - `agent.client_key`, `conflict.dedupe_key`, `judgement.pair_key`, `team_event.subject_key`, `team_event.idempotency_key`;
  - `decision_token.token` and `intent_token.token`, which hold tokenized wording (`docs/MODEL.md:35,46`);
  - `contract_assertion.shape_hash`;
  - `project.repo_url`.
- `last_error` does not match the pattern, so it is plain `expose`.

**Generator.** `deploy/nuzur-agent/gen-views.sh [--check|--apply]` runs as root on
the box. Like `deploy/scripts/apply-schema.sh`, it runs `mysql` inside the pod
via `kubectl exec`, with the root password from the pod's environment and SQL on
stdin. It never selects a row value.

1. Read `information_schema.columns` for `metiche` (names, ordinal positions, data types).
2. **Fail before any DDL** if any of these hold, printing each offender:
   - an unclassified column;
   - a table with no lines;
   - a line whose column no longer exists (the view would error with 1356);
   - a risky name marked plain `expose`;
   - a malformed or duplicate line.
3. For each table, in ordinal order, emit
   ``CREATE OR REPLACE ALGORITHM=MERGE DEFINER=`nuzur_views`@`localhost` SQL SECURITY DEFINER VIEW `metiche_nuzur`.`<t>` AS SELECT … FROM `metiche`.`<t>`;``
   Add `DROP VIEW` for views that are no longer expected.
4. `--check` exits 0 only if the policy is total and the SQL's sha256 equals the value recorded in ConfigMap `nuzur-agent-views`.
5. `--apply` executes the SQL and records the hash. It is idempotent.

**When it runs.**

- Last step of every `deploy/sql/*.sql` migration and of `deploy/scripts/apply-schema.sh`. apply-schema never adds columns (`deploy/scripts/apply-schema.sh:12-16`), so migrations are applied by hand.
- In CI, against the committed `code/backend/metiche/core/repository/sql/schema/create.sql`. That check needs no database.
- Daily drift check: a host systemd timer on the box runs `gen-views.sh --check` through `kubectl exec`, like `apply-schema.sh`, in addition to the CI check above (§13 Q2).

---

## 6. Deployment design

### 6.1 Image: `deploy/docker/nuzur-agent.Dockerfile`

**Where it is built.** On the box, like the other images: no registry, imported
into containerd, `imagePullPolicy: Never` (`deploy/scripts/build-images.sh:1-33`).
`build-images.sh` gains a `nuzur-agent` target, tagged `nuzur-agent:<cli-version>-<git short sha>`.

**Stage 1, fetch.** It uses `alpine:3.22.1`, the same base as
`deploy/docker/metiche-web.Dockerfile`, with `curl` added.

- **Artifact name.** GoReleaser's archive template (`cli/.goreleaser.yaml:30-39`) yields `nuzur-cli_Linux_x86_64.tar.gz` for linux/amd64.
- **Checksums file.** The yaml has no `checksum:` block, so GoReleaser's default name applies: `nuzur-cli_1.9.2_checksums.txt`. nuzur's bootstrap uses exactly these names (`cli/deploy/templates/bootstrap.sh.tmpl:414`, `:432`, `:438`).
- **Both come from the release** at `https://github.com/nuzur/nuzur-cli/releases/download/v1.9.2/`.
- **Pin the tarball's sha256 in the repo** as build ARG `NUZUR_CLI_SHA256`, and verify it **as well as** the release checksums file, before `tar`. The releases use `release: mode: replace` (`cli/.goreleaser.yaml:45-49`), which allows assets to be re-uploaded, so the release's own checksums alone are not a pin.
- **Build fails** on any mismatch.
- **Static binary.** The build is `CGO_ENABLED=0` (`cli/.goreleaser.yaml:23-24`), so it runs on alpine with no libc concerns.

**Stage 2, runtime.** `alpine:3.22.1`.

- `apk add --no-cache ca-certificates`. The CLI verifies TLS for `cm.nuzur.com` and `product.nuzur.com` against the system pool (`cli/cmclient/client.go:36-40`, `cli/productclient/client.go:36-40`).
- User and group `nuzur`, uid and gid 10001, home `/var/lib/nuzur`, shell `/sbin/nologin`.
- `COPY nuzur-cli /usr/local/bin/nuzur-cli`, root-owned, 0755.
- `COPY nuzur-agent-preflight nuzur-agent-hold /usr/local/bin/`: two small POSIX sh scripts, run by busybox.
- `USER 10001:10001`
- `ENTRYPOINT ["/usr/local/bin/nuzur-cli"]`, `CMD ["agent", "start"]`
- No dbus and no `dbus-launch`, which guarantees the file keyring (§4.D).

### 6.2 Chart: `deploy/.helm/nuzur-agent`

The chart follows the conventions of the existing charts: `_helpers.tpl` labels,
`existingSecret` references, no secret values, and `automountServiceAccountToken: false`
(`deploy/.helm/metiche-mysql/values.yaml:148-170`, `deploy/.helm/metiche/values.yaml:328`).

**Templates.**

| template | content |
|---|---|
| `serviceaccount.yaml` | SA `nuzur-agent`, `automountServiceAccountToken: false` |
| `service.yaml` | headless Service `nuzur-agent` (`clusterIP: None`, no ports). It exists only because a StatefulSet names a governing Service. It exposes nothing. |
| `statefulset.yaml` | see below |

**No** Role, RoleBinding, Ingress, ConfigMap or Secret. Setup creates the
Secrets and ConfigMaps (§8), so a Helm upgrade can never overwrite them.

**Values.** They contain nothing secret.

```yaml
agent:
  mode: ""                 # REQUIRED: "setup" or "run" — template fails on anything else
image:
  repository: nuzur-agent
  tag: ""                  # required, --set-string
  pullPolicy: Never
machineIdSecret: nuzur-agent-machine-id   # key: machine-id
idsConfigMap: nuzur-agent-ids             # AGENT_UUID, CONNECTION_UUID, EXPECT_HOSTNAME, EXPECT_USER, MACHINE_ID_SHA256
roPasswordSecret: nuzur-agent-db          # key: NUZUR_RO_PASSWORD — mounted in setup mode only
persistence: { storageClass: microk8s-hostpath, size: 1Gi }
resources:
  requests: { cpu: 10m, memory: 32Mi }
  limits:   { memory: 256Mi }
```

**StatefulSet spec, fixed in the template.**

- `metadata.name: nuzur-agent`, **literal**. This is the hostname (§4.D).
- `replicas: 1`, **literal**, not a value.
- `serviceName: nuzur-agent`.
- `persistentVolumeClaimRetentionPolicy: {whenDeleted: Retain, whenScaled: Retain}`.
- `volumeClaimTemplates: data` (RWO, 1Gi, `microk8s-hostpath`).
- Pod labels `app.kubernetes.io/name: nuzur-agent` and `app.kubernetes.io/instance: nuzur-agent`. These are the labels the MySQL NetworkPolicy selects.

**Pod spec.**

- `automountServiceAccountToken: false` and `enableServiceLinks: false`.
- **Not set:** `hostname`, `subdomain`, `setHostnameAsFQDN`, `hostNetwork`, `hostPID`, `shareProcessNamespace`.
- `securityContext: {runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001, seccompProfile: {type: RuntimeDefault}}`.
- `terminationGracePeriodSeconds: 30`.

**Volumes.**

| volume | source | mounted at |
|---|---|---|
| `data` | the PVC | `/var/lib/nuzur` |
| `tmp` | `emptyDir {sizeLimit: 16Mi}` | `/tmp` |
| `machine-id` | Secret `nuzur-agent-machine-id`, item `machine-id`, `defaultMode: 0444` | `/etc/machine-id`, `subPath: machine-id`, `readOnly: true` |
| `ro-password` | Secret `nuzur-agent-db`, `defaultMode: 0440`. **Only rendered when `agent.mode=setup`.** | `/run/secrets/nuzur-ro` |

**Common container settings** (`agent` container and `preflight` init container).

- **Env:**
  - `HOME=/var/lib/nuzur`
  - `XDG_CONFIG_HOME=/var/lib/nuzur/config`
  - `USER=nuzur`
  - `LOGNAME=nuzur`

  Nothing else. No `NUZUR_*` variable is ever set.
- **securityContext:** `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities: {drop: [ALL]}`.
- **Probes: none.** The agent listens on nothing, reconnects on its own (`cli/agent/daemon.go:129-171`), and exits on non-retryable conditions (`:141-151`). A liveness probe could only restart it, which adds nothing.

**`agent.mode=setup`, for the one-time exec.**

- No init container.
- The `agent` container runs `nuzur-agent-hold`. Every 60 seconds it prints one line of local state (paired? how many registry entries? keyring item present?) and then sleeps. It never runs `nuzur-cli` except `agent status`, which is local only (`cli/app/command_agent_install.go:52-94`).
- The Secret mount `ro-password` is present, so step 10 can build the DSN inside the pod.

**`agent.mode=run`.**

- **Init container `preflight`:**
  - same image;
  - PVC mounted **`readOnly: true`**, which proves it writes nothing;
  - `machine-id` mounted;
  - `envFrom: configMapRef nuzur-agent-ids`;
  - runs `nuzur-agent-preflight`.
- **Container `agent`:** runs `nuzur-cli agent start`. No password Secret, no ids ConfigMap.

**Preflight checks.** It runs as uid 10001 with the same env and the same pod
hostname as the daemon. It exits 1 on the **first** failure with a single
explicit sentence, so the pod shows `Init:CrashLoopBackOff` and
`kubectl logs nuzur-agent-0 -c preflight` says why:

1. **Fingerprint:**
   - `hostname` is `nuzur-agent-0` and equals `EXPECT_HOSTNAME`;
   - `$USER` equals `EXPECT_USER`;
   - `sha256sum /etc/machine-id` equals `MACHINE_ID_SHA256`;
   - `XDG_CONFIG_HOME` is `/var/lib/nuzur/config` and lies on the mount.
2. **Pairing:** `local_agent_uuid.txt` exists and equals `AGENT_UUID`, and `local_agent_token.txt` is non-empty.
   - Message: "agent not paired on this PVC; this pod never pairs itself — follow docs/NUZUR_AGENT.md §8 step 9 (or 'PVC loss')".
3. **Registry:**
   - exactly one entry, whose uuid is `CONNECTION_UUID` and whose name is `metiche-prod`;
   - `keyring/dsn-$CONNECTION_UUID` exists.
4. **Decryption:** `nuzur-cli agent connection list` exits 0, and its output contains `CONNECTION_UUID`. The output is discarded; only a match is logged.
   - This runs the real `connections.Load`, which fails if the passphrase no longer decrypts the item (`connections.go:105-114`).
   - `list` never publishes (`cli/app/command_agent_connection.go:150-175`).
   - At startup it touches no network (§4.B, step 1).
5. **Forbidden state:**
   - `NUZUR_AGENT_DSN`, `NUZUR_AGENT_DRIVER` and `NUZUR_PROVISIONING_TOKEN` are unset;
   - `local_agent_dsn.txt`, `local_agent_driver.txt` and `config/nuzur/token.txt` are absent;
   - `/tmp/nuzur-cli` is absent.

**What the operator sees when something is wrong.**

- **Preflight passes but `agent start` still exits,** for example the token was revoked or the CLI is too old: the pod goes to `CrashLoopBackOff`, and the daemon's own message is in `kubectl logs`. That is "agent not paired" (`daemon.go:79`), a server rejection (`:147-151`) or CLI-too-old (`:220-225`).
- **In no failure mode** does anything pair, add or publish.
- **Exit on SIGTERM.** `agent start` exits 1 on SIGTERM (`daemon.go:133-135`, `cli/main.go:17`), which is harmless for pod termination.

### 6.3 NetworkPolicy template, in the metiche-mysql chart

See §7.1. It belongs in `deploy/.helm/metiche-mysql`, because it selects the
MySQL pods. It is off by default and enabled by `networkPolicy.enabled=true`.

---

## 7. Network

### 7.1 Ingress to metiche-mysql: backend and agent only

There is no policy in `metiche` today, so any pod in the cluster can reach 3306
*(observed)*. New template `deploy/.helm/metiche-mysql/templates/networkpolicy.yaml`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: metiche-mysql-ingress
spec:
  podSelector:
    matchLabels: { app.kubernetes.io/name: metiche-mysql, app.kubernetes.io/instance: metiche-mysql }
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:   # the backend
            matchLabels: { app.kubernetes.io/name: metiche, app.kubernetes.io/instance: metiche }
        - podSelector:   # the nuzur agent
            matchLabels: { app.kubernetes.io/name: nuzur-agent, app.kubernetes.io/instance: nuzur-agent }
      ports: [{ protocol: TCP, port: 3306 }]
```

**Where these labels come from.**

- **Backend:** `metiche.selectorLabels` (`deploy/.helm/metiche/templates/_helpers.tpl:40-43`) with release `metiche`. Observed on the running Deployment.
- **MySQL:** the StatefulSet pod labels *(observed)*.

**Nothing else breaks.**

- `metiche-web` has no database (`deploy/.helm/metiche-web/values.yaml:7`, `:92`, `:116`).
- No chart contains a Job, CronJob or Helm hook (a grep of `deploy/.helm` finds none), and the repo has no backup job.
- `apply-schema.sh` and `gen-views.sh` use **`kubectl exec`** (`deploy/scripts/apply-schema.sh:66-70`). Exec goes API server → kubelet → container runtime, **not the pod network**, so it is unaffected.
- The MySQL readiness and liveness probes are `exec` (`mysqladmin ping -h 127.0.0.1` inside the container) *(observed)*, so they are unaffected too.

**Test.** Revert with `helm upgrade … --set networkPolicy.enabled=false`.

1. `helm upgrade metiche-mysql … --set networkPolicy.enabled=true --dry-run`, then apply.
2. The backend works: no DB errors in its logs, and the board loads and updates.
3. `kubectl -n metiche get pod metiche-mysql-0 -w` shows no restarts and `1/1`.
4. A data-manager query through the agent succeeds.
5. A probe pod is refused: `kubectl -n metiche run np-probe --rm -it --restart=Never --image=busybox:1.36 -- nc -zvw3 metiche-mysql 3306` times out. This pod carries neither allowed label, and before the policy the same command connects.

### 7.2 The agent's own ingress

Nothing listens in the pod, and the headless Service has no ports. No ingress
policy is needed.

### 7.3 Egress: a follow-up (D6)

A follow-up NetworkPolicy on `nuzur-agent` pods would have `policyTypes: [Egress]`
and allow only:

- DNS to `kube-system` kube-dns on UDP and TCP 53;
- TCP 3306 to the metiche-mysql pods;
- TCP 443 to `cm.nuzur.com` and `product.nuzur.com`.

**Caveat to test first.** Both hosts resolve to this box's own public address,
69.164.192.246 *(observed)*, and are served by the ingress controller in
namespace `ingress`. Whether Calico matches the 443 rule on
`ipBlock 69.164.192.246/32` or on the ingress controller pods, after DNAT,
has to be established on the box before enforcing.

---

## 8. Idempotent setup

`deploy/nuzur-agent/setup.sh` runs as root on the box, from a checkout, with
`KUBECTL="microk8s kubectl"` and `HELM="microk8s helm3"`. Every step is
**check → skip or act → verify**. The script stops at the first STOP or human
step, and a re-run resumes from the first incomplete step.

Helpers:

```sh
podx() { $KUBECTL -n metiche exec nuzur-agent-0 -c agent -- "$@"; }          # non-interactive
mysql_root() { $KUBECTL -n metiche exec -i metiche-mysql-0 -c mysql -- \
    sh -c 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -u root --batch --skip-column-names'; }
CFG=/var/lib/nuzur/config/nuzur/agent                                          # path inside the pod
```

**Step 0: preconditions (check only).** Running as root on x86_64.
`metiche-mysql-0` is Ready. `$KUBECTL` and `$HELM` work.

**Step 1: the `nuzur_ro` password.**

- **Check:** `/etc/metiche/nuzur_ro.env` (root 0600, `NUZUR_RO_PASSWORD=…`) exists and is non-empty. If so, keep it.
- **Act:** generate it exactly like `gen_password` in `deploy/scripts/gen-credentials.sh`: 32 characters from `[A-Za-z0-9]`, written by redirection under `umask 077`, never echoed.
- **Always:** create or refresh Secret `nuzur-agent-db` from that file with `lib.sh`'s `secret_from_env_file`. The file is the source of truth, so the Secret can never drift into a different password.

**Step 2: MySQL accounts and database.**

- **Check:**
  - `nuzur_ro@'10.1.0.0/255.255.0.0'` and `nuzur_views@localhost` exist;
  - `SHOW GRANTS` for each equals exactly the expected set;
  - `metiche_nuzur` exists.

  All true → skip. Any **extra** grant → **STOP**, never silently revoke.
- **Act:** SQL assembled with the shell's `printf` builtin, so the password is never an external command's argument, and piped into `mysql_root`:

```sql
CREATE DATABASE IF NOT EXISTS metiche_nuzur;
CREATE USER IF NOT EXISTS 'nuzur_views'@'localhost' ACCOUNT LOCK;
GRANT SELECT ON metiche.* TO 'nuzur_views'@'localhost';
CREATE USER IF NOT EXISTS 'nuzur_ro'@'10.1.0.0/255.255.0.0' IDENTIFIED BY '<from file>' PASSWORD EXPIRE NEVER;
ALTER USER 'nuzur_ro'@'10.1.0.0/255.255.0.0' WITH MAX_USER_CONNECTIONS 5;
GRANT SELECT ON metiche_nuzur.* TO 'nuzur_ro'@'10.1.0.0/255.255.0.0';
```

**Step 3: views.** `deploy/nuzur-agent/gen-views.sh --apply` (§5.2).

**Step 4: machine-id.**

- **Check:** `/etc/metiche/nuzur-agent.machine-id` exists. If Secret `nuzur-agent-machine-id` also exists, its value must equal the file, else **STOP**. Skip generation.
- **Act (first run only):** `umask 077; head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > file; printf '\n' >> file`. Create the Secret from the file (`--from-file=machine-id=<file>`). The argv carries the path, not the value.
- **Never** regenerate.

**Step 5: identity ConfigMap.**

- **Check:** ConfigMap `nuzur-agent-ids` exists. `CONNECTION_UUID`, once set, is **never** changed.
- **Act (first run only):** create it with:
  - `CONNECTION_UUID=$(cat /proc/sys/kernel/random/uuid)`
  - `EXPECT_HOSTNAME=nuzur-agent-0`
  - `EXPECT_USER=nuzur`
  - `MACHINE_ID_SHA256=$(sha256sum < file | cut -d' ' -f1)`
  - `AGENT_UUID=` (empty)
- **Mirror** these non-secret values to `/etc/nuzur-agent/ids.env` (root 0644), so teardown still knows the uuids if the cluster objects are gone.

**Step 6: image.**

- **Check:** `microk8s ctr image ls` contains `nuzur-agent:<tag>`. Skip.
- **Act:** `deploy/scripts/build-images.sh nuzur-agent`. It downloads the release, verifies the sha256 **before** `tar`, and imports into containerd.

**Step 7: install in setup mode.**

- **Check:** `helm status nuzur-agent`.
  - Release exists and the pod passes run-mode preflight → skip to step 11.
  - Release exists in setup mode → continue.
- **Act:** `helm upgrade --install nuzur-agent deploy/.helm/nuzur-agent -n metiche --set-string agent.mode=setup --set-string image.tag=<tag> --wait`.
- **Verify:**
  - `podx printenv XDG_CONFIG_HOME` prints `/var/lib/nuzur/config`;
  - `podx hostname` prints `nuzur-agent-0`;
  - `podx sh -c 'df /var/lib/nuzur | tail -1'` shows the PVC mount.

  Any mismatch → **STOP** before pairing (duplicate path 9).

**Step 8: snapshot before pairing (laptop).** Record the owner's agent uuids with
`listLocalAgents`. The existing agents stay untouched (D8).

**Step 9: pair, once, by a human.**

- **Check:**
  - `podx test -s $CFG/local_agent_uuid.txt` → already paired. Its content must equal `AGENT_UUID` if that is recorded, else **STOP**. Skip.
  - `AGENT_UUID` is recorded but the file is missing → **STOP**. This is the PVC-loss case below. Never pair automatically.
- **Act:**
  1. The owner mints a token at `app.nuzur.com/pair` ("Pair a server") **immediately** before. It lives 15 minutes.
  2. `$KUBECTL -n metiche exec -it nuzur-agent-0 -c agent -- nuzur-cli agent pair`, **without** `--provisioning-token`. The container is headless, so the CLI shows the masked "Pairing token" prompt with 3 attempts (§4.A). Paste the token.

  The token never appears in argv, on the box or in the pod.
- **Record:**
  1. On the laptop, `listLocalAgents` now shows **exactly one** uuid not in the step 8 snapshot. Its `machine_name` is `nuzur-agent-0` and its `connections` list is empty.
  2. `podx nuzur-cli agent status` prints the same uuid. It is local only (`cli/app/command_agent_install.go:60-66`).
  3. If the two differ → **STOP**.
  4. Write it as `AGENT_UUID` into ConfigMap `nuzur-agent-ids` and `/etc/nuzur-agent/ids.env`.

**Step 10: connection, once.** No human input beyond running it: the password
comes from the Secret (D7).

- **Check:** `podx nuzur-cli agent connection list` shows exactly one entry, `metiche-prod`, with `CONNECTION_UUID`. The CLI masks the DSN (`command_agent_connection.go:171`, `cli/app/command_agent_start.go:316-354`). Skip.
  - Any other entry, or a different uuid → **STOP**.
- **Act.** The whole script is single-quoted, so every `$…` expands **inside the pod**. On the box, `kubectl`'s argv and the API server's exec request carry only the literal script, never the password. The password exists only in the argv of `nuzur-cli` inside the pod's PID namespace, for the lifetime of that process (D7).

```sh
$KUBECTL -n metiche exec nuzur-agent-0 -c agent -- env CONNECTION_UUID="$CONNECTION_UUID" sh -c '
  PW=$(cat /run/secrets/nuzur-ro/NUZUR_RO_PASSWORD)
  exec nuzur-cli agent connection add \
    --uuid "$CONNECTION_UUID" --driver mysql --schema metiche_nuzur --non-interactive \
    --dsn "nuzur_ro:${PW}@tcp(metiche-mysql.metiche.svc.cluster.local:3306)/metiche_nuzur?parseTime=true&timeout=5s&readTimeout=60s&writeTimeout=60s" \
    metiche-prod'
```

- **Verify:**
  - The output contains `Added connection "metiche-prod" (uuid: <CONNECTION_UUID>, dsn: nuzur_ro:***@…)` and `Published —` (`command_agent_connection.go:132`, `:144`).
  - `listLocalAgents` shows `AGENT_UUID` with `connections = [{uuid: CONNECTION_UUID, name: metiche-prod, db_type: 1, default_schema: metiche_nuzur}]` and no `shared_team_uuids`.
- **If the output says "Saved on this machine but publishing … failed"** (`:139-143`): run the same command again. A scripted add with the same `--uuid` upserts in place (`:70-78`) and republishes. It never mints a new uuid.

**Step 11: switch to run mode.**

- **Check:** the release values show `agent.mode=run` and the pod is `Running` with preflight completed. Skip.
- **Act:** `helm upgrade nuzur-agent … --reuse-values --set-string agent.mode=run --wait`.
  - This drops the password mount.
  - The pod is recreated with the same name and the same PVC.
- **Verify:**
  - `kubectl logs nuzur-agent-0 -c preflight` ends with "preflight ok";
  - `kubectl logs nuzur-agent-0 -c agent` shows `paired and online` (`cli/agent/daemon.go:214`);
  - `listLocalAgents` shows status 1 (ONLINE).

**Step 12: MySQL NetworkPolicy.** §7.1, then its test.

**Step 13: data manager (laptop, once).**

- Attach the "Via agent" connection: agent `AGENT_UUID` → `metiche-prod` → schema `metiche_nuzur`. The catalog default already points there (§4.F).
- Check first that it is not already attached.
- Do not share it (D1).

**Step 14: P1** (§10.3).

### PVC loss (the one manual re-pair)

Symptom: preflight says "agent not paired on this PVC". The PVC was deleted or
the node's disk was lost.

1. Revoke the **old** metiche agent **by the recorded `AGENT_UUID` only** (§11 step 2).
2. Clear `AGENT_UUID` from the ConfigMap and the mirror file. Keep `CONNECTION_UUID`, the machine-id and the password.
3. Switch to setup mode, then run steps 8–11. The new agent gets a new uuid; the connection keeps its recorded uuid.
4. Re-point the data manager's saved connection to the new agent.
5. Run P1.

---

## 9. Credentials

| Secret | Created | Stored | Moves by | Never |
|---|---|---|---|---|
| `nuzur_ro` password | step 1, random | `/etc/metiche/nuzur_ro.env` (root 0600) → Secret `nuzur-agent-db`, plus inside the encrypted DSN on the PVC | `printf` builtin into `kubectl exec` stdin (MySQL); Secret volume in setup mode → in-pod `nuzur-cli` argv (D7) | typed, echoed, in `kubectl`'s argv, in values, in the repo, on the laptop |
| machine-id | step 4, random | `/etc/metiche/nuzur-agent.machine-id` (0600) → Secret `nuzur-agent-machine-id` | Secret volume, read-only | regenerated, retyped, in values, in the repo |
| provisioning token | owner, browser, just before step 9 | nowhere; single use, 15 min | pasted into the masked prompt | argv, env, file |
| agent token | pairing | PVC, 0600 (`cli/app/command_agent.go:270-281`) | read by the daemon and sent in `Hello` | copied, backed up (§13 Q1: no PVC backup), hashed for display |
| DSN | step 10 | PVC `keyring/dsn-<uuid>`, encrypted, 0600 | read by the daemon | the plaintext fallback files or env (§4.E) |

Nothing under `deploy/` contains a secret. `.gitignore` already covers
`credentials.env`, `prod.yaml`, `*.key` and `*.pem`, and the files above live
outside the checkout.

---

## 10. Verification

### 10.1 Data through the data manager

- `SELECT * FROM account LIMIT 100` and `SELECT * FROM agent LIMIT 100` return rows. `token_hash` is NULL throughout; `email`, `client_key` and `repo_url` are populated.
- `SELECT COUNT(*) FROM invite WHERE code IS NOT NULL` returns 0. The same holds for the other four redacted columns.
- Entity search works. It uses explicit field lists, which proves the view column names line up.
- `SHOW DATABASES` lists no `metiche`. `USE metiche` fails with 1044.

### 10.2 Writes refused

- In the data manager's SQL editor, each of these fails with 1142:
  - `INSERT INTO plan (id, \`key\`, name, status) VALUES (UUID(), 'x', 'x', 1)`
  - `UPDATE account SET display_name = display_name`
  - `DELETE FROM team_event WHERE 1=0`
  - `CREATE TABLE t (i INT)`
- Creating a record through the UI fails.
- **Independent of nuzur:** `kubectl -n metiche run ro-probe --rm -it --restart=Never --image=mysql:8.0 --labels app.kubernetes.io/name=nuzur-agent,app.kubernetes.io/instance=nuzur-agent` plus a `[client]` defaults file mounted from a temporary Secret. Run the same statements; each fails, and `SELECT 1` succeeds. Delete the temporary Secret afterwards.
  - The probe borrows the agent's labels so the NetworkPolicy admits it.
  - The password never appears in any argv.

### 10.3 Property P1: identity is stable across restarts, reschedules, deletes and upgrades

**Snapshot S.**

- **From `listLocalAgents` on the laptop:**
  - the set and count of the owner's non-revoked agent uuids;
  - for `AGENT_UUID`: `created_at`, `machine_name` (`nuzur-agent-0`), and `connections` as a list of `{uuid, name, db_type, default_schema, shared_team_uuids}`, with exactly one entry whose uuid is `CONNECTION_UUID`.
- **From the pod:**
  - `podx sha256sum $CFG/local_agent_uuid.txt $CFG/local_agent_connections.json`;
  - `podx stat -c '%i %s %Y' $CFG/local_agent_token.txt`. The token is never hashed, because its hash is what the server treats as secret (`go/product/server/local_agent.go:240-243`);
  - `podx ls $CFG/keyring` and the mtime of `dsn-$CONNECTION_UUID`;
  - `$KUBECTL -n metiche get pvc data-nuzur-agent-0 -o jsonpath='{.metadata.uid}'`.

**Excluded, because they change legitimately:** `status`, `last_seen_at`,
`updated_at` (`go/connection-manager/server/local_agent_channel.go:186-196`),
the pod uid and the pod IP.

**Procedure.** Take S0 with the agent ONLINE. After each action below, wait until
`status` is 1 with a newer `last_seen_at`, take a snapshot, and assert it equals S0.

1. **Restart ×2:** `kubectl -n metiche rollout restart statefulset/nuzur-agent`, twice, each followed by `rollout status`.
2. **Delete:** `kubectl -n metiche delete pod nuzur-agent-0`, graceful, never `--force`. The StatefulSet recreates it.
3. **Container crash:** `podx kill 1`. The container restarts in the same pod, and its RESTARTS count increases.
4. **Image upgrade:** rebuild with a new tag, then `helm upgrade --set-string image.tag=<new>`.
5. **Optional node reboot:** after the box comes back.
6. **Negative control.**
   1. Temporarily set `EXPECT_USER=wrong` in ConfigMap `nuzur-agent-ids`, then delete the pod.
   2. Expect `Init:CrashLoopBackOff`, with the preflight log naming the USER mismatch.
   3. Expect `listLocalAgents` to equal S0, apart from status going OFFLINE.
   4. Restore the ConfigMap, delete the pod, snapshot, and assert equal.

**Pass:** every snapshot equals S0. Any difference fails P1: stop and investigate.
`deploy/nuzur-agent/verify-p1.sh` automates the box half and prints only uuids,
counts, non-secret file hashes, inode numbers and mtimes.

---

## 11. Revocation, in order, by recorded uuid only

1. **Cut data access.** `DROP USER 'nuzur_ro'@'10.1.0.0/255.255.0.0';`, then `KILL` any sessions with `user='nuzur_ro'` in `information_schema.processlist`. DROP USER does not end open sessions.
2. **Revoke the metiche agent, and only that one.**
   1. Read `AGENT_UUID` from `/etc/nuzur-agent/ids.env`.
   2. In `listLocalAgents`, confirm that uuid's `connections` is exactly `[metiche-prod, CONNECTION_UUID]`. If not → **STOP**.
   3. `nuzur-cli agent revoke "$AGENT_UUID"` from the signed-in laptop (`cli/app/command_agent.go:235-267`). The server marks it REVOKED and clears its shares (`go/product/server/local_agent.go:186-225`). Reconnects are refused (`go/connection-manager/server/local_agent_channel.go:163-165`).
   4. **Never** select an agent by name in the web UI or anywhere else. The agent serving another project must remain untouched.
   5. Remove the data manager's saved connection.
3. **Stop the agent.** `helm uninstall nuzur-agent -n metiche`. The PVC is retained (§6.2).
4. **Remove agent state.**
   - `kubectl -n metiche delete pvc data-nuzur-agent-0`. The reclaim policy is Delete, so this removes the uuid, the token, the registry and the keyring. Confirm the hostPath directory under `/var/snap/microk8s/common/default-storage/` is gone.
   - `kubectl -n metiche delete secret nuzur-agent-db nuzur-agent-machine-id`
   - `kubectl -n metiche delete configmap nuzur-agent-ids nuzur-agent-views`
5. **Tidy.**
   - `DROP DATABASE metiche_nuzur; DROP USER 'nuzur_views'@'localhost';`
   - `rm /etc/metiche/nuzur_ro.env /etc/metiche/nuzur-agent.machine-id`
   - `rm -r /etc/nuzur-agent`
   - Remove the agent rule from the MySQL NetworkPolicy, and keep the backend rule.
   - Remove `nuzur-agent:*` from containerd.
6. **Confirm.** `listLocalAgents` shows `AGENT_UUID` REVOKED (status 3) and **every other agent unchanged**.

---

## 12. Where the non-secret pieces live in the repo

This is a description only; none of it is written yet.

| path | purpose |
|---|---|
| `deploy/docker/nuzur-agent.Dockerfile` | §6.1. Pinned version plus pinned sha256 ARGs. |
| `deploy/nuzur-agent/nuzur-agent-preflight` | Run-mode init checks (§6.2). POSIX sh, copied into the image. |
| `deploy/nuzur-agent/nuzur-agent-hold` | Setup-mode placeholder process (§6.2). |
| `deploy/.helm/nuzur-agent/` | `Chart.yaml`, `values.yaml`, `templates/{_helpers.tpl, serviceaccount.yaml, service.yaml, statefulset.yaml, NOTES.txt}` |
| `deploy/.helm/metiche-mysql/templates/networkpolicy.yaml` | §7.1, behind `networkPolicy.enabled`. |
| `deploy/nuzur-agent/setup.sh` | §8. |
| `deploy/nuzur-agent/columns.policy` | §5.2. |
| `deploy/nuzur-agent/gen-views.sh` and `check-policy-ci.sh` | §5.2. |
| `deploy/nuzur-agent/verify-p1.sh` | §10.3. |

Changes to existing files:

- `deploy/scripts/build-images.sh` gains a `nuzur-agent` target.
- `deploy/scripts/helm-deploy.sh` gains `nuzur-agent`. It never passes a secret, and it `require_secret`s the two Secrets.
- `deploy/README.md` gains the line "after any `deploy/sql/*.sql`, run `deploy/nuzur-agent/gen-views.sh --apply`".

---

## 13. Decisions (owner-approved 2026-09-13)

These add to the decisions already recorded in §2, which stand unchanged.

1. **Q1: back up the PVC?** No backup. Losing it costs one provisioning token, one re-pair and re-pointing the data manager (§8, "PVC loss"), while a backup would be a second copy of a live credential (the agent token and the encrypted DSN) to protect.
2. **Q2: where does the daily views drift check run?** A host systemd timer on the box runs `gen-views.sh --check` via `kubectl exec` (like `apply-schema.sh`), plus the CI check; no in-cluster CronJob. No MySQL root credential leaves the MySQL pod, and no extra NetworkPolicy exception is needed.
3. **Q3: CLI upgrade policy.** `nuzur-cli` stays pinned at 1.9.2 and is upgraded only when the server forces it (it raises `min_cli_version` and the pod reports CLI-too-old, `cli/agent/daemon.go:141-143`, `:220-225`); each upgrade is a rebuild with a new pinned sha256, a `helm upgrade`, then P1 re-run (§10.3). Every upgrade has to re-prove P1, so none happens without a reason.
