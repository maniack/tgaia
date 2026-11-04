# Deploying tgaia with Docker and Kubernetes

Containers run the same binary as local installations. Kubernetes uses separate API
and worker Deployments sharing PostgreSQL. API replicas receive Telegram updates
and serve the Web UI; workers process the queue, MCP calls, tasks and compaction.
SQLite is for local use, not for sharing between pods.

## 1. Build and publish the image

From the repository root, with Docker/BuildKit and registry access:

```sh
# Replace the registry and tag. Use a new tag/digest for each rollout.
export TGAIA_IMAGE=registry.example.invalid/team/tgaia:release-1
docker build --pull -t "$TGAIA_IMAGE" .
docker run --rm "$TGAIA_IMAGE" --help
docker push "$TGAIA_IMAGE"
```

For mixed amd64/arm64 clusters:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t "$TGAIA_IMAGE" --push .
```

The two-stage Dockerfile compiles a static, CGO-free binary with Go 1.27.3 and runs
it in Alpine with CA certificates. Neither config.yaml nor system-prompt.md is
embedded; mount both externally. The container runs as UID/GID 10001 and receives
SIGTERM directly, without a shell wrapper. Build arguments GO_VERSION and
ALPINE_VERSION select base images; Go must satisfy go.mod. For reproducible
production builds, pin approved base-image digests and publish immutable application
images. Configuration, tokens and local databases are excluded from the image.

HTTP/SSE MCP needs no additional packages. For stdio MCP, install its executables
and interpreters in a derived image, configure absolute paths and explicitly allow
environment/arguments. All workers need the same binaries. With a read-only root
filesystem, writes require mounted volumes (/tmp in this example). The image does
not bundle Node, Python or arbitrary MCP servers.

For standalone Docker, first prepare PostgreSQL and the production configuration
from section 2. Database/OIDC/LiteLLM addresses must be reachable from the container:

```sh
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --mount "type=bind,source=$(pwd)/data/config.k8s.yaml,target=/etc/tgaia/config.yaml,readonly" \
  --mount "type=bind,source=$(pwd)/data/system-prompt.k8s.md,target=/etc/tgaia/system-prompt.md,readonly" \
  "$TGAIA_IMAGE" --config /etc/tgaia/config.yaml db-migrate

docker run --rm --name tgaia --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,nosuid,size=64m -p 127.0.0.1:8080:8080 \
  --mount "type=bind,source=$(pwd)/data/config.k8s.yaml,target=/etc/tgaia/config.yaml,readonly" \
  --mount "type=bind,source=$(pwd)/data/system-prompt.k8s.md,target=/etc/tgaia/system-prompt.md,readonly" \
  -e TGAIA_ROLE=all "$TGAIA_IMAGE" --config /etc/tgaia/config.yaml serve
```

Files must be readable by UID/GID 10001 inside the container. On Linux, use a suitable
group and mode 0440 or a secret volume; do not make configuration world-readable.
On Docker Desktop, verify permissions for your mount setup. The HTTPS reverse proxy
in front of the published localhost port must match public_url and the OIDC callback.
Local mock/loopback YAML is not a production container configuration.
Stop with `docker stop --time 45 tgaia`.

## 2. Prepare infrastructure and configuration

Requirements:

- PostgreSQL reachable from every pod, a dedicated database and backups. The migration
  Job needs DDL privileges; the application needs DML. `db-migrate` creates the schema.
- A working OIDC issuer (for example, a Keycloak realm), confidential client, client
  secret and an ID-token `groups` mapper. Callback: `https://<host>/auth/callback`.
- An HTTPS LiteLLM endpoint, model/router name and Telegram bot token.
- Ingress controller, DNS and a TLS certificate for the UI. For private images, add
  registry credentials through imagePullSecrets in pod specs.

```sh
mkdir -p data
cp config/config.example.yaml data/config.k8s.yaml
chmod 600 data/config.k8s.yaml
cp config/system-prompt.md data/system-prompt.k8s.md
# Edit data/config.k8s.yaml using the production alternatives documented inline.
```

Change app.listen to `0.0.0.0:8080` to match this example's Service/probes. Replace
the SQLite DSN with the PostgreSQL URL described below, set app.public_url to the
external HTTPS URL without a path, app.oidc.mode to oidc, app.telegram.mode to polling
and app.llm.mock to "false". Supply the documented issuer/client/group, LLM and bot
credentials. Every YAML leaf is a string, including booleans and IDs. LiteLLM uses an
OpenAI-compatible endpoint; the adapter adds /v1 if necessary.

session_key is a persistent random secret of at least 32 characters, identical across
replicas. It signs sessions and encrypts secret DB overrides. Back it up with the
database: replacing it arbitrarily makes existing encrypted overrides unreadable.
Generate one with `openssl rand -hex 32`. admin_groups is a comma-separated string
of exact administrator group values from the ID-token `groups` claim.
One bot_id belongs to one agent_id in the database.
Real bot ID/username are resolved through getMe; supply the complete token.

