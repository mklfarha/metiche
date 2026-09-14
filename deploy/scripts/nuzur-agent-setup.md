# Runbook: the nuzur agent for metiche's database

This is the ordered procedure. Design and reasons: [docs/NUZUR_AGENT.md](../../docs/NUZUR_AGENT.md).
Every box step is a subcommand of [`nuzur-agent-setup.sh`](nuzur-agent-setup.sh). Each one
checks, then skips or acts, then verifies, so a re-run resumes where it stopped.

**Who does what**

| who | where | does |
|---|---|---|
| **OWNER** | browser and the box | mints the pairing token and types `agent pair`; attaches the connection in the data manager; approves production runs |
| coordinator | the box, as root | every `nuzur-agent-setup.sh` step |
| coordinator | laptop, the nuzur MCP | `listLocalAgents` snapshots and diffs; never anything that writes |

**Never, at any step**

- Pair from a script, pass `--provisioning-token`, or use `agent pair --force`.
- Run `nuzur-cli login`, `connect` or `agent install` in the pod.
- Set `NUZUR_AGENT_DSN` or `NUZUR_AGENT_DRIVER`.
- Select, revoke or edit any agent by name. The metiche agent is only ever its recorded
  `AGENT_UUID`. The owner's other agent, which serves a different project, is never touched.
- Print, type or paste a password, a token, a DSN or the machine-id.
- Force-delete the pod.

---

## 0. Session setup (box)

```sh
sudo -i
cd <the metiche checkout on the box>     # git pull first: it must contain this file
export KUBECTL="microk8s kubectl" HELM="microk8s helm3"
nas() { deploy/scripts/nuzur-agent-setup.sh "$@"; }
```

## 1. Build everything up to the pairing (coordinator)

```sh
nas preconditions        # read-only: mysql Ready, column policy total, chart renders
nas secrets              # nuzur_ro password, machine-id, ConfigMap nuzur-agent-ids (+ /etc/nuzur-agent/ids.env)
nas db                   # metiche_nuzur, nuzur_views@localhost (locked), nuzur_ro@10.1.0.0/255.255.0.0
nas views                # 27 redacted views; ends "ok: views applied and verified"
TAG=$(nas image)         # builds nuzur-agent:1.9.2-<sha>, checks both sha256 pins, imports into containerd
nas install-setup "$TAG" # setup mode; verifies hostname, USER, uid, config dir on the PVC, machine-id
nas pair-check           # prints "SAFE TO PAIR" and the exact next five steps, or STOPs
```

`secrets` never regenerates anything that exists. It STOPs, rather than guessing, if the
machine-id file and its Secret disagree.

`views` fails, naming each offender, if the live schema has a column that
[`deploy/sql/nuzur/columns.policy`](../sql/nuzur/columns.policy) does not classify.

## 2. Pair, once

1. **Coordinator, laptop:** `listLocalAgents`. Save the set of agent uuids. This is snapshot **A**.
2. **OWNER, browser:** app.nuzur.com/pair, then "Pair a server". Copy the token. It lives 15
   minutes and works once, so do this immediately before step 3.
3. **OWNER, on the box:**

   ```sh
   sudo microk8s kubectl -n metiche exec -it nuzur-agent-0 -c agent -- nuzur-cli agent pair
   ```

   The container has no display, so the CLI shows a **masked** "Pairing token" prompt with 3
   attempts. Paste the token there, never on the command line. It prints
   `Paired local agent (headless).` and a uuid.
4. **Coordinator, laptop:** `listLocalAgents` again. This is snapshot **B**. B minus A must be
   **exactly one** uuid, with `machine_name` `nuzur-agent-0` and an empty `connections` list.
   Anything else is a STOP. Every uuid in A must be unchanged in B.
5. **Coordinator, box:**

   ```sh
   nas record-agent <the one new uuid>   # cross-checks the pod's uuid file and `agent status`
   ```

## 3. Connection and run mode (coordinator)

```sh
nas connection   # publishes metiche-prod with the recorded CONNECTION_UUID, signed by the agent
```

- **Expect** `Added connection "metiche-prod" (uuid: <CONNECTION_UUID>, dsn: nuzur_ro:***@…)`,
  then `Published —`.
- **If it says "publishing … failed"**, run `nas connection --again`. That repeats the same add
  with the same uuid, which upserts in place. It never mints a new uuid.
