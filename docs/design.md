# tgaia — technical specification

Status: working specification for further design and implementation.

## Implementation status and continuation guide

This section is the primary implementation summary. Checkmarks describe verified PoC progress, not production readiness.

### Implemented and verified

- [x] Go 1.27.3 in go.mod and Dockerfile; builds and checks use this toolchain.
- [x] MCP SDK v1.8.0: HTTP/SSE/stdio, native tool loop, per-bot allowlists, schema validation and durable checkpoints; MCP CRUD/discovery/reset in the Web UI and file configuration.
- [x] Go CLI (`urfave/cli/v3`, altsrc), strict YAML/environment parsing, safe diagnostics and CGO-free builds.
- [x] GORM SQLite/PostgreSQL adapters and explicit migrations 001–011. SQLite is tested; a PostgreSQL 16 Docker smoke test verified migrations and startup. Full PostgreSQL storage contracts and failover remain unverified.
- [x] DB queue: deduplication, scoped FIFO, leases/heartbeats/retries, bounded workers, cancellation/shutdown and concurrent storage tests.
- [x] Multiple bots per agent; history, summaries, tasks, deliveries and message IDs isolated by `(agent_id, bot_id, chat_id)`.
- [x] Agent-wide user ID blocks and separate chat blocks. New messages from blocked authors do not enter LLM context/history, but users remain visible to administrators.
- [x] Atomic inbox/history/enqueue; polling offsets advance after durable acceptance; per-bot polling receiver leases; webhook endpoint with a secret header.
- [x] Private messages, mentions of the receiving bot and replies to that bot trigger responses. Entity offsets use UTF-16; replies to someone else do not trigger responses by themselves.
- [x] OpenAI-compatible HTTP adapter with configurable LiteLLM endpoint/model/key. JSON Schema response formats and native tool calls are covered by opt-in integration tests.
- [x] Per-bot DB system prompts, explicit user-requested chat memory and separate administrator-published shared knowledge, never populated automatically from chats.
- [x] Background compaction checkpoints only whole messages actually supplied to the model and continues after input limits. Editing/deleting an answer invalidates its summary; migration 008 adds memory_revision to rebuild deduplication keys.
- [x] Scheduler: user requests create tasks through the LLM; UI CRUD; a short transaction coordinates scheduler replicas.
- [x] Durable send intents, cached LLM results and outbound message IDs. Uncertain outcomes are not blindly resent. UI edit/delete uses the queue and verified ID registry.
- [x] Standard Markdown replies are rendered as safe Telegram HTML for send/edit; raw HTML is escaped and only supported link schemes are emitted.
- [x] OIDC discovery/signature/audience/issuer validation, nonce/state/PKCE, administrator group allowlist, signed HttpOnly sessions and Origin/CSRF checks. Local mock OIDC was explicitly authorized; browser login and HTTP security tests passed.
- [x] Web UI: history/summaries, users/chats/blocks, tasks, shared knowledge, prompts and DB overrides; secrets can be changed without being returned to clients.
- [x] Runtime settings, including log_level, support DB overrides. DB connection/timeout and OIDC are bootstrap-only. Replicas apply DB changes without restart (section 19); prompts are read for subsequent jobs.
- [x] Default local startup runs versioned migrations then serves; production uses explicit `db-migrate` followed by `serve`. Roles all/api/worker and graceful SIGINT/SIGTERM handling are supported.
- [x] Full `CGO_ENABLED=0 go test ./...`, `go vet ./...` and build passed. Queue/storage/service/webauth race tests passed.
- [x] Browser-driven integration tests covered message processing, reply persistence and reminder creation with isolated test data.
- [x] Manual live-adapter verification covered Telegram identity discovery, polling and an explicitly configured OpenAI-compatible endpoint.
- [x] DB hot reload on each replica and UI/API protection for listen/public_url/role (sections 19 and 21).
- [x] Scheduled execution mode and real Telegram reminder E2E (section 20).
- [x] Typing lifecycle implemented and tested; live visual confirmation remains pending (section 22).
- [x] Markdown fallback `system-prompt.md` beside the config; individual DB prompts take precedence (section 23).
- [x] Makefile build/test/test-race/vet/check targets implemented and verified (section 24).

### Local setup and continuation

- Keep populated configuration outside version control with mode 0600. Never print secrets or commit runtime data.
- `app.telegram.bots.<alias>.token` is the complete BotFather token, `ID:secret`. `id` and `username` are needed for mocks; real mode resolves both through getMe. Do not remove the ID prefix from the token.
- The development UI uses `http://127.0.0.1:8080/` with explicit mock or live adapters. Use README instructions for reproducible builds; no machine-local binary path is part of the project contract.
- Migration 007 prevents switching one database between mock and live Telegram. Use a separate ignored database for simulated demonstrations.
- Local development may run `bin/tgaia --config config.yaml` to migrate then serve. Production runs `db-migrate` separately and starts replicas with explicit `serve`.
- Secret DB overrides use AES-GCM with a purpose-specific key derived from the persistent bootstrap session key. Preserve that key across restarts and with DB backups.
- A local OpenAI-compatible endpoint may use `http://127.0.0.1:1234`; a production router should use an explicitly configured HTTPS URL and model. These are explicit profiles, never automatic fallbacks or defaults.
- Production OIDC requires the exact issuer URL and administrator group values; a bare hostname is not an issuer URL.

### Remaining work and PoC limitations

- [x] End-to-end delivery and scheduled reminder execution were verified with isolated test data (section 20).
- [ ] Run storage contracts against a dedicated PostgreSQL via `TEST_POSTGRES_DSN`; test failures across multiple processes/replicas.
- [x] Two-stage Dockerfile, externally mounted config/prompt and Kubernetes guide; Docker build/PostgreSQL smoke tests passed (section 25).
- [ ] Kubernetes cluster rollout/failover remains untested; webhook registration is external.
- [x] Metrics, OpenTelemetry traces, worker health endpoint and transactional administrative audit (section 31).
- [x] Reader/user access matrix with global user and chat block precedence (section 30).
- [x] Images/text attachments, edited updates, isolated forum topics and reader-only channel/anonymous handling (section 29). Audio/video recognition is explicitly disabled pending models.
- [ ] Calendar/timezone recurrence. Current schedules are one-shot or fixed interval >=60s, stored as UTC milliseconds. After downtime they emit one overdue occurrence, then advance into the future. Scheduler ticks share memory.interval.
- [ ] History/explicit-memory retention, conversational deletion and a vector index for large memory sets. The PoC has conversation messages, summaries and bounded bge-m3-compatible semantic retrieval.
- [x] Configurable worker/pool/LLM/compaction limits, timeouts, concurrency, token budgets and quotas (section 32). Structural protocol/storage ceilings remain fixed.
- [ ] Full application validation in config-check (currently storage/CLI only); model/prompt-version provenance for summaries.
- [x] UI/workflow for manually resolving uncertain deliveries/tool outcomes, with version checks and durable administrator decisions (section 28).
- [x] Shared plus API/worker DB setting scopes, with deployment-local listeners/public URL and bootstrap-only process role (section 33).
- [ ] Production OIDC and LiteLLM structured-output/tool compatibility, load testing, real Telegram webhook/edit/delete integration, user-provided MCP servers and visual confirmation of live typing.

## 1. Purpose and scope

Build a Telegram AI agent in Go. This document records the required stack and engineering conventions. Unspecified features remain open questions rather than agreed requirements.

It also guides future development sessions: continue from recorded decisions, identify assumptions explicitly and support completion claims with verification.

## 2. Required stack

