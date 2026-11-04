# tgaia

[![CI](https://github.com/maniack/tgaia/actions/workflows/ci.yaml/badge.svg)](https://github.com/maniack/tgaia/actions/workflows/ci.yaml)

Telegram AI assistant in Go. Architecture and security requirements live in
[docs/design.md](docs/design.md); queue guarantees are documented in
[docs/queue.md](docs/queue.md).

The current implementation includes Telegram polling/webhooks, an OpenAI-compatible LLM adapter,
DB queue/workers, scheduled prompts, isolated chat memory with background compaction,
and an OIDC-protected administration UI. Users can explicitly ask a bot to remember a
fact for later retrieval within that bot/chat only (shared by all forum topics of the chat). Private messages trigger replies; in groups,
only mentions of this bot and replies to this bot do. Other received text contributes
to the chat's context. All bots have independent history, summaries and system prompts.
Administrator-published knowledge is the explicit shared layer.

## Local run

Requires the Go version declared in `go.mod`. Copy the public templates from `config/`
to an ignored runtime directory, replace the session secret, and keep the prompt beside
the runtime configuration.

```sh
mkdir -p data
cp config/config.example.yaml data/config.yaml
cp config/system-prompt.md data/system-prompt.md
chmod 600 data/config.yaml
```

The example is a fully simulated loopback profile with mock OIDC, Telegram and LLM.
Its comments document the production alternatives. Configure explicit providers and
credentials before enabling live modes; no automatic fallback is performed.

```sh
make build
./bin/tgaia --config data/config.yaml
```

Open http://127.0.0.1:8080/. Mock OIDC logs in as `local-admin` in the
`local-admins` group; real OIDC validates issuer, signature, audience, state, nonce
and PKCE and allows only identities whose `groups` claim intersects `admin_groups`.
Health, readiness and Prometheus metrics are available only on the dedicated
loopback listener at http://127.0.0.1:9090/ (`/healthz`, `/readyz`, `/metrics`).
Stop with Ctrl-C. The default local command applies versioned migrations before
starting. Explicit `serve` never migrates and is intended for production after the
separate `db-migrate` step.

To keep demo data separate, override the database for the command:

```sh
TGAIA_DATABASE_DSN=demo.db ./bin/tgaia --config data/config.yaml
```

Mock bots require positive ID and username in the config. The database refuses switching
between mock and live Telegram. Real bot identity is resolved using getMe from its token.
Mocks require loopback bind and public URL. No automatic fallback is performed.

## Settings and storage

Bootstrap configuration uses CLI > environment > YAML > defaults. Runtime DB precedence is
bootstrap configuration, then `shared`, then the replica's `api` or `worker` scope. Overrides
apply automatically (DB polling every 250 ms).
The UI shows apply status/errors; invalid changes retain the last working configuration.
In-flight jobs retain their LLM snapshot; MCP revocations are checked before each call. Per-bot
system prompts are read anew on each job. Database connection (including timeout) and
OIDC, process `role`, `observability.listen` and `telegram.webhook_listen` stay bootstrap-only. To prevent UI lockout, `listen` and `public_url`
are excluded from the Web UI settings API/editor (including Reset); manage them through
configuration or operator-level scoped DB changes. `listen` and `public_url` belong to
the `api` scope. Worker processes do not open the application listener;
they expose only the bootstrap-configured observability listener. Migration 016 discards legacy unscoped values for these
three deployment-local keys instead of applying them to every replica. `config-check` currently validates/prints only the redacted
storage/CLI configuration; full app validation occurs during `serve`.

In Telegram webhook mode, an empty `telegram.webhook_listen` serves
`/telegram/<bot_id>` on the application listener. Set it (or
`TGAIA_TELEGRAM_WEBHOOK_LISTEN`) to a distinct address to expose only that route on a
dedicated listener. Keep routing and TLS termination in the trusted reverse proxy.

See the comments in `config/config.example.yaml` for every bootstrap app setting and default
operational limit. YAML leaf values must be strings; quote numeric values and booleans.
Unknown keys/env variables are rejected. Secrets
have no CLI flags, are never returned from the settings API, and DB secret overrides
are encrypted using a key derived from the persistent bootstrap session key. Back up
that key together with the DB; do not rotate it independently of encrypted overrides.

SQLite uses pure Go and local files. Production uses GORM/PostgreSQL. All private
memory keys include `(agent_id, bot_id, chat_id, thread_id)`. A blocked user ID is denied across
all bots/chats of its agent. The UI also supports chat blocks, task CRUD, knowledge,
settings, bot prompts and queued edit/delete actions for known bot message IDs.

Everything the bot does for itself goes through built-in tools in the tool phase:
`task_list`/`task_create`/`task_update`/`task_delete` for scheduled and recurring
tasks and `memory_list`/`memory_save`/`memory_delete` for explicit facts. The
final model call can only write the reply text. `task_create` takes the user's words
(weekday or date, wall-clock time, IANA timezone, or a relative delay); the code
computes the first run, returns it to the model and appends the same computed time to
the delivered reply, so the user never sees a time the model worked out itself. The
tool phase is structural: every turn is a function call (`tool_choice: required`) and
ends with `phase_finish(note ≤ 300 chars)`, so the answer is generated once.
Explicit requests such as “remember that …” are persisted with the application's
current conversation scope; the model cannot select another bot or chat. Set `app.memory.embedding_model: bge-m3` (or
`TGAIA_MEMORY_EMBEDDING_MODEL`) to use the configured OpenAI-compatible LLM endpoint
and key for embeddings. Later requests use cosine similarity over only the current
bot/chat (all of its topics), merge the top semantic matches with recent memories, and pass at most
16 KB to the response model. No pgvector extension is required. Reader-only and
anonymous senders cannot add memories; a user can forget only their own facts. Changing the embedding model does not mix vector
spaces; old records remain available as recent memory but require regeneration before
they participate in semantic ranking.

Outgoing sends use durable intents. Unknown Telegram outcomes are marked `uncertain`
and are not blindly resent; exactly-once external delivery is not promised. The UI
provides paginated history/lists and manual confirmation or dismissal of uncertain
deliveries/tool calls, with evidence, version checks and recorded administrator decisions.
Bot replies use simple standard Markdown, converted to Telegram's supported HTML subset;
raw HTML and unsafe link schemes are escaped instead of passed through.
Resolving an outcome never blindly repeats the uncertain external operation. Schedules support one-shot or fixed-interval
execution (minimum 60 seconds); after downtime, repeat schedules emit one overdue
occurrence and advance to the next future time. Calendar/timezone recurrence is pending.

## Verification and deployment

```sh
make test
make vet
make test-race
```

Tests use isolated databases and fake transports. `TEST_POSTGRES_DSN` enables destructive
schema fixtures in a dedicated test PostgreSQL database; never point it at production.
`TEST_LM_STUDIO_URL=http://127.0.0.1:1234 go test ./internal/llm -run TestLiveLMStudio -v`
explicitly checks the configured local model's structured output.

[Docker/Kubernetes examples](examples/kubernetes/README.md) split API and worker replicas.
Container builds and PostgreSQL migration/startup smoke tests passed. Full PostgreSQL
queue contracts, failover and Kubernetes cluster deployment remain unverified.
Audio/video transcription, calendar/timezone recurrence, retention policy, semantic
search, production identity-provider validation, load testing and cluster failover remain
outside the verified scope; see the design checklist.

## Security and contributions

Never commit populated configuration, runtime databases, logs, screenshots or provider
credentials. See [SECURITY.md](SECURITY.md) for private vulnerability reporting and
[CONTRIBUTING.md](CONTRIBUTING.md) for development and privacy requirements.

## MCP tools

MCP clients use `github.com/modelcontextprotocol/go-sdk` v1.8.0. Add servers in
**MCP tools → Add server**, or under `app.mcp.servers` in YAML. The UI supports
create/edit/delete, discovery without invoking tools, and resetting the DB override
to the file. All MCP server definitions (including credentials) are stored as one
versioned, encrypted `mcp.servers` override. Credentials are never returned by the API;
blank secret inputs preserve existing values. DB changes apply automatically across
API/worker replicas; editing the bootstrap YAML still requires a restart.

```yaml
app:
  llm:
    endpoint: https://your-litellm.example
    model: Default
    api_key: YOUR_PRIVATE_KEY
  mcp:
    servers:
      documents:
        enabled: "true"
        transport: http
        url: https://your-mcp.example/mcp
        token: YOUR_PRIVATE_BEARER_TOKEN
        bots: '["assistant"]'
        tools: '["search", "get_document"]'
      local_tools:
        enabled: "false"
        transport: stdio
        command: /usr/local/bin/my-mcp-server
        args: '["--stdio"]'
        env: '{"API_KEY":"YOUR_PRIVATE_KEY"}'
        bots: '["assistant"]'
        tools: '["lookup"]'
```

This is a snippet to merge with the rest of your configuration, not a complete
bootstrap file. The production LLM provider is **LiteLLM**. LLM endpoint/model,
agent identity, bind address, public URL, process role and OIDC client ID must be
explicitly configured; no environment-specific values are built into defaults.
Opt-in local-provider tests are development-only.
The production router must support OpenAI-compatible native `tools`/`tool_calls`
and JSON Schema responses. No provider-specific execution path or fallback is used.

Transports: `http` (Streamable HTTP), `sse` (legacy), `stdio`. For stdio, install the
executable in each worker image; the agent does not download packages or invoke a
shell. Only the explicitly supplied environment is inherited by the child. Remote
credentials use Bearer auth; interactive MCP OAuth is not implemented. Use HTTPS
for non-local credentials; plain HTTP is available for explicitly configured internal
services. Cross-origin requests/redirects and remote schema loading are blocked.

Bot/tool allowlists are explicit (`*` is an intentional grant to all). The bot list
uses config aliases, such as `assistant`, not Telegram IDs. Empty lists deny access.
Tool annotations are not authorization. Every call rechecks global user/chat blocks
and validates arguments against the discovered JSON Schema. Sessions and tool
transcripts belong to one queue job; the MCP server remains responsible for isolation
of its own data under its configured credentials. Do not connect a cross-user private
memory server with a shared credential and expect tgaia to partition its storage.

The loop is bounded to 4 model rounds / 8 calls / 75 seconds, 20 seconds per MCP call,
64 exposed tools and 16 KB per result. Binary/resource content is not dereferenced.
Tool turns and intents/results are durable in `tool_steps` (migration 009). Unknown
outcomes stop further actions and are not blindly repeated on queue retries; this
is not exactly-once execution in an external system. Compaction never invokes tools.

Tests use actual SDK HTTP/SSE and stdio fixtures, bot access/schema checks, encrypted
CRUD, and retry/unknown-outcome checkpoints. A live local-model integration is opt-in:

```sh
TEST_LM_STUDIO_URL=http://127.0.0.1:1234 go test ./internal/service -run TestLiveModelMCP -v
```

Protocol references: [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk),
[LiteLLM function calling](https://docs.litellm.ai/docs/completion/function_call).

## Default bot system prompt

When a bot's DB system prompt is empty or whitespace-only, the worker reads
`system-prompt.md` from the directory containing `--config`. The repository includes
a base [personal-assistant prompt](config/system-prompt.md); copy or customize it beside
your ignored config file.
A nonempty per-bot DB prompt replaces this fallback. Application protocol, access
rules and scheduled execution instructions still apply. The file is read for every
new generation, so edits apply without restarting; cached job results remain cached.
Missing/empty/unreadable files or files over 16000 bytes fail that generation with
a clear error rather than silently running without assistant instructions.
Mount the Markdown file alongside config.yaml on every API/worker replica.
Without `--config`, the file is resolved from the working directory at startup.

## Make targets

`make build` creates `bin/tgaia` with CGO disabled. `make test` runs all tests;
`make test-race` runs them with the race detector and requires a C compiler.
`make check` runs tests and `go vet`. `make` lists the available targets.
Go 1.27.3 is specified in go.mod; toolchain selection follows standard Go behavior.

Override `GO`, `BINARY` or `TEST_ARGS` as needed, for example:

```sh
make build BINARY=bin/tgaia-dev
make test TEST_ARGS='-count=1 -run TestScheduled'
```

No local LLM endpoint, credentials, config path or Go cache location is baked into
the Makefile. Live LLM tests remain opt-in through their documented environment variables.

## Containers and Kubernetes

The two-stage [Dockerfile](Dockerfile) builds a CGO-free binary and runs it as UID 10001
in Alpine with CA certificates. Configuration and `system-prompt.md` are mounted
externally; neither file is embedded in the image. See the complete
[deployment guide](examples/kubernetes/README.md) for build/push, PostgreSQL, OIDC, Secrets,
ConfigMaps, migration Job, API/worker Deployments, TLS ingress, scaling and upgrades.

## GitHub Actions and GHCR

[CI](.github/workflows/ci.yaml) runs the CGO-free build, tests, vet and race detector
on pull requests and branch/tag pushes. Pull requests and non-default branches build
and smoke-test a local image without registry login or package write permissions.
After checks pass, pushes to the repository's default branch or `v*` tags publish
`ghcr.io/<owner>/<repository>` for linux/amd64 and linux/arm64. The name is lowercased
automatically. Manual runs publish only from those same refs.

Every published image receives a `sha-<full-commit>` tag. The default branch also
updates `latest`; semantic version tags such as `v1.2.3` add `1.2.3` and `1.2`.
Prereleases retain their prerelease version and do not update `latest`. The workflow
records the immutable digest in its summary; prefer that digest for deployments.
It uses the built-in GITHUB_TOKEN with packages:write only in the publication job,
so no personal token is required. If the GHCR package already exists, grant this
repository Actions access to it. Package visibility is configured separately in
GitHub; the workflow does not make private packages public.

Actions are pinned to verified release commit hashes. BuildKit cache, minimal
provenance and SBOM generation are enabled. Config and system-prompt.md remain
external mounts. Actual publication requires pushing this workflow to GitHub;
local validation does not publish an image.

Reference: [GitHub container publication guide](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images).

## Telegram media, topics and sender identity

Forum topics share one history, one memory summary and one set of explicit memories per
chat (migration 018 collapses earlier per-topic summaries, keeping the most advanced one);
outgoing replies and scheduled tasks still retain their topic. Chat blocks cover all topics. Edits update stored content and
invalidate affected summaries. Hidden authors/channel principals get reply-only access
on mentions/replies; they cannot invoke tools or create tasks.

Images use the configured LiteLLM vision-capable model, with extracted descriptions
stored as untrusted reference data. UTF-8 text documents are supported; audio/video
recognition is disabled until models are available. Downloads and extracted text are
bounded; unsupported content is identified explicitly. Every generation receives fresh
UTC time and the bot's Telegram name/username. See design sections 28–29 for limits,
migration requirements and verification scope.

### Operational limits and quotas

Use `app.limits` in YAML or `limits.<name>` in the database/Web UI. Values are strings; durations use Go syntax (`30s`, `2m`). Changes are validated and apply without restarting the process. Worker options drain/cancel the old worker group and start a replacement; in-flight calls otherwise retain their immutable configuration snapshot.

Use the Web UI scope selector for shared policy/identity defaults and API/worker specializations. API replicas can specialize `telegram_timeout`; worker replicas can specialize worker, LLM, compaction, tool and media limits. Shared quotas remain shared deliberately. MCP definitions may be shared or worker-specific.

| Group | Settings (defaults) |
| --- | --- |
| Workers | `worker_concurrency` (4), `worker_poll` (1s), `worker_lease` (30s), `worker_heartbeat` (10s), `worker_timeout` (12m; must exceed `tool_timeout` + `llm_timeout`), `worker_shutdown` (15s), `retry_base` (1s), `retry_max` (1m) |
| LLM | `llm_concurrency` (4), `llm_timeout` (5m, total ceiling per streamed call), `llm_first_token_timeout` (2m, silence before the first chunk), `llm_idle_timeout` (45s, silence between chunks), `embedding_timeout` (30s), `llm_input_bytes` (128000), `llm_output_tokens` (2048) |
| Reply context | `reply_messages` (30), `reply_context_bytes` (100000) |
| Compaction | `compaction_messages` (100), `compaction_input_bytes` (90000), `compaction_output_tokens` (2048), `compaction_min_messages` (10; a chat is compacted once this many messages left the window, or earlier when more than `reply_messages` are unsummarized), `compaction_summary_chars` (6000, hard cap applied in code) |
| Tools | `tool_timeout` (5m total phase), `tool_rounds` (4), `tool_calls` (8), `tool_output_tokens` (4096 per tool turn), `tool_context_bytes` (118000), `mcp_call_timeout` (20s), `mcp_result_bytes` (16000) |
| Telegram/media | `telegram_timeout` (35s, minimum 31s for long polling), `image_bytes` (8388608), `document_bytes` (262144), `media_messages` (4) |
| Shared DB quotas | `llm_calls_per_chat_hour`, `compaction_calls_per_chat_hour`, `active_tasks_per_user` (all 0 = disabled) |

Hourly quotas use database time and fixed UTC-hour windows, scoped by agent/bot/parent chat across topics and replicas. Response quota counts actual final, tool-planning and vision call attempts; compaction has a separate quota. A reserved call is charged even if the provider fails. Cached results consume no new quota. Exhaustion prevents provider calls and surfaces as a failed job through the normal retry policy; it does not promise a response or deferred execution. Active-task quotas count the user's tasks across the agent and apply to creation/reactivation through the bot and admin UI.

`memory.window` is the age threshold for compaction, not a fixed grouping interval; `memory.interval` controls scheduler scans. Each batch extends the previous summary with the oldest eligible unsummarized messages, bounded by count and input bytes. If a single message cannot fit, the job fails rather than silently losing its content: raise the configured budget. Protocol/storage safety ceilings (message/result sizes, maximum supported input, Telegram chunk size) remain enforced even when operational settings increase.

## License

Licensed under the [MIT License](LICENSE).
