# Durable queue and workers

The queue and worker engine are implemented as Go packages and wired into `serve`
with `all` and `worker` roles. Handlers cover Telegram responses, LLM/MCP calls,
compaction and delivery; scheduler producers and Kubernetes manifests are implemented.
See [design.md](design.md) for verification status and remaining production checks.

## Storage and migrations

For local development, the default command migrates and then starts the application:

```sh
CGO_ENABLED=0 go build -o bin/tgaia ./cmd/tgaia
./bin/tgaia --config config/config.example.yaml
```

Production should instead run `db-migrate` in a migration Job, then start every
application replica with explicit `serve`. PostgreSQL migrations take
an advisory transaction lock. SQLite is the local development adapter, not a shared Kubernetes volume
or a replacement for PostgreSQL concurrency tests.

`Database.Queue()` returns the queue adapter. `Enqueue` accepts a stable agent/bot/chat scope, deduplication
key, kind, JSON payload (at most 1 MiB), and maximum attempts (1–100). Repeating a key within the scope
returns unchanged, including after completion or exhaustion. Producers must never change payloads under
an existing key. Independent agents, bots and chats have independent deduplication namespaces.

Queue state is `pending → leased → completed`, or `leased → retry → leased`, eventually `dead`.
An expired lease can be reclaimed; an expired final attempt is retired in bounded batches during polling.
Rows are retained, so deduplication survives completion. A retention policy must preserve dedupe markers
for the required replay window; cleanup is not implemented yet.

### Failure classes and dead jobs

Handlers classify failures with `queue.Fail(class, terminal, err)` and annotate the phase with
`queue.WithPhase`. The worker logs only the bounded class and phase (`llm_timeout`, `llm_429`,
`llm_http_5xx`, `llm_truncated`, `llm_budget`, `embedding_timeout`, `quota`, `panic`, `job_timeout`,
`handler_error`, …), never the error text, and exports `tgaia_queue_failures_total{kind,class}`.
Terminal classes (model timeouts, truncated output, exceeded budgets, 4xx rejections) retire the job
at once through `Store.Fail`: the same prompt would fail the same way on every retry. Other failures
retry with backoff until `max_attempts`. A job-level deadline retries, because tool turns and results
are checkpointed and the next attempt resumes from them.

When a `respond` job becomes dead, the worker calls the registered `OnDead` hook once. The engine
then sends a short reply to the originating message explaining that no answer is coming (timeout,
truncated output, budget, generic error). The notice is best effort and outside the durable
delivery protocol.

Dead jobs are listed in the Web UI (Outgoing messages → Dead jobs) with a **Replay** action backed
by `POST /api/queue-replay`. `Queue.Replay` resets a dead job to `pending` with a fresh attempt
budget; tool-step checkpoints, cached results and delivery receipts remain, so replay resumes
rather than repeats finished work.

## Concurrency and ordering

PostgreSQL claims use atomic `UPDATE … RETURNING` with `FOR UPDATE SKIP LOCKED` in the candidate query.
SQLite uses atomic `UPDATE … RETURNING` with its writer serialization. No network call or handler runs
inside a database transaction.

Within `(agent_id, bot_id, chat_id)`, only the earliest nonterminal row can run. Enqueues acquire a scope row
lock before allocating IDs: commit order cannot introduce an unseen earlier job after a later claim.
Different scopes can run concurrently. An unsupported job kind at the head blocks its scope deliberately;
rollouts must register handlers for all outstanding kinds or explicitly resolve those jobs.
A retry delay blocks later jobs of the same chat. Scheduled future tasks live in the scheduler's task
store and enter this execution queue only when due; do not enqueue a distant future task at the head.

Every claim increments the attempt count and generates a fresh lease token. Extend, complete and retry
require that token and an unexpired lease. The database supplies the time, so pod clock skew does not
control ownership. Stale workers cannot change queue state after expiry/reclaim.

These guarantees do **not** fence external services. A worker paused past its lease can have an in-flight
request with an unknown result. Handlers must honor cancellation, implement domain idempotency where
possible, and define recovery for ambiguous external effects. Telegram delivery is not exactly-once.
Domain checkpoints must also check ownership/version; queue fencing alone does not protect
arbitrary writes by handlers. The application uses dedicated atomic repository operations
for durable inbox/history/enqueue and fenced domain checkpoints.

## Worker engine

`queue.NewWorker(store, handlers, options, logger)` validates dependencies and copies the handler registry.
`Run(ctx)` owns a bounded pool and stops claims when cancelled. Handler contexts have a task timeout.
A heartbeat renews ownership; renewal failure cancels the handler and prevents settlement by that worker.
Failures and panics use bounded exponential backoff, without persisting/logging raw handler errors.
On shutdown, unfinished leases expire for another replica to reclaim. Handlers must stop on cancellation;
exceeding shutdown timeout is a fatal error and the host process must exit.

Default worker options (the runtime uses these defaults; configurable limits remain pending):

| Option | Default |
| --- | --- |
| Concurrency per process | 4 |
| Poll interval | 1 second |
| Lease | 30 seconds |
| Heartbeat | 10 seconds |
| Task timeout | 12 minutes (`worker_timeout`; must exceed `tool_timeout` + `llm_timeout`) |
| Shutdown timeout | 15 seconds |
| Retry backoff | 1 second, doubling up to 1 minute |

For Kubernetes, keep `terminationGracePeriodSeconds` greater than worker shutdown timeout plus
resource-close allowance. Stateless worker pods use the same PostgreSQL database and stable agent and bot IDs,
never pod identity as the conversation scope. Readiness/schema checks and queue-depth/oldest-job metrics
must be wired before deployment; future autoscaling should account for queue age and DB/LLM capacity.

## Verification

```sh
CGO_ENABLED=0 go test ./...
go test -race ./internal/queue ./internal/storage
```

Set `TEST_POSTGRES_DSN` through your environment to run the same queue contract on a real PostgreSQL
server. The suite creates a uniquely named test schema and removes **that schema only** afterward;
the test role needs schema creation privileges. Use a dedicated test database. Without the variable,
PostgreSQL cases skip explicitly.

Coverage includes idempotency, independent consumers/connections, per-chat ordering, concurrent enqueue,
expired lease recovery, stale-owner rejection, attempt exhaustion, worker heartbeats, timeouts, panics,
lease loss, cancellation and bounded shutdown. PostgreSQL execution remains required before declaring
production concurrency verified.

## Multiple bots and user blocks

Register bot ownership through `Database.Bots().Register` before enqueueing work or recording outbound
messages. The registration stores a credential reference, not a token. Runtime startup
registers configured bots and verifies real identities with Telegram getMe. Every scope includes a positive Telegram bot user ID; FK constraints reject unregistered bots.

For user-originated work, producers must provide `ActorUserID` from the trusted ingress event (or the
schedule author). Zero is reserved for application-generated system work, never an anonymous-user fallback.
`Database.Access().SetUserBlocked` is a privileged persistence method; the admin API
authorizes it through OIDC and the administrator group allowlist.
Blocks apply to every bot and chat of the agent. Enqueue, claim, lease renewal and settlement check this policy.
A bounded poll sweep retires blocked users' outstanding work. Unblocking does not replay it automatically.
Handlers must also call `CheckUser` before external effects; a DB block cannot retract an in-flight HTTP call.

Migration 003 requires stopping old consumers before the schema upgrade. Legacy rows have `bot_id=0`,
retain their data and cannot be claimed or accessed by ordinary scoped APIs until explicitly mapped.
No CLI/UI migration mapping exists yet. Fresh installations start without any legacy rows.