Use a DSN such as
`postgres://<user>:<password>@<host>:5432/tgaia?sslmode=verify-full&sslrootcert=/etc/ssl/certs/ca-certificates.crt`.
For private CAs, add certificates to the image or a read-only volume and configure PostgreSQL,
LiteLLM/OIDC/MCP trust accordingly; keep TLS verification enabled. URI passwords
must be percent-encoded. Replace sslrootcert with the actual mounted CA path when
PostgreSQL does not use public PKI.

## 3. Create the namespace, Secret and ConfigMap

Run these commands in the intended kube-context. Check it first; this repository
does not select a cluster automatically.

```sh
kubectl config current-context
export TGAIA_NAMESPACE=tgaia
kubectl create namespace "$TGAIA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$TGAIA_NAMESPACE" create secret generic tgaia-config \
  --from-file=config.yaml=data/config.k8s.yaml --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$TGAIA_NAMESPACE" create configmap tgaia-prompt \
  --from-file=system-prompt.md=data/system-prompt.k8s.md --dry-run=client -o yaml | kubectl apply -f -
```

A read-only projected volume mounts both files in /etc/tgaia without subPath.
Do not commit populated configuration or Secrets; data/ is excluded from Git and
the Docker context. Production deployments may use an external secret manager.

## 4. Run migrations before starting the application

Render every role with the same published image:

```sh
sed "s|tgaia:replace-me|${TGAIA_IMAGE}|g" examples/kubernetes/migration.yaml > data/k8s-migration.yaml
sed "s|tgaia:replace-me|${TGAIA_IMAGE}|g" examples/kubernetes/workloads.yaml > data/k8s-workloads.yaml
kubectl -n "$TGAIA_NAMESPACE" apply -f data/k8s-migration.yaml
kubectl -n "$TGAIA_NAMESPACE" wait --for=condition=complete job/tgaia-migrate --timeout=300s
kubectl -n "$TGAIA_NAMESPACE" logs job/tgaia-migrate
# Continue only after the Job succeeds.
kubectl -n "$TGAIA_NAMESPACE" apply -f data/k8s-workloads.yaml
kubectl -n "$TGAIA_NAMESPACE" rollout status deployment/tgaia-api --timeout=180s
kubectl -n "$TGAIA_NAMESPACE" rollout status deployment/tgaia-worker --timeout=180s
```

The migration Job is applied and awaited before the workloads so Deployments cannot
race ahead of schema changes. Migrations are idempotent and serialized with a PostgreSQL
advisory lock. serve does not run them. To rerun the Job, first confirm the previous
one has finished, then delete job/tgaia-migrate and reapply it. Do not delete a running
Job to start another.

API and worker replicas have startup/readiness/liveness probes. /readyz checks schema access and, for worker/all roles, that the worker group is running. /healthz checks the HTTP process. Worker listeners expose only health/readiness/metrics, not administration or webhook routes. Example resource requests/limits need load-based tuning.
Pods have no Kubernetes API token, run non-root with a read-only root filesystem,
drop all capabilities and use seccomp.

## 5. Expose the Web UI and verify Telegram

Copy ingress.example.yaml into data/, replace ingressClassName, host and TLS
secretName, then apply it in the same namespace. Your ingress/certificate manager
must provide the TLS Secret; the manifest does not create certificates.
The host, public_url and OIDC client callback must agree.

```sh
cp examples/kubernetes/ingress.example.yaml data/k8s-ingress.yaml
# Replace placeholders and prepare the TLS Secret.
kubectl -n "$TGAIA_NAMESPACE" apply -f data/k8s-ingress.yaml
kubectl -n "$TGAIA_NAMESPACE" get pods,svc,ingress
kubectl -n "$TGAIA_NAMESPACE" logs deployment/tgaia-api --tail=100
kubectl -n "$TGAIA_NAMESPACE" logs deployment/tgaia-worker --tail=100
```

Open the external URL, sign in through OIDC, send the bot a test message and create
a one-shot reminder. Verify history, task and delivery in the UI/Telegram.
For health diagnostics, port-forward the Service to localhost. OIDC will not work
through that URL unless it matches the configured public_url.

Polling needs no inbound webhook. A DB lease selects one receiver per bot_id;
other API replicas wait for takeover. Startup/token changes require getMe. If
Telegram is unavailable, startup may fail and Kubernetes will retry. Remove any
existing webhook for the bot before enabling polling.