- **Laptop check:** `listLocalAgents` shows `AGENT_UUID` with exactly
  `[{uuid: CONNECTION_UUID, name: metiche-prod, db_type: 1, default_schema: metiche_nuzur}]` and
  no `shared_team_uuids`. Every other agent is unchanged.

```sh
nas run          # dry-runs the preflight in the setup pod, then switches; waits for "paired and online"
```

- **Laptop check:** `listLocalAgents` shows `AGENT_UUID` with status 1 (ONLINE).

## 4. Close MySQL to everything else (coordinator, then a manual check)

```sh
nas netpol       # helm-deploy.sh mysql with networkPolicy.enabled=true
```

This step also:

- proves a label-less probe pod is refused;
- proves a pod with the agent's labels connects;
- checks the MySQL pod was not restarted.

It writes `/etc/metiche/metiche-mysql.networkpolicy.yaml`. **From now on every
`helm-deploy.sh mysql` must run with `METICHE_VALUES_MYSQL` set to that file**, or the policy is
removed again (see "Follow-ups" in docs/NUZUR_AGENT.md).

**Then by hand:**

- `microk8s kubectl -n metiche logs deploy/metiche --since=5m | grep -i 'mysql\|database'` shows no errors.
- The board loads and updates.

**Revert:** set `enabled: false` in that file and re-run
`METICHE_VALUES_MYSQL=/etc/metiche/metiche-mysql.networkpolicy.yaml deploy/scripts/helm-deploy.sh mysql`.

## 5. Data manager (OWNER, once)

1. Check the connection is not already attached.
2. Attach "Via agent": `AGENT_UUID`, then `metiche-prod`, then schema `metiche_nuzur`.
3. Do not share it with any team.

Run these in its SQL editor:

- `SELECT * FROM account LIMIT 100`, `SELECT * FROM agent LIMIT 100`: `token_hash` is NULL, `email` and `client_key` are populated.
- `SELECT COUNT(*) FROM invite WHERE code IS NOT NULL` returns 0. The same holds for `notification_channel.target_url`,
  `team_event.response_snapshot`, `board_login_link.secret_hash`, `browser_session.secret_hash`, `browser_session.user_agent`, `browser_session.ip_hint`.
- `SHOW DATABASES` has no `metiche`. `USE metiche` fails with 1044.
- `INSERT INTO plan (id, \`key\`, name, status) VALUES (UUID(), 'x', 'x', 1)`, `UPDATE account SET display_name = display_name`,
  `DELETE FROM team_event WHERE 1=0` and `CREATE TABLE t (i INT)` each fail with 1142.

## 6. P1: identity survives restarts (coordinator)

```sh
nas p1
```

This takes snapshot S0, then does two `rollout restart`s, one graceful `delete pod` and a
`kill 1` crash. After each it waits for the pod to come back online and asserts the snapshot
equals S0. The snapshot holds the uuids, file hashes, token inode/mtime, keyring item, PVC uid
and machine-id hash, and never the token. After **each** action, the coordinator runs
`listLocalAgents` and checks it matches S0: same uuids and count, and for `AGENT_UUID` the
same `created_at`, `machine_name` and `connections`. Only `status`, `last_seen_at` and
`updated_at` may change.

Two more checks, by hand:

```sh
# Image upgrade: a new commit gives a new tag.
TAG2=$(nas image)
microk8s helm3 upgrade nuzur-agent deploy/.helm/nuzur-agent -n metiche \
  --set-string agent.mode=run --set-string image.tag="$TAG2" --wait
nas snapshot            # compare with the S0 file under /etc/nuzur-agent/p1-*/S0

# Negative control: a fingerprint mismatch must stop the daemon, not re-pair it.
microk8s kubectl -n metiche patch configmap nuzur-agent-ids --type merge -p '{"data":{"EXPECT_USER":"wrong"}}'
microk8s kubectl -n metiche delete pod nuzur-agent-0
microk8s kubectl -n metiche get pod nuzur-agent-0     # Init:CrashLoopBackOff
microk8s kubectl -n metiche logs nuzur-agent-0 -c preflight   # "USER is 'nuzur', but the recorded EXPECT_USER is 'wrong'"
#   laptop: listLocalAgents equals S0 apart from status going OFFLINE
microk8s kubectl -n metiche patch configmap nuzur-agent-ids --type merge -p '{"data":{"EXPECT_USER":"nuzur"}}'
microk8s kubectl -n metiche delete pod nuzur-agent-0
nas snapshot            # equals S0 again
```