| Area | Requirement |
| --- | --- |
| Language | Go |
| Entrypoint | `github.com/urfave/cli/v3` |
| Configuration sources | `altsrc` integrated with the CLI; one typed runtime configuration |
| Database access | GORM |
| Production database | PostgreSQL |
| Local database | SQLite |
| Build | Both database adapters work without CGO; verify with `CGO_ENABLED=0` |
| Web UI | Required administrative interface using `github.com/go-chi/chi/v5`, protected by OIDC |

Check compatibility before selecting dependency versions, Telegram/LLM SDKs and database drivers. Pin versions in `go.mod`/`go.sum`. Do not replace required components without a user decision.

## 3. Architecture

- Keep `cmd/tgaia` thin: parse commands, create the root context, start the application and convert the final error to an exit code.
- Put application logic in `internal/`, organized by capability and responsibility. Add layers only when useful.
- Wire concrete adapters in one composition root; avoid `init()` wiring and mutable globals.
- Separate Telegram, LLM, tools and storage from application rules. Use narrow interfaces owned by consumers or stable domain capabilities.
- Keep GORM, SQL and SDK types out of application contracts. Constructors return valid objects without starting background work.
- Give each goroutine, transaction, queue, timer and resource an explicit owner. Treat a constructed dependency graph as immutable.

## 4. Configuration and lifecycle

- Source precedence: explicit CLI flags → environment → config file → defaults. Verify this project policy through the altsrc integration.
- Parse and normalize, then validate fully before opening resources.
- Type durations, URLs, limits and enums. Reject unknown file keys and misspellings under the application's own environment prefix; ignore unrelated environment variables.
- Do not introduce a second independent configuration framework.
- Enable optional modules only through valid configuration sections; an absent section must not enable functionality.
- Telegram/LLM tokens and passwords have no meaningful defaults and must not appear in logs, errors, CLI arguments or effective-config output. Redact secrets structurally.
- Long-running work uses `Run(ctx)`. On SIGINT/SIGTERM stop accepting work, cancel and join owned workers within a shutdown deadline, then close resources in reverse order.

## 5. Telegram processing and reliability

These requirements apply the engineering conventions to Telegram. The PoC supports polling and webhooks; the local profile uses polling.

- Persist accepted updates idempotently before acknowledgment. Define the safe polling-offset advancement and successful webhook-response boundaries separately.
- Duplicate delivery must not create duplicate application work. Specify deduplication keys and their uniqueness scope in storage.
- Recover unfinished jobs after restart; distinguish pending, leased/in-progress, completed and retry-scheduled states.
- Bound concurrency, queue processing, execution time, attempts and backoff; context cancellation stops retries.
- Perform external actions outside DB transactions. A failure after commit does not undo a durable checkpoint; it requires separate retry/recovery handling.
- Document uncertain Telegram API results and crashes between sending and recording the result. Do not promise exactly-once external delivery.
- Serialize work within `(agent_id, bot_id, telegram_chat_id)` while processing different chats concurrently. Workers across Kubernetes pods share the DB queue.

## 6. Data storage

- Repositories expose use cases, explicit partial updates and semantic results: applied, unchanged or conflict.
- Application operations define atomicity boundaries without passing ORM transactions/sessions into application code.
- Prevent lost updates with transactions, optimistic versions or atomic conditional updates.
- Preserve the same domain guarantees in PostgreSQL and SQLite; isolate dialect differences in adapters.
- Version and test migrations separately. Normal startup must not silently perform destructive or long-running schema changes.
- Bound growing queries with stable ordering; prefer cursor pagination with a tie-breaker for runtime data.
- Store time in UTC. Define retention, deletion and recovery ownership for messages, history, model results and service records.

### 6.1. Context and memory isolation

Each Telegram bot has its own context and memory in each chat. Two bots of the same agent in the same chat must not share history, summaries or local facts. Groups, channels and private chats do not share history automatically. Administrator-published shared knowledge is a separate layer.

| Scope | Contents | Read access | Write access |
| --- | --- | --- | --- |
| Chat | History, summaries, local long-term memory and tool results | Processing within that bot/chat scope only | Authorized operations in that scope only |
| Agent-wide knowledge | Explicitly administrator-published information, not bot memory | Any permitted chat of that agent | Agent administrators only |

- Memory/execution scope is `(agent_id, bot_id, telegram_chat_id)`, never names, usernames or participant IDs. One user does not automatically share memory between private chats and groups.
- A channel and its discussion group are separate scopes. Forwards, replies, links and mentions do not authorize access to another chat. Group ID migration requires an explicit transfer, not name-based merging.
- History, summaries, facts, embeddings, attachment references, tool results and schedules carry scope keys. Repositories require scope explicitly; missing scope is an error, never a query across all chats.
- Restrict searches, including vector search, in storage before returning results. Cache keys and external LLM conversation IDs include scope. Filtering a completed model answer cannot provide isolation.
- Assemble model context from trusted application instructions, selected shared knowledge and current-chat data only. Shared knowledge remains data and cannot override instructions, permissions or scope.
- Never publish chat content or model output to shared knowledge automatically. Creating, editing or deleting shared entries requires separately authorized administrative operations; a model suggestion is not authorization.
- Administrators authenticate through OIDC; the server authorizes every action. The session retains `sub` for identity validation, while administration requires an exact match between the ID-token `groups` claim and the configured group allowlist. Telegram group administrator status does not grant agent administration rights.
- Shared entries have IDs, versions, authors and UTC timestamps. Changes check the expected version and invalidate affected caches/search views. Never automatically copy private data into this layer available to all permitted chats.
- Scheduled tasks retain chat scope and response destination. Execution/retries use that scope's memory plus shared knowledge; prompts/tool arguments cannot choose another chat.
- Deleting one chat's memory must not affect other chats or shared knowledge. Define coordinated retention for history, summaries and indexes; deleted data must not reappear from stale caches.
- Memory scope within forum groups is the entire chat (decided 2026-11-04, migration 018): history, the compaction summary and explicit memories are shared by all topics; replies, reply links and scheduled tasks keep their topic. Channel context requirements do not automatically define channel ingestion/publication behavior.

### 6.2. Background memory compaction

- A periodic service job selects one chat's messages over a configured window and asks the LLM for a compact summary. Compaction is separate from user delivery tasks; its result is stored, not sent to Telegram.
- Message window and run frequency are separate settings. Originally proposed keys: `memory.compaction.window`, `memory.compaction.interval`, `memory.compaction.max_input_tokens`, `memory.compaction.max_output_tokens`, `memory.compaction.timeout`, `memory.compaction.concurrency`. These are design proposals, not a list of implemented configuration keys; consult the config examples for current names.
- Process a stable scoped range with upper cursor `(received_at, id)`. Messages arriving during the model call remain beyond the checkpoint for later processing. Split large ranges within context budgets.
- Call the LLM outside transactions. Atomically save validated summaries, source ranges, prompt/model versions and checkpoints. Errors, cancellation or invalid results must not advance checkpoints. Retries/concurrent workers must not create multiple active summaries for one range. Prompt/model provenance remains pending in the PoC.
- Response context combines selected summaries with uncovered recent messages under an explicit size budget. Summaries remain untrusted data, never system instructions or shared knowledge.
- Compaction does not delete source messages automatically. Retention is a separate, unresolved policy. Source edits/deletions require summary recomputation or invalidation consistent with deletion policy.
- Test cross-chat isolation, LLM/restart recovery, period boundaries, large ranges and messages arriving during compaction.

### 6.3. Web UI, access and task management