Alternatively, use app.telegram.mode: webhook with a separate random
bots.<alias>.webhook_secret of at least 16 characters per bot. The operator registers
Telegram setWebhook at `https://<host>/telegram/<bot_id>` with the matching
secret_token; the application does not register it automatically. Ingress must pass
that route and X-Telegram-Bot-Api-Secret-Token without an external browser-login gate.
The application's UI/API have their own OIDC protection. Never run polling and
webhooks simultaneously for one bot; coordinate Telegram registration with app
configuration when switching. See [Telegram setWebhook](https://core.telegram.org/bots/api#setwebhook).

## 6. Settings, scaling and upgrades

- DB overrides apply automatically across replicas, typically within 250ms plus
  preparation. listen/public_url/role are hidden from the UI. Do not create a shared
  role override: it supersedes TGAIA_ROLE and breaks the API/worker split. Inspect
  inherited databases for these overrides through operator access before startup.
- YAML/OIDC are startup snapshots. After changing the Secret, restart both Deployments;
  updating the Secret alone does not trigger a rollout.
- system-prompt.md is read before new generations when a bot's DB prompt is empty.
  Update its ConfigMap using section 3. Once the mounted file updates, new generations
  use it without restart; Kubernetes volume propagation is not instantaneous.
- Scale API/workers independently. PostgreSQL coordinates queue FIFO, heartbeat,
  fenced writes and polling leases. The current PoC runs four worker handlers per
  pod; account for aggregate LiteLLM/MCP and DB connection capacity.

```sh
kubectl -n "$TGAIA_NAMESPACE" scale deployment/tgaia-api --replicas=2
kubectl -n "$TGAIA_NAMESPACE" scale deployment/tgaia-worker --replicas=3
# After changing the bootstrap Secret:
kubectl -n "$TGAIA_NAMESPACE" rollout restart deployment/tgaia-api deployment/tgaia-worker
```

Before upgrading, back up the database and preserve session_key. For migrations
incompatible with old code, especially early migration 003, stop both roles with
scale --replicas=0, migrate with the new version, then apply the new Deployments.
This requires downtime; universal zero-downtime schema compatibility is not guaranteed.
For compatible schemas, RollingUpdate replaces replicas gradually. The 45-second
termination grace period allows worker cancellation, polling lease release and HTTP
shutdown. Inspect uncertain deliveries/tool steps; unknown external outcomes are
not blindly retried.

Use kubectl rollout undo only if the old version understands the current schema and
settings. There are no automatic down migrations. Restoring an earlier database is
a separately agreed recovery procedure, not part of rollout undo.

## 7. Networking and troubleshooting

Allow DNS, PostgreSQL, Telegram HTTPS, LiteLLM, OIDC discovery/JWKS/token endpoints
and configured MCP endpoints. NetworkPolicy depends on your CNI/service addresses;
the example deliberately does not embed a generic external-IP policy. API ingress
must permit ingress-controller traffic and probes. Workers do not need a Service.

Common checks:

- ImagePullBackOff: image tag, registry secret and node access to the registry.
- CrashLoopBackOff: `kubectl logs <pod> --previous`, configuration, getMe, DB/schema and OIDC.
- OIDC callback errors: issuer realm, client secret, exact callback and administrator groups claim.
- Missing responses: role overrides, worker logs/queue, user/chat blocks, Telegram
  transport and LLM model/key. For default-prompt errors, check the mounted Markdown file.
- Settings decryption errors: mismatched or replaced session_key.
- Late reminders: the shared scheduler Tick uses memory.interval (30s in the example),
  followed by queue, model and Telegram latency.

Before applying, validate the prepared files against the target cluster API:
`kubectl -n "$TGAIA_NAMESPACE" apply --dry-run=server -f data/k8s-migration.yaml -f data/k8s-workloads.yaml`.
Full cluster rollout, production OIDC/LiteLLM and
PostgreSQL failover need a separate integration environment.

References: [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/),
[Kubernetes projected volumes](https://kubernetes.io/docs/concepts/storage/projected-volumes/),
[Secret volume updates](https://kubernetes.io/docs/concepts/configuration/secret/#using-secrets-as-files-from-a-pod).

## Completed verification

Container checks covered non-root UID,
absence of config/prompt in the image, CLI, PostgreSQL 16 migrations through schema 9,
db-check, read-only startup with external files, readyz/healthz and SIGTERM exit 0.
The smoke test used mock OIDC/Telegram/LLM and no real user data. YAML, mount and
security checks passed. Manifests were not applied to a Kubernetes
cluster; multiarch push and production failover remain untested.


## Observability and audit

Scrape `/metrics` on API and worker pod ports. Counters include job attempts/errors, accumulated duration and active handlers; queue counts are database-wide, so avoid summing them across replicas. Process counters reset on restart. Keep metrics and probe routes internal using ingress routing/NetworkPolicy; they have no OIDC and contain no chat IDs, prompts or credentials.

Set `app.observability.otlp_endpoint` (or `TGAIA_OTLP_ENDPOINT`) explicitly to enable OTLP/HTTP tracing; an empty value disables export. Database overrides hot-reload it. Worker jobs are root spans, with nested LLM/MCP spans; no prompts, tokens, model responses or raw errors are exported. Sampling is parent-based at 10% for roots. Export uses a bounded batch queue, a 5s timeout and shutdown flush. No collector endpoint is built into defaults. See the [official Go exporter documentation](https://opentelemetry.io/docs/languages/go/exporters/).

Migration 014 adds a paginated admin Audit view. Successful administrative mutations and their actor/operation/request/before/after record commit atomically; rejected authenticated mutations are recorded separately. Credential-bearing settings, MCP environment/arguments/URLs and tool-resolution result bodies are redacted. Audit records have no update/delete endpoint. This is an application audit, not a tamper-proof database-admin or OS audit. Operational scheduler/queue transitions remain in their dedicated durable tables. External MCP discovery is read-only and is not an administrative data mutation.