## 7. Daily drift check (coordinator, with owner approval)

```sh
nas drift-timer   # systemd timer: deploy/sql/nuzur/gen-views.sh check-live, daily
```

After **any** `deploy/sql/*.sql` migration:

1. Classify the new columns in `columns.policy`, in the same commit.
2. Once the migration is applied, run `deploy/sql/nuzur/gen-views.sh apply` on the box.

---

## PVC loss (the one manual re-pair)

**Symptom:** the preflight says "agent not paired on this PVC".

1. Teardown step 2 below. The **OWNER** revokes the old `AGENT_UUID`, by uuid.
2. Clear `AGENT_UUID`. Keep everything else, including `CONNECTION_UUID`:

   ```sh
   microk8s kubectl -n metiche patch configmap nuzur-agent-ids --type merge -p '{"data":{"AGENT_UUID":""}}'
   sed -i 's/^AGENT_UUID=.*/AGENT_UUID=/' /etc/nuzur-agent/ids.env
   ```

3. `NUZUR_AGENT_RESEAL=1 nas install-setup "$(microk8s kubectl -n metiche get sts nuzur-agent -o jsonpath='{.spec.template.spec.containers[0].image}' | cut -d: -f2)"`
4. Section 2, then `nas connection`, then `nas run`. The new agent has a new uuid; the connection keeps its recorded one.
5. **OWNER:** re-point the data manager's saved connection to the new agent.
6. `nas p1`.

**If the passphrase changed** (hostname, machine-id or USER), follow the "re-seal" steps in
docs/NUZUR_AGENT.md §4.D. They keep both uuids.

## Teardown, by recorded uuid only

```sh
nas teardown-cut-access          # 1. DROP USER nuzur_ro, KILL its sessions; prints the recorded AGENT_UUID
```

2. **OWNER, laptop:**
   1. Read `AGENT_UUID` from `/etc/nuzur-agent/ids.env`.
   2. In `listLocalAgents`, confirm that uuid's `connections` is exactly
      `[metiche-prod, CONNECTION_UUID]`. If not, STOP.
   3. `nuzur-cli agent revoke <AGENT_UUID>`.
   4. Remove the data manager's saved connection.
   5. Never pick an agent by name.

```sh
nas teardown-remove <AGENT_UUID> # 3-5. Refuses any uuid but the recorded one; asks for confirmation of step 2
```

This removes:

- the release, and PVC `data-nuzur-agent-0`, which deletes the pairing;
- both Secrets and ConfigMap `nuzur-agent-ids`;
- `metiche_nuzur` and `nuzur_views`;
- the drift timer, the credential files and the images.

Then:

- Drop the `nuzur-agent` entry from `networkPolicy.allowFrom` in
  `/etc/metiche/metiche-mysql.networkpolicy.yaml` and re-run `helm-deploy.sh mysql` with it.
- **Laptop:** `listLocalAgents` shows `AGENT_UUID` REVOKED (status 3) and every other agent
  unchanged. Only after that, `rm -r /etc/nuzur-agent`.

---

## Proven locally, and how to re-run it

None of these contact the box or nuzur.

```sh
deploy/sql/nuzur/gen-views.sh check                      # column policy total (CI; no database)
deploy/sql/nuzur/test-views-docker.sh                    # throwaway MySQL 8.4: views, canaries, denials, host range, mutation, drift
NUZUR_TEST_MYSQL_IMAGE=mysql:8.0 deploy/sql/nuzur/test-views-docker.sh   # the production MySQL line
docker buildx build --platform linux/arm64 --load -f deploy/docker/nuzur-agent.Dockerfile -t nuzur-agent:1.9.2-local-arm64 deploy/docker
deploy/scripts/nuzur-agent-test-pod.sh                   # pod-shaped, --network none: preflight positive and negative cases
helm template nuzur-agent deploy/.helm/nuzur-agent -n metiche --set-string agent.mode=run --set-string image.tag=x
```

What only production can prove: the real pairing, publishing, "paired and online", the
Calico enforcement of the NetworkPolicy, and P1 against nuzur's `listLocalAgents`.