- Provide an OIDC-protected chi v5 Web UI showing discovered users/chats, permissions/blocks and all scheduled tasks with prompts and CRUD operations.
- Authentication and administrator authorization are separate checks. Protect every administrative API, including tasks. Implement sessions, CSRF, issuer/audience validation and OAuth state/nonce. Configure issuer, client ID/secret and callback URL without exposing secrets.
- Discover users/chats from available Telegram updates; do not promise complete membership or identities hidden by Telegram. Trigger-based replies and task creation are allowed by default; administrators grant expanded rights.
- `(agent_id, telegram_user_id)` blocks apply across all bots/chats, including private messages. Chat blocks override participant permissions; chat-level allowance never bypasses a global user block. Block responses, new LLM/tool operations and scheduled sends, and recheck before external delivery. The PoC's handling of incoming blocked-user context is specified in section 16.
- Tasks store author, chat scope, prompt, schedule, state and version. The LLM proposes structured data; the application validates it and derives author/scope from authenticated events. Prompts cannot choose another chat or elevate privileges.
- Cross-chat administrative task viewing is a distinct UI privilege, not permission to mix model memory. Audit rights/task changes without logging secrets or prompts in ordinary logs.

## 7. LLM and tools

- Depend on narrow capabilities, not a universal SDK client. Intended provider: LiteLLM, model/router `Default`; verify protocol/authentication before production integration.
- Prompts and schemas have owners/versions and live in separate assets or specialized source files.
- Treat Telegram messages, retrieved content, tool results and previous model output as untrusted data, separate from trusted instructions.
- Select context using an explicit size-bounded policy. Bound tool rounds, total duration and result size; enforce model concurrency and cost/token budgets.
- Validate machine output against closed schemas and deterministic domain rules. Model errors are not successful checkpoints.
- Do not share a client with a constructor-imposed schema across roles requiring incompatible output formats.
- Tools require typed inputs/outputs, timeouts, limits and authorization. Models propose actions; the application permits them. Define mutation/confirmation policies before enabling such tools.

## 8. Observability and verification

- Use `log/slog` structured logs, stable event schemas, low-cardinality metrics and OpenTelemetry traces for operational flows.
- Never log secrets, raw prompts/messages or sensitive tool payloads, including in metric labels.
- Test application rules with handwritten fakes behind narrow dependencies.
- Run common storage conformance tests on SQLite and PostgreSQL; SQLite cannot verify production SQL concurrency semantics.
- Cover duplicate updates, crash recovery, update conflicts, retry bounds, cancellation, shutdown, post-commit failures and uncertain external sends.
- Test malformed model schemas, prompt injection, exceeded limits and unauthorized tool calls.
- Verify isolation across groups, channels and private chats: the same user/request in different chats must not mix history, memory or caches. Requests for another chat's data never expand scope.
- Verify shared-knowledge reads across permitted chats, denied non-admin writes, no automatic publication of local memory, invalidation after changes/deletion and scheduler retry isolation.

## 9. Proposed implementation stages

1. Clarify the minimal user scenario and open decisions in section 11.
2. Create the Go module, CLI and configuration; test precedence, errors and secrets.
3. Implement storage adapters, migrations and common tests for both databases.
4. Implement Telegram ingestion, durable inbox and recoverable workers.
5. Integrate the agreed LLM scenario and allowed tools.
6. Verify E2E behavior, recovery, shutdown and CGO-free builds; document operation.

This is a working plan, not authorization to add unconfirmed product features.

## 10. First-version acceptance criteria

- The agreed scenario works from incoming Telegram message to delivered reply.
- CLI/altsrc produces one validated configuration with documented precedence.
- Both DB adapters build and pass relevant checks with `CGO_ENABLED=0`.
- Duplicate delivery/restarts do not lose accepted jobs; external delivery limits are documented and tested.
- Shutdown and processing are bounded; diagnostics redact secrets structurally.
- OIDC and server-side authorization protect the UI; blocks affect dialogue and scheduled delivery.
- Administrators manage tasks through CRUD; permitted users create them through the agent.
- Compaction saves summaries/checkpoints without losing messages or mixing chat contexts.
- Enabled features have tests, migrations and operating instructions; unverified integrations are explicitly identified.

## 11. Open product decisions

- Exact assistant tasks, commands and MVP scenarios beyond the implemented core.
- Permission levels and detailed rights beyond basic access/chat blocks.
- Administrator-configured MCP servers/tools via the official Go SDK, UI or config; production OIDC issuer and administrative role mapping.
- Kubernetes transport choice: webhook ingress replicas or long polling with one leased receiver per bot. Workers scale independently; both transports are implemented.
- Compaction windows/frequency/summary budgets, source retention after compaction and forum-topic isolation. Basic shared-knowledge UI CRUD is implemented.
- Attachment/streaming scope; timezone/calendar recurrence and missed-run policy beyond current fixed intervals; conversational editing/deletion of users' own tasks.
- Budgets, load limits, availability and recovery targets.

## 12. Public references

