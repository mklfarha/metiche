# deploy/ — metiche on an existing microk8s box

**This is one implementation of metiche's deployment contract, not the contract.**

The contract is in the project [README](../README.md#deploy-it) and it is short:
one Go binary, one MySQL 8 database it owns, two HTTP surfaces, TLS in front
that does not buffer and does not cut idle connections, configuration supplied
from outside the image, and no egress. A laptop, a single VPS under systemd,
and a two-service `docker-compose.yml` all satisfy it. What follows is what
that looks like on **a Linode box that already runs microk8s for other
projects** — which is where metiche.xyz happens to live, and nothing more
general than that.

If you are here to find out what metiche needs, read the README. If you are
here to deploy *this* box, read on.

---

## What gets created, and where

Everything lives in one namespace, `metiche`.

| | |
|---|---|
| `metiche` | the Go backend. `METICHE_ROLE=all`: MCP endpoint, web API and SSE stream in one process. |
| `metiche-web` | the board. A Go binary serving assets compiled into it. |
| `metiche-mysql` | MySQL 8, StatefulSet, one PVC. |

Hostnames: `metiche.xyz` → the board, `api.metiche.xyz` and `mcp.metiche.xyz`
→ the backend.

### What it touches outside its own namespace

The `metiche` namespace object, created by name if it is absent. **That is the
complete list.** No chart here renders a ClusterRole, ClusterRoleBinding,
IngressClass, StorageClass, PersistentVolume, CRD, admission webhook or
PriorityClass, and nothing writes into another namespace. Verify it yourself:

```sh
for c in metiche metiche-web metiche-mysql; do
  helm template "$c" "deploy/.helm/$c" -n metiche | grep '^kind:'
done | sort -u
```

Everything in that list is namespaced.

---

## Assumptions about a box you did not build

These five are checked and printed, not assumed silently, by
`deploy/deploy.sh --preflight`. Check them before the first run.

1. **microk8s is installed and running**, with ingress-nginx and cert-manager
   already serving other projects. This deployment adds to that; it bootstraps
   nothing.
2. **The IngressClass name.** The charts default to `public`, which is what
   microk8s' `ingress` addon registers. An upstream ingress-nginx chart
   registers `nginx` instead. `kubectl get ingressclass` settles it; override
   with `METICHE_INGRESS_CLASS=nginx`.
3. **A ClusterIssuer exists**, named `letsencrypt-prod` by default. The charts
   **reference** it and never create or edit one, because a ClusterIssuer is
   cluster-scoped and shared with everything else on the box. Override with
   `METICHE_ISSUER=<name>`, or set it to `""` and supply your own TLS Secrets
   in the namespace.
4. **A StorageClass exists**, `microk8s-hostpath` by default. Override with
   `METICHE_STORAGE_CLASS=<name>`, or `""` to use the cluster default.
5. **DNS already points `metiche.xyz`, `api.metiche.xyz` and `mcp.metiche.xyz`
   at this box.** cert-manager's HTTP-01 challenge cannot succeed before that
   is true, and the symptom is a Certificate stuck at `READY=False` with a
   pending Order.

One more, and it is the one that bites: **the box is amd64 and your laptop is
probably arm64.** Images are built *on the box*. `build-images.sh` refuses to
run anywhere whose architecture is not the target rather than cross-building.

---

## First deployment

Everything runs **on the box**, from a checkout on the box.

```sh
# 1. get the source there
git clone https://github.com/mklfarha/metiche.git
cd metiche

# 2. look before you leap — read-only, changes nothing
deploy/deploy.sh --preflight

# 3. the whole thing
sudo deploy/deploy.sh
```

`deploy.sh` runs preflight, asks once, then: generate credentials → build and
import images → install `metiche-mysql` → apply the schema → install `metiche`
→ install `metiche-web`. The order matters exactly once: the backend opens a
connection pool as its first act, so MySQL has to be up and its tables have to
exist before the backend pod starts.

Non-interactive: `METICHE_YES=1 sudo deploy/deploy.sh`.

### The steps on their own

```sh
sudo deploy/scripts/gen-credentials.sh     # passwords + config overlay + Secrets
     deploy/scripts/build-images.sh        # prints the tag on stdout
     deploy/scripts/apply-schema.sh        # create.sql into the running pod
     deploy/scripts/helm-deploy.sh all     # or: mysql | backend | web
```

Redeploy without rebuilding:

```sh
METICHE_TAG=<existing tag> sudo deploy/deploy.sh --no-build
```

---

## Credentials

**This repository is public. Nothing in `deploy/` is or may become a secret.**

* Passwords are **generated on the box**, by `gen-credentials.sh`, into
  `/etc/metiche/credentials.env` — root-owned, `chmod 600`, outside the
  checkout, and already covered by the repository `.gitignore` besides.
* They are **never echoed**. No script prints one, not even with `--dry-run`.
  To see one: `sudo cat` the file, on the box, deliberately.
* They are **never a command-line argument**. Every process on a shared box
  can read every other process's `argv` out of `/proc`, and argv also lands in
  shell history. So there is no `kubectl create secret --from-literal`, no
  `mysql -p<password>`, and no `sed s/x/$PASSWORD/` anywhere here. Values move
  through files and stdin only:
  * `kubectl create secret --from-env-file=` / `--from-file=`, piped straight
    into `kubectl apply` so the rendered manifest never touches disk;
  * the template substitution in `gen-credentials.sh` is a shell `while read`
    loop, not `sed`/`awk`/`envsubst`, for exactly this reason;
  * `apply-schema.sh` runs `mysql` **inside** the MySQL pod, where the password
    is already in the pod's environment, via `MYSQL_PWD` rather than `-p`;
  * the MySQL probes read the password from a mounted Secret file inside the
    probe's own shell.
* `prod.yaml.example` is the committed template. Its one secret field is
  `__DB_PASSWORD__`, and `gen-credentials.sh` fails loudly if substitution
  leaves it behind.
* Two Secrets result, both namespaced to `metiche`:
  `metiche-db` (`METICHE_DB_ROOT_PASSWORD`, `METICHE_DB_PASSWORD`) and
  `metiche-config` (key `prod.yaml`).

**The join code and the agent tokens are not deploy's business at all.** They
are minted by the server and live in the database. Nothing here reads them, and
nothing here should ever `SELECT` a row — the same tables hold
`notification_channel.target_url`, a team's Slack or Discord webhook, which
[`docs/MODEL.md`](../docs/MODEL.md) calls the first real secret in the system:
write-only from the outside, never returned by an API, never rendered, never
logged. `apply-schema.sh` runs exactly one query and it is a `COUNT(*)`.

### Rotating

The **root** password cannot be rotated by redeploying: the mysql image honours
`MYSQL_ROOT_PASSWORD` only on an empty datadir, and afterwards only an
`ALTER USER` you run yourself changes it. Regenerating the credentials file
would mint a password the existing volume has never heard of, and the pod would
come up rejecting every login — so `gen-credentials.sh` keeps an existing file
and tells you this instead.

The **application** password:

```sh
sudo deploy/scripts/gen-credentials.sh --rotate-app
# then ALTER USER inside the pod, then:
kubectl -n metiche rollout restart deploy -l app.kubernetes.io/part-of=metiche
```

---

## Configuration

The backend image ships `config/base.yaml` — generated, committed, and full of
`<< Database Password Here>>` placeholders. The real values arrive at runtime
as a **mounted Secret**, and the loader merges the two:

```
CONFIG=/root/config,/etc/metiche/config/prod.yaml
```

Later file wins. That is the pattern the generated `base.yaml` implies, and it
is the reason the repository can be public.

**A Secret rather than a hostPath**, deliberately, and against the sketch in
`docs/PLAN.md` which said "hostPath config overlay":

* a hostPath silently pins the pod to whichever node holds the file, and its
  failure mode when the file is missing is an empty config rather than an
  error;
* a Secret is namespaced, so it cannot be read from another project's
  namespace on this shared box;
* it is the same mechanism on a single-node microk8s and on a real cluster, so
  the runbook does not change if metiche ever moves off this box.

The root-owned file on the box still exists — it is the *source* the Secret is
built from, not what the pod reads.

**On the path.** These scripts keep everything under `/etc/metiche/`
(`credentials.env` and `prod.yaml`, both root-owned `0600`) so all of metiche's
on-box state is in one directory that belongs to metiche and to nothing else.
The repository `.gitignore` names `/etc/config/nuzur/metiche/prod.yaml` in a
comment; that was the path in the original sketch, and the ignore rules
themselves are filename-based (`prod.yaml`, `credentials.env`), so both paths
are equally covered — and both are outside the checkout regardless. If you
prefer the other path, everything here honours `METICHE_CRED_DIR`,
`METICHE_CRED_FILE` and `METICHE_CONFIG_FILE`.

Changing config does not restart pods on its own (the Deployment carries the
Secret's *name*, never its contents, so there is no checksum to change):

```sh
sudo deploy/scripts/gen-credentials.sh --secret-only
kubectl -n metiche rollout restart deploy -l app.kubernetes.io/part-of=metiche
```

### The board → backend hop is configuration

`metiche-web` never has a backend hostname compiled in or written into a
template. `backend.url` overrides it outright; otherwise it is *derived* from
`backend.serviceName` + the release namespace, so it resolves to the backend
Service in-cluster and the hop never leaves the node. Set
`backend.serviceName=""` and the board replays the recordings embedded in its
image instead — which is the fastest way to prove ingress, TLS and SSE work
before the backend exists.

---

## SSE — read this before you debug the frontend

Three Ingresses carry Server-Sent Events: the board's `/t/{slug}/stream`, the
backend's `/v1/teams/{slug}/stream`, and the MCP transport. All three get:

```yaml
nginx.ingress.kubernetes.io/proxy-buffering: "off"
nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

nginx's defaults are wrong here in two independent ways and **both fail in a
way that looks like a frontend bug**:

* `proxy_buffering on` (the default) holds the response until a buffer fills.
  The board renders nothing at all while the browser shows a healthy open
  connection.
* `proxy_read_timeout 60s` cuts an idle stream — and an idle stream is normal,
  because a team where nobody has done anything for a minute is most teams most
  of the time. htmx's SSE extension reconnects cleanly enough that the only
  symptom is a board that flickers every 60 seconds and occasionally drops a
  frame.

The project README calls this "the single most likely way to make a correct
deployment look broken". The check:

```sh
curl -N https://metiche.xyz/t/<slug>/stream     # must keep printing past 60s
kubectl -n metiche get ingress -o yaml | grep -A1 proxy-
```

Off ingress-nginx the equivalent knobs are Caddy's `flush_interval -1`,
Traefik's `responseForwarding.flushInterval`, and Envoy's per-route
`idle_timeout`.

---

## Health probes

**Liveness never touches the database.** A momentary database blip must not get
otherwise-healthy pods killed: that turns a ten-second outage into a
`CrashLoopBackOff` that outlives it, and tears down every browser's SSE
connection for nothing.

| | liveness | readiness |
|---|---|---|
| `metiche` | `GET /healthz` — the generated static handler, writes `{"status":"OK"}` and touches nothing | `GET /healthz` today; **point this at a database-aware endpoint when the backend grows one**, via `readinessProbe.path`. Do not point liveness at it. |
| `metiche-web` | `GET /healthz` — in-memory cursors, no outbound call | same. A backend outage deliberately does not make the board unready: a board showing its last folded state while it reconnects beats no board. |
| `metiche-mysql` | `mysqladmin ping` | `mysqladmin ping` — this one *is* the database |

---

## The role split

v1 runs **one release, `METICHE_ROLE=all`**, and that is not a compromise: with
one process the in-process SSE fan-out covers 100% of writes and the 250ms
database tailer never has to be the delivery path
([`docs/PLAN.md`](../docs/PLAN.md), "Realtime").

The split ships ready anyway, so isolating agent load from the board later is a
second release rather than a rewrite:

```sh
helm upgrade --install metiche-api deploy/.helm/metiche -n metiche \
  -f deploy/.helm/metiche/values-api.yaml --set image.tag=$TAG
helm upgrade --install metiche-mcp deploy/.helm/metiche -n metiche \
  -f deploy/.helm/metiche/values-mcp.yaml --set image.tag=$TAG
```

Each host entry in `values.yaml` carries a `roles:` list, so the `mcp` release
cannot take `api.metiche.xyz` away from the `api` release even though it
inherits both entries. Uninstall the `all` release first — three releases would
all claim the same hostnames — and point the board at the new backend Service
with `--set backend.serviceName=metiche-api`.

---

## Images

No registry. `build-images.sh` builds with the local Docker daemon and imports
straight into microk8s' containerd:

```
docker build → docker save → microk8s ctr image import
```

which is why every chart sets `imagePullPolicy: Never`. A tag that is not in
containerd gives `ErrImageNeverPull` rather than a doomed pull. To use a
registry instead, set `image.repository` to the registry path and
`image.pullPolicy` to `IfNotPresent`.

The backend uses its own generated `code/backend/metiche/Dockerfile`. The board
has no Dockerfile of its own, so one lives here at
`deploy/docker/metiche-web.Dockerfile` with `code/frontend` as its build
context — packaging is deploy's business, and the frontend is a plain
`go build` that knows nothing about containers.

`mysql:8.0` is pinned and pre-pulled into containerd by the same script, so a
pod rescheduled at 3am does not depend on Docker Hub being reachable.

Neither build context has a `.dockerignore`, and neither needs one today: both
are source trees with no build output and no credentials in them. If you add
one, the thing it is for is keeping a stray local `prod.yaml` or `.env` out of
an image layer.

---

## Schema

```sh
deploy/scripts/apply-schema.sh [path/to/create.sql]
```

Defaults to `code/backend/metiche/core/repository/sql/schema/create.sql`, which
codegen generates and the repo commits. Every statement is
`CREATE TABLE IF NOT EXISTS`, so it is safe to re-run and it picks up new
tables after a codegen.

**It does not migrate.** `IF NOT EXISTS` adds missing tables and silently
leaves alone a table whose definition has *changed* — a new column on an
existing entity will not appear. Once there is data worth keeping, schema
changes are an `ALTER` you write and apply deliberately.

It is a script rather than a `/docker-entrypoint-initdb.d` ConfigMap because
the mysql image runs that directory exactly once, on an empty datadir; that
would make the schema un-re-appliable and turn every codegen into "delete the
volume".

---

## Data

The MySQL PVC is a `volumeClaimTemplate`, which Helm **does not delete on
`helm uninstall`**. That is deliberate: the one irreplaceable thing here is the
data, and an uninstall typed at 2am should not be the last event in its life.

```sh
kubectl -n metiche delete pvc data-metiche-mysql-0     # the explicit version
```

Backups are not automated by anything in this directory. `mysqldump` from
inside the pod, with the password out of the pod's own environment:

```sh
kubectl -n metiche exec metiche-mysql-0 -- sh -c \
  'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -u root --single-transaction metiche' \
  > metiche-$(date -u +%F).sql
```

That dump contains join codes, agent token hashes and any
`notification_channel.target_url`. Treat it exactly like the credentials file:
root-owned, off the laptop, never in the repo.

---

## Uninstall

```sh
helm -n metiche uninstall metiche-web metiche metiche-mysql
kubectl -n metiche delete secret metiche-config metiche-db   # optional
kubectl -n metiche delete pvc data-metiche-mysql-0           # deletes the data
kubectl delete namespace metiche                             # optional
```

Nothing outside the namespace is left behind, because nothing outside the
namespace was ever created.

---

## Layout

```
deploy/
  deploy.sh                    orchestrator; --preflight, --no-build, --charts-only
  prod.yaml.example            the config template. Placeholders only.
  docker/
    metiche-web.Dockerfile     the board's image (context: code/frontend)
  scripts/
    lib.sh                     shared helpers; sourced, not run
    preflight.sh               read-only: prints every assumption
    gen-credentials.sh         passwords on the box → root-owned file → Secrets
    build-images.sh            native build + import into containerd
    apply-schema.sh            create.sql into the running MySQL pod
    helm-deploy.sh             helm upgrade --install, one chart or all three
  .helm/
    metiche/                   the backend. values-api.yaml / values-mcp.yaml
    metiche-web/               the board
    metiche-mysql/             MySQL 8 on a PVC
```