- [Telegram Bot API](https://core.telegram.org/bots/api)
- [OpenID Connect Core](https://openid.net/specs/openid-connect-core-1_0.html)
- [Model Context Protocol](https://modelcontextprotocol.io/)
- [OpenTelemetry for Go](https://opentelemetry.io/docs/languages/go/)
- [Go modules reference](https://go.dev/ref/mod)

## 13. Clarifications and implementation history

### Decisions

- The agent assists in multiple Telegram groups and private chats. Groups, channels and private chats have isolated context/memory; administrators maintain shared knowledge separately (section 6.1).
- Production LLM: an explicitly configured OpenAI-compatible HTTPS endpoint and router/model name.
- LiteLLM requires an API key. Configure endpoint/model/key explicitly; keys are accepted through private files/environment, redacted in diagnostics and unavailable as CLI arguments. Verify production API paths and options before integration.
- Opt-in integration testing may use an explicitly configured loopback OpenAI-compatible endpoint. It is never a production fallback.
- In private chats, all incoming messages allowed by access policy trigger a response. In groups, only mentions of the receiving bot or replies to its own messages do. Replies to other participants do not trigger responses alone. Replies never expand access scope.
- Save all messages actually available to the bot for context, not just response triggers. Telegram API/privacy settings limit availability; the application records its own outgoing messages.
- Basic access is open by default. Administrators see users/chats, assign permissions/access levels and block chats in the Web UI; detailed levels remain pending.
- Unsolicited messages are allowed only when executing scheduled tasks; arbitrary proactive messaging is outside scope.
- All permitted users can request scheduled tasks. Administrators see every task/prompt and manage them through CRUD. PoC supports one-shot/fixed intervals, UTC storage and one overdue occurrence after downtime; calendar/timezone recurrence remains open.
- Background LLM compaction summarizes messages over a configured period independently for each chat (section 6.2).
- Detailed permissions, production OIDC/transport choices, retention/compaction settings and specific tools require further decisions.

### First increment (historical; use the status summary for current progress)

- Go module, thin `cmd/tgaia`, `urfave/cli/v3` CLI.
- `config-check`/`db-check`; strict YAML/environment/CLI with tested precedence.
- altsrc nested-key lookup provides CLI value sources after strict parsing into one file snapshot. Its standard URI reader suppresses read/decode errors, which conflicts with fail-fast requirements.
- DSNs accepted through file/environment only; structural secret redaction and safe errors.
- GORM PostgreSQL via `gorm.io/driver/postgres`, SQLite via `github.com/glebarez/sqlite`; versions pinned in go.mod/go.sum.
- At this stage, migrations, Telegram, LLM and long-running `Run(ctx)` were not implemented. Commands exited after validation and accepted SIGINT/SIGTERM cancellation.
- A shared storage contract was prepared; PostgreSQL required a separate test server.

### Telegram ingestion notes

To receive ordinary group messages, disable bot privacy mode or grant group administrator status; Telegram still limits available updates. Preserve messages actually received, without promising access to past history. Sources: [Bots FAQ](https://core.telegram.org/bots/faq), [Bot API](https://core.telegram.org/bots/api).

Long polling (`getUpdates`) and webhooks are mutually exclusive for one bot. Kubernetes supports multiple webhook ingress replicas; polling requires one leased receiver per bot. Neither choice limits worker or Web UI replica counts.

## 14. Kubernetes and the task queue

- Production uses Kubernetes and shared PostgreSQL; SQLite is local-only. Stateless pods must not depend on local disk for memory, queues or schedules.
- Separate ingestion/UI, workers and scheduler producers logically. They may share a binary, but must not assume a single application process.
- Atomically persist accepted updates with enqueued work before acknowledging Telegram. At this historical increment, Enqueue saved only a job; inbox+enqueue was implemented later.
- Queue states: `pending`, `leased`, `retry`, `completed`, `dead`. Claims are atomic; PostgreSQL uses `FOR UPDATE SKIP LOCKED`. Heartbeats renew leases; expired jobs recover; attempts/backoff are bounded.
- Deduplicate events within agent/bot/chat scope. Schedule keys include task ID, schedule version and occurrence, preventing duplicate work across scheduler replicas. Compaction uses analogous range/version keys.
- Each claim gets a fresh token. Complete/retry/extend require that token and an unexpired lease. DB time, not pod clocks, determines ownership.
- Processing is at-least-once. Lease fencing protects queue state, not exactly-once external sends. Handlers honor cancellation, permissions and blocks; domain writes also check ownership/version.
- Serialize all jobs/retries in one chat; allow other chats concurrently. Keep future schedules separate until due.
- SIGTERM stops claims and cancels work; workers join owned goroutines within the shutdown deadline. Other replicas reclaim unfinished leases. Kubernetes grace periods must cover shutdown and resource closure.
- Run migrations explicitly as a Kubernetes Job before rollout, never in each worker's startup. Deployment requires schema/readiness checks, DB/LLM concurrency limits and queue depth/age monitoring.

Historical increment: the DB queue, migration and worker library were implemented/tested; runtime handlers, inbox, producers and manifests followed later. See [queue.md](queue.md) for current guarantees and the status summary for remaining PostgreSQL verification.

## 15. Outbound bot message registry

- Store every known sent message reference as `(agent_id, bot_id, telegram_chat_id, telegram_message_id)`. Message ID alone is insufficient; a public URL is optional and cannot replace API identifiers.
- Cover replies, scheduled/service messages and every part of long replies/media groups when supported. The shared sending boundary records returned IDs before successful job completion.
- Record the scoped originating `delivery_key`, part number and Telegram timestamp. One delivery/part can have multiple known IDs after uncertain repeated sends; do not hide known duplicates with an overly restrictive delivery-key constraint.
- Recording the same reference is idempotent; inconsistent original metadata conflicts. Late recording retries must not erase edit/delete markers.
- Support scoped lookup and paginated listing by chat/delivery. Reject unscoped queries. Model-provided IDs do not prove ownership; verify registry membership and permissions before Telegram mutations.
- Edit/delete are separate jobs in the same chat queue. Recheck permissions, blocks and current state before the API call; record confirmed edit/delete timestamps using optimistic versions. DB CAS alone does not resolve external uncertainty.
- Deletion retains a tombstone with `deleted_at`. Compaction never removes the registry. Edit/delete updates history, invalidates summaries and increments memory_revision in the PoC.
- Telegram message-type, age and permission limits still apply. Stored IDs cannot guarantee every future operation is allowed: [editMessageText](https://core.telegram.org/bots/api#editmessagetext), [deleteMessage](https://core.telegram.org/bots/api#deletemessage).
- Sending and DB recording cannot be atomic. A crash after sending but before saving an ID leaves uncertainty; absence of a record is not proof no message was sent. Unknown IDs cannot be recovered by assumption. Durable send intents mark unknown results uncertain without automatic resend.

Migration `002_bot_messages.sql` and scoped `BotMessages` methods `RecordSent`, `Get`, `List`, `ConfirmEdited`, `ConfirmDeleted` are implemented. Telegram send/edit/delete handlers are wired; live edit/delete still need verification. Tests cover isolation, retries, version conflicts, multipart/duplicate records, tombstones and schema upgrades preserving queued work.

## 16. Multiple bots and global user blocks

- `agents` stores agent identity; `telegram_bots` associates multiple bot user IDs with one agent. A real bot belongs to only one agent. Store `credential_ref`, not a token, in the registry; resolve tokens from private config/encrypted overrides and verify identity with getMe.
- Scope all local state—history, summaries, facts, embeddings, caches, external LLM conversation IDs, jobs, schedules, send intents and message references—by `(agent_id, bot_id, chat_id)`. Never reuse one bot's context in another. Transport inbox deduplication uses `(bot_id, update_id)`.
- Only explicit administrator-published knowledge is agent-wide; never populate it automatically from bot memory.
- Group bots respond to their own mentions/replies only; a reply to another bot is not a trigger. Private-chat responses belong to the receiving bot.
- `blocked_users` uses `(agent_id, user_id)` without bot/chat keys. Blocks apply across that agent's bots, groups, channels and private chats; different agents retain independent policy. Use stable Telegram IDs, not usernames.
- Reject new jobs from blocked users. Retire outstanding jobs with their `actor_user_id` to dead in bounded batches; do not renew or successfully settle their active leases. Unblocking does not automatically replay suppressed jobs.
- Derive `actor_user_id` from verified Telegram events or the schedule author, never prompts/tool arguments. Zero is reserved for trusted system tasks such as compaction, not anonymous users. Decide anonymous-author policy before supporting those events.
- Telegram/LLM/tool handlers recheck blocks before external effects, including scheduled sends. Blocks cannot retract requests already accepted by external services. Ingress, scheduler and response handlers perform policy checks.
- Blocking does not delete existing memory. Acknowledge new blocked-author messages through the inbox without adding them to history/LLM context; retain seen_users records for administration.

Migration `003_bots_and_access.sql` adds bot ownership, global user blocks and expanded composite keys. Legacy rows with unknown bots retain technical `bot_id=0`; ordinary APIs/workers cannot access them. Never assign them automatically to the first bot. Explicit administrative mapping is required but not yet available in CLI/UI.

This migration rebuilds tables and changes key contracts. Stop old consumers before applying it; mixed old/new code rollout is unsupported. Tests cover independent bot processing/deduplication, scoped message IDs, global blocks, lease revocation and legacy preservation.

## 17. System prompts, settings and the local PoC

- Each `bot_id` has a DB system prompt editable in the Web UI. Prompts cannot disable server-side access/scope/action/schema rules.
- Runtime settings support DB overrides except database connection and OIDC, the agreed bootstrap exceptions. DB overrides take precedence over file/environment/CLI values.
- The UI shows effective values, override presence and reset to bootstrap/default. It never returns secrets; separate inputs replace them. Typed validation and optimistic versions protect writes.
- Runtime overrides apply without restart using snapshots and client/listener/consumer updates (section 19). Bot prompt changes affect subsequent processing.
- Local development uses explicit mock OIDC. Production accepts a configured issuer and bootstrap administrator groups; any authenticated account is not automatically an administrator.
- Mock OIDC/Telegram/LLM require loopback bind/public URLs and never activate as fallbacks. Live credentials belong only in ignored configuration or a secret manager.
- Polling mode uses per-bot leases and advances offsets only after durable acceptance; Kubernetes deployments may instead use webhooks with the scalable queue.
- Keep requirements and implementation status in this document so future sessions can continue without reconstructing decisions from chat.

## 18. MCP tools

### Requirements and decisions

- Use official `github.com/modelcontextprotocol/go-sdk` v1.8.0. The project and Dockerfile pin Go **1.27.3**.
- Add/edit servers through YAML or **MCP tools** in the UI: CRUD, Discover tools (discovery only, no calls) and Reset to configuration file.
- YAML keys: `app.mcp.servers.<alias>` with enabled, transport (`http`/`sse`/`stdio`), url, token, command, args, env, bots, tools. args/bots/tools are JSON-array strings and env is a JSON-object string, following strict string-leaf configuration.
- The versioned DB override `mcp.servers` replaces the entire file list. AES-GCM encrypts definitions/credentials; CAS prevents lost edits. GET returns public fields and secret-presence flags only. Blank inputs preserve secrets; explicit clear/reset removes them.
- DB watchers apply MCP changes automatically across API/workers. Saving alone does not start a server; explicit discovery connects to or launches the saved server.
- Only administrators set bot/tool allowlists. Bots are configuration aliases. Empty lists deny access; `*` explicitly grants all. Tool annotations are not permissions. Allowed tools may mutate external systems; the UI states this, and interactive approval is not implemented.
- MCP logic is provider-independent. The shared OpenAI Chat Completions adapter exchanges tools/tool_calls/tool messages, then requests a separate structured reply/schedule result.
- Environment-dependent values have **no defaults**: LLM endpoint/model, agent_id, listen/public_url, role and OIDC client_id are explicit. Concrete environments belong in profiles/documentation. OIDC issuer/credentials and DB connection remain bootstrap settings.

### Implemented

- [x] SDK Streamable HTTP/SSE/stdio adapters, discovery and JSON Schema validation without remote references. Stdio uses absolute executables without a shell, does not inherit agent environment and does not log stderr.
- [x] One MCP session per job, never shared between bots/chats. The external server must isolate its own data; separate sessions do not partition shared storage under common credentials.
- [x] Every call checks bot/tool allowlists, arguments, user/chat blocks and lease ownership before intent persistence. The model cannot choose server URLs, credentials, registry entries or access rights.
- [x] Limits: 16 servers, 64 exposed tools, 4 rounds/8 calls, 75s tool phase, 20s call timeout and 16KB results. Text/structured results are untrusted; binary resources are not fetched.
- [x] Migration 009 `tool_steps(job_id,step,request,state,result)` persists native model turns and call intents. Completed steps are reused; interrupted started calls become uncertain and are not repeated. Unknown outcomes stop subsequent actions; exactly-once external effects are not promised.
- [x] Scope comes only from the job; queue FKs link records. Tool outputs never become shared knowledge or compaction instructions. Compaction has no tools.
- [x] Real SDK HTTP/SSE/stdio fixtures, schema rejection, allowlist/environment isolation, CRUD/CAS/redaction and retry/uncertainty tests.
- [x] An opt-in provider integration test calls a fixture MCP tool and verifies the persisted response without contacting Telegram.

### Technical findings and continuation

- SSE uses the Connect context for the long-lived stream; do not cancel it immediately after initialization. Initialization timeout is separate from session lifetime, owned by the job.
- Some OpenAI-compatible endpoints require `properties: {}` even for empty object schemas. The adapter normalizes missing properties generically without provider-specific execution paths.
- User MCP endpoints and production LiteLLM/router remain untested; production credentials were not used. Interactive MCP OAuth, resources/prompts, elicitation/sampling and granular per-user tool grants remain unimplemented.

Full tests, vet, CGO-free build and MCP/service/storage/webui/llm race checks passed on Go 1.27.3. Browser checks used isolated mock services and synthetic data.

The scheduler prompt regression discovered during this work was fixed later (section 20).

## 19. Applying DB settings without restart

All runtime DB overrides, whether saved through the UI or another replica, must apply during execution. Database connection and OIDC remain bootstrap-only. YAML changes still require restart; Reset removes the override and restores the current process's bootstrap snapshot.

- [x] Each replica reads runtime_settings every 250ms and compares a digest of all values, including secrets (never exposed). This detects deletion/reset/recreation even with reused versions. No new tables are required.
- [x] Validate and prepare the whole configuration before publishing an immutable snapshot. New jobs use the new LLM endpoint/model/key and MCP registry; in-flight jobs retain their LLM snapshot. Reuse unchanged LLM clients, OIDC authentication and mock senders.
- [x] MCP rechecks current authorization before the next call. Removing/disabling/replacing a server or bot alias invalidates old sessions; already-issued external calls cannot be undone.
- [x] Refresh log level, compaction timing/window, HTTP handler/webhook credentials, callback URL, API-scoped application listener and pollers. Bind the new application listener before draining the old one for up to 10s. Process role, observability listener and optional dedicated Telegram webhook listener remain bootstrap-only; worker-option changes replace the consumer group and unfinished jobs recover through the queue. Pollers release leases using owner tokens and preserve offsets.
- [x] Invalid config or bind failure preserves the last working snapshot. OIDC-protected `/api/runtime` reports applied/applying/error and application time on the serving replica; UI polls every second. Failed application does not roll back the stored override: correct or Reset it. Retry application at most once per second.
- [x] Prompts, blocks, tasks and shared knowledge continue to be read by their operations without restart.

Convergence is usually about 250ms plus client preparation. getMe/OIDC initialization can take up to 15s; stopping consumers adds time. Replicas converge eventually, without a global atomic barrier. UI status describes its serving replica. Failures preserve working configuration. Mock/live database-family and bot ownership restrictions remain enforced. Since migration 016, the application listener/public URL are deployment-local API settings; role and observability listener are bootstrap-only. Shared overrides cannot change them.

Tests used two API replicas on shared SQLite, setting save/reset/recreate, occupied-port failure preserving HTTP service and recovery after Reset. Separate LLM endpoints verified model/key changes without mixed snapshots. Scope tests cover the API application listener, the bootstrap observability listener and combined-role API transport. HTTP/SSE tests verified revocation before the next call; stale polling-owner release was rejected. Production PostgreSQL/Kubernetes rollout remains unverified.

Full tests, vet, CGO-free build and app/mcpclient/webui/storage race tests passed. API listener/public_url changes and worker-option reloads were tested. A temporary memory interval override and Reset applied without restarting; the test override was removed afterward.

## 20. Executing reminders when due

Symptom: a request to drink water in five minutes received an immediate promise, then repeated that promise when due. The queue delivered on time; generic dialogue instructions plus the earlier promise and a weak scheduled-task hint caused incorrect execution context. Quoted example responses below are English translations of the Russian test conversation.

- [x] Scheduled jobs receive explicit execution instructions: the time has arrived; perform the task now and return the reminder itself. Existing prompts with relative times work without migration.
- [x] New task creation requests an execution-time action prompt, with delay/interval stored separately.
- [x] Scheduled responses use a separate JSON Schema requiring `schedule: null`. Backend checks also prevent scheduling from scheduled jobs, including cached results. Recurrence belongs to the scheduler.
- [x] Regression tests cover request → confirmation/task → Tick → execution, including history, execution instructions/schema, no new task and no duplicate delivery on retry.
- [x] Opt-in `TestLiveScheduledReminder` uses explicit provider settings, isolated SQLite and a fake Telegram sender to verify execution-time reminder output without contacting Telegram.

Service/config/storage tests, service vet and CGO-free build passed on Go 1.27.3.

### End-to-end verification

- [x] Isolated integration verification covered request acceptance, durable scheduling, due execution and delivery without retaining private data in the repository.
- Timing limitation: task/compaction Tick shares memory.interval (30s locally), adding up to that interval plus queue/LLM/Telegram latency. A separate scheduler interval remains pending.

## 21. Protecting Web UI availability

- [x] Hide listen/public_url/role in the settings editor and GET `/api/settings`; these can disable HTTP or break OIDC callbacks. POST/DELETE return 403 for these keys, including Reset, regardless of existing overrides.
- [x] Operators manage listeners/public URL through bootstrap configuration or direct scoped DB administration. Role, DB and OIDC remain bootstrap-only. Migration 016 deliberately removes unsafe legacy unscoped role/listener/public URL overrides.
- [x] Tests verify hiding, denied writes/reset, role-scope isolation and scoped runtime application. Other runtime/MCP settings remain editable.

## 22. Typing indicator

- [x] Telegram client supports sendChatAction with action typing and the current job's chat_id.
- [x] Response generation, including MCP/scheduled execution, starts a best-effort loop immediately and every 4s. Each call has a 2s timeout and rechecks user/chat blocks. Failure stops the indicator, not response processing.
- [x] Cancel and join the loop before delivery so typing cannot appear after the answer. Job cancellation/generation errors also stop it. Cached retries, compaction and edit/delete do not send typing; non-triggering messages do not start it.
- [x] Tests cover API payloads, immediate/periodic notifications, joined cancellation, errors, Respond integration, cached retries and service/telegram races.

Telegram hides typing after a message or expiry; sendChatAction has no explicit cancel action. On errors without a reply, the indicator expires on Telegram's timeout. Reference: [sendChatAction](https://core.telegram.org/bots/api#sendchataction).

Service/telegram tests, race, vet and CGO-free build passed. Visual typing behavior remains primarily covered by automated transport and lifecycle tests.

## 23. Default system prompt from Markdown

- [x] Use `system-prompt.md`, as requested, beside `--config`. Without a config path, resolve from the startup working directory. No environment-specific path is built in.
- [x] Repository template contains a base personal-assistant prompt. Copy or customize it beside ignored configuration and mount it beside config on every Kubernetes replica.
- [x] Nonempty per-bot DB prompts take precedence. Empty/whitespace prompts use the file; do not copy fallback text into bot rows, so edits apply automatically.
- [x] Read the file before each new generation without restart; cached results remain cached. Missing/unreadable/empty/files over 16000 bytes fail generation clearly. Custom DB prompts do not depend on the file.
- [x] Persona supplements mandatory application protocol/access instructions; scheduled execution instructions still take precedence.
- [x] Tests cover whitespace fallback, edits between jobs, DB priority with no file, size limits and missing/empty files.

Full tests, config/service/app vet and CGO-free build passed on the Go version declared in `go.mod`. Manual startup verified polling, Markdown fallback and typing support without retaining private runtime artifacts in the repository.

Superseded (2026-11-07): there is no embedded protocol prompt any more. The per-bot DB
prompt, or `config/system-prompt.md` when no DB override exists, is the bot's only prompt
and is used unchanged for replies, scheduled runs, compaction and image description.
Situational behaviour is expressed as data (the `Event` JSON, offered tools) and as
loop mechanics; schema, permission and scope checks are still enforced by code. Model-
specific adjustments come from behaviour corrections the bot proposes during compaction
and an operator approves (or auto-approves per chat).

## 24. Build and test Makefile

- [x] `make build`: CGO-free `bin/tgaia` with `-trimpath`.
- [x] `make test`: all tests; `make test-race`: all tests with race detector/CGO; `make vet`: static analysis; `make check`: tests plus vet; default/help lists targets.
- [x] Override GO, BINARY and TEST_ARGS as needed. Standard Go toolchain selection follows go.mod. No local cache/config paths, endpoints or credentials are embedded; live LLM tests remain opt-in.

`make build check test-race` passed and created `bin/tgaia`.

## 25. Docker and Kubernetes deployment

- [x] Two-stage Dockerfile: Go 1.27.3/BuildKit caches/TARGETOS+TARGETARCH → Alpine with CA certificates and a CGO-free, trimpath, stripped binary. UID/GID 10001; direct SIGTERM handling without a shell entrypoint.
- [x] Neither config.yaml nor system-prompt.md is embedded; both are externally mounted. The prompt is excluded from build context. No production endpoints/secrets are baked in.
- [x] Separate plain migration.yaml and workloads.yaml manifests (two API and two worker replicas plus ClusterIP Service), optional ingress and a documented production configuration. Read-only projected Secret/config.yaml plus ConfigMap/system-prompt.md without subPath.
- [x] Non-root pods, fsGroup 10001, RuntimeDefault seccomp, drop ALL, read-only root filesystem, emptyDir /tmp, no automounted service-account token, API probes/resources/graceful shutdown. Worker health endpoint is still absent and documented.
- [x] [Deployment guide](../examples/kubernetes/README.md): build/push, Docker mounts, PostgreSQL/TLS, OIDC, persistent secret key, namespace/Secret/ConfigMap, migration-first deployment, ingress, polling/webhooks, MCP stdio dependencies, independent scaling, upgrades/rollback and troubleshooting.
- [x] Container smoke tests verified the CLI, non-root UID, absence of embedded configuration, PostgreSQL migrations, mounted mock configuration, probes, read-only operation and graceful termination.
- [x] Plain Kubernetes manifest dry-run and YAML structure/security/mount checks passed.
- [ ] Real cluster deployment, production OIDC/LiteLLM, multiarch manifest push and PostgreSQL failover/load remain unverified. Docker smoke covers startup/migrations, not full queue HA.

## 26. Specification review and remaining scope

The status summary and remaining scope were reconciled with the implementation and recorded checks.

Sections 13–18 include historical states; when they differ, the status summary and later increments take precedence. Distinguish mandatory unfinished implementation (permissions, observability, configurable operating limits) from implemented but unverified production integrations and unresolved product scope. Retention periods, calendar/timezone rules, streaming, embeddings/semantic search and conversational task editing/deletion need scope decisions. MCP OAuth, resources/prompts, elicitation/sampling and interactive tool approvals are possible extensions, not automatically agreed first-PoC requirements.

## 27. Documentation language

Project documentation is maintained in English. Runtime prompts, test fixtures and private configuration remain separate from documentation; translating the docs does not change runtime behavior.

## 28. Sequential implementation plan and admin improvements

Implementation order:

1. [x] Administrative pagination and manual uncertain delivery/tool resolution.
2. [x] Telegram attachments/images, edited updates, forum topics, channels and anonymous authors. Audio/video recognition is disabled until a suitable model is configured. All model capabilities go through LiteLLM. Support bounded text attachments and images without a separate provider path.
3. [x] Access matrix: reader (replies only; no tools or task creation) and user (replies, own tasks and allowed MCP tools). User combines the originally proposed user/operator levels. Administrators assign levels through the Web UI; OIDC authenticates administrators rather than replacing the UI. Global user/chat blocks take precedence.
4. [x] Metrics, OpenTelemetry, worker health and administrative change audit (scope and limitations in section 31).
5. [x] Configurable worker concurrency, timeouts, LLM/compaction budgets and quotas.
6. [x] Shared versus API/worker-specific DB setting scopes.

### Item 1 — implemented

Migration 010 adds outcome versions and an append-only resolution record (job/target, administrator identity, reason, action and DB timestamp). Run explicit db-migrate before deploying this version.

OIDC/CSRF-protected list APIs provide bounded keyset pagination for bots, chats, users, tasks, deliveries, tool calls and full scoped message history, including compacted source messages. Cursors bind list kind and history scope; page size is 1–100, UI default 50. First/previous/next controls preserve each list's cursor. Lists are live views, not snapshot transactions; changing a task's run time can move it between pages.

The UI provides explicit confirmation or dismissal without blindly retrying external actions. Delivery confirmation requires a verified Telegram message ID/send time and records the message registry/history. Dismissal does not claim success. Tool confirmation accepts a verified JSON result; dismissal retains uncertainty and prevents subsequent tool effects in that run. Resolutions require a reason, expected version and no live lease in the conversation; stale or repeated resolutions conflict. Scope/job row locks protect resolution against PostgreSQL claims. Stale send receipts are now fenced. Remaining multipart replies may resume using cached model output and resolved parts are skipped. Tool resolution never regenerates an already cached final reply. Re-executing an uncertain action is deliberately not offered.

Verification: full Go tests, go vet, CGO-free build and storage/service/webui race tests passed. Added pagination across 125 rows, cursor-scope rejection, active/stale/repeated resolution conflicts, audit/receipt persistence, stale worker rejection and multipart continuation without model/send replay. PostgreSQL variants remain opt-in through `TEST_POSTGRES_DSN`. Browser verification covered pagination and manual dismissal using isolated mock services and synthetic data.

### Additional requested work

- [x] GitHub Actions `.github/workflows/ci.yaml`: build/check/race; PR/non-default branch container build and smoke without registry writes; default branch and v* tags publish linux/amd64+linux/arm64 to ghcr.io/<lowercase-owner/repo> using GITHUB_TOKEN. Full-SHA, default-branch latest and semantic version tags; OCI metadata, GHA caches, minimal provenance and SBOM. Actions pinned to verified official release commit hashes. README documents usage and permissions.

## 29. Telegram extensions and trusted runtime identity

Item 2 is implemented with audio/video recognition explicitly disabled as requested. Migration 011 adds topic IDs, sender-chat identities, message update revisions, attachment metadata/extracted text and topic-keyed summaries. Legacy records remain in topic 0. Apply migrations with old consumers stopped before deploying; old code cannot safely read the new topic-keyed memory layout.

- Forum memory/history, bot message references, schedules, compaction and reply destinations include message_thread_id. Queue serialization remains conservative across the entire parent chat. Parent-chat blocks cover all topics. The admin UI lists topics and passes topic scope for history/tasks/message actions.
- Receive edited_message/edited_channel_post as well as ordinary/channel messages. Edits update existing records only when newer, preserve message identity, clear derived media text and invalidate covered summaries. Stale response jobs stop before delivery; compaction checks memory revision before committing. Already accepted external calls cannot be undone by a later edit.
- Channel/anonymous sender_chat is a separate principal, never Telegram's fake compatibility user. Hidden authors are reader-only: respond only on mention/reply to this bot, with no MCP or task creation. Own channel replies can be recognized through the outbound ID registry. A hidden person's user ID cannot be inferred or checked against personal blocks; chat blocks remain applicable.
- Photos/image documents download through getFile with bounded size and path/origin checks. No token-bearing file URL enters the model or logs. Vision uses the configured OpenAI-compatible LiteLLM model; a no-tool description call turns the image into persisted untrusted reference text for replies/memory. UTF-8 text documents are supported with bounded extraction. Other binary documents are identified as unsupported. Limits currently include 8MiB images, 256KiB text downloads and four recent attachment-bearing messages per response; item 5 will make operational limits configurable.
- Audio/video are recorded as unsupported attachments and never transcribed or sent to recognition models. The agent asks for text/images instead. It never pretends to have inspected unavailable content. Passive media beyond the inspected response window remains metadata until processed; raw files are not retained in the database.
- Each bot receives a trusted runtime block with its getMe display name, username, bot ID and fresh RFC3339 UTC time. Mock names come from configured bot aliases. Names are refreshed when real clients are initialized; cached job results remain immutable. The block is supplied to response, vision and compaction generation.

Validation: full Go tests, vet and storage/service/telegram/llm race checks passed. Added tests for topic isolation and scheduled destinations, edit ordering/invalidation/stale summary rejection, image enrichment and persisted reuse, current UTC/name/username, anonymous schedule denial, download path restrictions/audio disabling and topic send payloads. External media/forum/channel integration and PostgreSQL variants remain unverified.

Next: item 3 access matrix, then isolated integration tests of rights, global user block/unblock and chat block/unblock, including reply/scheduled delivery. Suppressed tasks must not replay on unblock.

### Edited source messages: replace the existing reply

Migration 012 adds scoped source-message-to-reply-part links and backfills known sent responses. When an edited source already has a delivered reply, regenerate its text and edit the existing Telegram message IDs. Remove surplus parts when the new reply is shorter; send and link only missing parts when it grows. Keep bot-message history synchronized and invalidate affected summaries. If no delivered reply exists, the normal response path applies. Newer source revisions supersede stale response jobs. Edited replies do not repeat MCP effects or create additional schedules; previously completed external actions are not undone.

Ordinary Telegram Bot API chats do not emit user-message deletion updates. Therefore automatic deletion of replies when users delete their source messages is not implemented or promised. The existing administrator message deletion action remains available. `deleted_business_messages` is a separate business-bot feature, outside the current integration. Reference: https://core.telegram.org/bots/api#update.

Validation: regression coverage verifies reuse of reply IDs, multipart shrink/growth, persisted active links and bot isolation. Deploy only after explicit migration with old consumers stopped.

Isolated verification with synthetic Telegram messages confirmed that editing a source request updates the existing bot reply rather than sending a duplicate. Multipart behavior remains covered by automated tests. Ordinary source deletion remains unsupported by the Bot API.


## 30. Access matrix and revocation checks

Migration 013 adds versioned reader/user roles to seen users. Default is user. Roles apply across bots/chats of the same agent; bot memory remains isolated. OIDC authenticates administrators; the People & access UI assigns roles with optimistic version checks. Unsaved selections survive automatic refresh. Hidden authors remain reader-only regardless of role lookup.

Reader can receive answers but cannot create active schedules or use MCP tools. User can receive replies, create own schedules and call explicitly allowed per-bot MCP tools. The backend checks permissions before tool planning/intent/calls and scheduled task creation/execution, including cached response paths. Prompt instructions are not authorization. Task creation serializes with role/chat policy changes on PostgreSQL. Revocation cannot retract an external operation already in flight.

Blocking users/chats or downgrading to reader pauses active schedules and retires queued/retrying/leased work in the affected scope. Restoring access permits new requests but never revives suppressed work or paused schedules. Global personal blocks override roles; chat blocks cover all topics. Message mutation jobs now validate their lease before action.

Validation: full Go suite and storage/service race checks passed. Tests cover reader reply/tool/task denial, stale role writes, promotion without restart, lease revocation and no replay after user/chat unblock or promotion. Isolated verification with synthetic policies covered scheduling denial, user/chat block and unblock, role restoration, task creation and scheduled delivery. MCP permission behavior remains covered by automated fixtures.

Next: item 4 observability and transactional audit, then items 5 and 6. OpenTelemetry dependencies have been fetched but instrumentation is not yet implemented.


## 31. Observability and administrative audit

Item 4 implemented. `/metrics` exposes bounded-label job count/error/duration/inflight metrics, worker-running state and database queue counts. Every role serves `/healthz`, `/readyz` and `/metrics` on a dedicated observability listener; these routes are absent from the application listener. API/all roles may additionally move `/telegram/{bot}` to a bootstrap-configured dedicated webhook listener containing no other routes. Worker replicas open only the observability listener, with no admin/webhook listener. Readiness requires database/schema access and a running worker group. Kubernetes probes use the internal ops port.

Official OpenTelemetry Go SDK 1.47.0 exports OTLP/HTTP when `observability.otlp_endpoint` is explicitly set; empty disables export. It hot-reloads without restarting consumers. Job spans include nested LLM/MCP calls; root sampling 10%, batch export bounded with 5s export/shutdown timeout. Sensitive content is not attached to spans. Trace context currently begins at the worker job, rather than propagating from the Telegram webhook across the persistent queue.

Migration 014 adds append-only-by-API audit records and keyset-paginated Web UI. All administrative persisted mutations (roles, blocks, tasks, settings/MCP definitions, prompts, knowledge, message-action enqueue, outcome resolution and demo events) use a shared transaction with audit append. Failed authenticated mutations roll back and retain a separate rejected-attempt record. Targeted entity snapshots record before/after for role/block/task/settings/prompt/knowledge/resolution mutations. Enqueued actions record the request; later external outcomes remain in job/message/tool tables. Credential-bearing values and MCP environment/arguments/URLs are redacted. Probe/discovery is read-only and not included; unauthenticated/CSRF-rejected requests are rejected before this audit. Database-administrator edits and filesystem configuration changes require infrastructure audit, not this application log.

Validation: full suite passed; metrics/span content, audit rollback and credential redaction tests were added. An isolated schema 014 upgrade verified `/readyz`, `/metrics`, and an administrative save with before/after state in Audit. No external collector is required by default; exporter transport uses the official SDK and span generation is tested in memory.


## 32. Configurable limits and shared quotas

Item 5 implemented through validated `app.limits` / `limits.*` settings, documented with defaults in README. Configurable worker concurrency/poll/lease/heartbeat/task/shutdown/retry durations, LLM concurrency/request timeout/input/output budgets, reply and compaction message/byte budgets, compaction output tokens, tool phase/call timeouts and round/call/context/result budgets, Telegram timeout and media limits. These are operational defaults, not environment-dependent endpoints/models. Worker option changes replace the consumer group gracefully; other in-flight work retains its configuration snapshot. New jobs use updated limits.

Migration 015 persists atomic fixed-UTC-hour quota counters per agent/bot/parent chat/category, shared by replicas/topics. Response/tool-planning/vision attempts and compaction attempts have separate optional call quotas. Active task quotas apply across bots/chats for each user and are enforced on creation/reactivation. Zero disables a quota. Calls are reserved immediately before invoking a provider after checking current access and lease; cached results do not spend additional calls. Errors/timeouts still consume reservations. Quota exhaustion blocks provider calls and follows normal failed-job retry handling; no automatic next-hour replay is promised. Protocol/storage safety ceilings remain non-configurable.

Verification: full Go suite passed; added concurrent two-connection quota enforcement, UTC bucket rollover, topic sharing/category isolation, invalid limits/cross-field checks and a provider-call-count regression proving denied calls never reach the model. PostgreSQL test variants remain opt-in.

## 34. OIDC group-based administrator authorization

Replace the current `admin_subjects` subject allowlist with an OIDC group membership check.

### Requirements

- Remove `AdminSubjects []string` and `Subjects []string` from `webauth.Config`/`webauth.Auth`.
- Add `AdminGroups []string` and the corresponding `admin_groups` YAML key / `TGAIA_ADMIN_GROUPS` env var.
- After OIDC callback, extract the `groups` claim from the ID token (Keycloak includes it as a JSON array of strings when the client has the groups mapper configured).
- Access is granted if the token's `groups` claim contains at least one entry from `AdminGroups`.
- Session cookie stores the matched group or the first group for audit; `Session()` returns it in place of the subject for the audit log. The subject (`sub`) is still available for session validation.
- Request the `groups` scope, or rely on the identity provider's configured claim mapper.
- Validation: `admin_groups` required when `oidc.mode = oidc`, same as `admin_subjects` today.
- Update `devmode.OIDC` mock to include a `groups` claim in the issued ID token; update `Config.Subjects` usages in tests.
- Update `config/config.example.yaml`, README and this design doc.

### Deployment confirmation still required

Confirm which claim name (`groups`, `realm_access.roles`, or a custom mapper name) and group/role values the production identity provider emits. The application and provider mapper must use the same contract.

### Status

Implemented using the `groups` claim and requested `groups` scope. Configuration, mock OIDC, callback authorization, signed sessions, UI/audit identity and tests use administrator groups. The provider-specific mapper and exact group value still require operator confirmation before production rollout.

## 35. Configurable default user role (fail-closed access)

Currently, users not present in `seen_users` are treated as `user` by default (fail-open). This means any Telegram user who can reach the bot gets full access to tools and task creation. The `reader` role restricts to reply-only without tools or tasks.

### Requirements

- Add a `default_role` configuration setting (`app.access.default_role` in YAML, `TGAIA_DEFAULT_ROLE` env var), accepting `reader` or `user`. Default for backward compatibility: `user`.
- In `CheckActions` and the tool-assembly path, treat unknown users (no `seen_users` row) as `default_role` rather than hardcoded `user`.
- Production deployments exposing MCP tools (especially sandbox) must set `default_role: reader` and explicitly promote trusted users via the Admin UI.
- Update README, `config/config.example.yaml` and this design doc.

### Status

Pending.

## 33. Shared and replica-specific settings scopes

Item 6 is implemented by migration 016. Runtime settings use a composite `(scope,name)` key with precedence `bootstrap configuration -> shared DB override -> api/worker DB override`. Existing unscoped runtime values migrate to `shared`, except `role`, `listen` and `public_url`: their old global meaning is unsafe for split deployments, so migration 016 removes them. `role`, `observability.listen` and `telegram.webhook_listen` are bootstrap-only. Operators may place `listen` and `public_url` in `api`; these connection settings remain excluded from the Web UI to prevent lockout. Worker replicas do not use `listen` because they have no application HTTP listener.

Shared identity, credentials and policy values continue to apply to every replica. API scope contains its application listener/public URL, Telegram request timeout and optional log/OTLP settings. Worker scope contains LLM/MCP/memory/worker/media limits and optional log/OTLP settings. The observability listener is deployment-local bootstrap configuration and is not stored in DB settings. Shared policy quotas, including active tasks per user, cannot be weakened by a worker-local override. An `all` process resolves worker settings for execution and API settings for its application listener, callback URL, polling transport and administration UI.

Secrets use the scope and setting name as AES-GCM associated data, preventing ciphertext from being copied between scopes. Writes retain optimistic versions, validate the fully resolved API and worker configurations and are included in the administrative audit. Tests cover precedence, reset/inheritance, scope isolation, legacy migration, role/listener protection, UI filtering, hot reload and combined-role API transport selection. Apply migrations explicitly with old consumers stopped; migration 016 intentionally requires any former DB listener/public URL overrides to be re-created in the API scope.

Isolated verification migrated schema 014 -> 016 and confirmed that authenticated API views return only settings allowed for each scope. A temporary API-scoped override applied within the watcher interval, Reset removed it, and both changes were audited. Backend responses and automated UI tests passed.

## 36. Explicit user-requested memory

Migration 017 adds explicit memories keyed by `(agent_id, bot_id, chat_id,
thread_id, source_message_id)`. The response schema has a nullable `remember` action;
the model may populate it only for an explicit current-user request. The application,
not the model, supplies the scope and actor and verifies current access before writing.
Reader-only and hidden senders cannot create memories.

When `memory.embedding_model` is configured, remembered facts and later queries are
embedded through the same OpenAI-compatible endpoint/key as the LLM. Embeddings are
normalized, stored as portable JSON and compared in the application, so SQLite and
PostgreSQL need no vector extension. Search reads at most 500 records from the current
bot/chat/topic and merges the 20 best cosine matches with 100 recent records; at most
16 KB reaches the response model, labelled as untrusted data. Model names are stored
with vectors so incompatible spaces are never compared. Query-embedding failure falls
back to recent memories; failure to embed a newly requested memory retries the job
instead of claiming it was stored as searchable.

Editing the source message deletes the stale record before a newly generated structured
response may replace it. Automated tests cover persistence, semantic ranking,
subsequent retrieval, reader denial, source-edit invalidation and bot/chat/topic
isolation. A dedicated vector index/backfill becomes useful only beyond this bounded
PoC search size.
