# Embedded Personal Agent service

`cmd/personal-agent` is the headless entry point for embedding an Agent in an
existing application. The host application supplies its user interface,
authenticated user identity, business data and tools. SopAgent owns planning,
dynamic Agent definitions, independent Agent invocations, task state, user
confirmation and reusable SOP snapshots.

The service uses the existing MySQL repository. It does not start Studio,
coding CLIs, Docker sandboxes or the software delivery integrations.

## Responsibilities

| Component | Owns |
| --- | --- |
| SopAgent | Planner, generated role definitions, dependency graph, Agent execution, task and event records, confirmation, SOP definitions and snapshots |
| Host backend | User authentication, project membership, model gateway, attachments, business tables, business validation and execution tools |
| Host frontend | Request entry, file upload, task progress, role results, confirmation and SOP selection |

Keep deployment credentials, real business tables and organization-specific
adapters in the host backend. No business roles are seeded by this service:
the Planner generates them from the current request and the available tools.

## Configure and start

Use the Go version declared in `go.mod` and an accessible MySQL database.
Create `local.env` in the repository root for this machine's configuration.
`/local.env` and `/local.env.*` are ignored by Git. The command loads
`local.env` from its working directory automatically; exported environment
variables take precedence, including explicitly exported empty values.

```dotenv
AGENTFLOW_MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/agentflow'
SOP_AGENT_SERVICE_TOKEN='<server-to-server token>'
SOP_AGENT_GATEWAY_TOKEN='<different gateway token>'
SOP_AGENT_GATEWAY_URL='http://127.0.0.1:3009/internal/sop'

# The host backend's model configuration; use your authorized provider.
OPENAI_BASE_URL='https://your-authorized-model-gateway.example/v1'
OPENAI_API_KEY='<model API key>'
MODEL_NAME='<model identifier>'
```

Start the service from the repository root:

```bash
go run ./cmd/personal-agent
```

Use `-env-file /absolute/path/to/local.env` for a different configuration file,
or `-env-file ''` to use only the process environment. A missing default
`local.env` is allowed; an explicitly requested missing file fails startup.
The loader supports `export KEY=value`, single- and double-quoted values,
comments, and common double-quoted escapes. It treats shell expressions and
variable references as literal data: no command execution or interpolation.
Configuration errors identify the line without printing its contents.

The Go service reads service, database and gateway settings. **Model calls
still run in the host backend's gateway**, so the backend process must also
load this same `local.env` before importing its model configuration. Adding
`OPENAI_*` only to the Go process does not configure the backend's model.
The `data-agent-chat` backend uses the same `OPENAI_BASE_URL`,
`OPENAI_API_KEY` and `MODEL_NAME` names as its existing `backend/.env`.
For example, launch that backend with an explicit shared environment file:

```bash
# Run from the data-agent-chat/backend directory.
uv run --env-file /absolute/path/to/SopAgent/local.env python -m uvicorn app.main:app --host 127.0.0.1 --port 3009
```

Keep the actual file local. Do not include its contents in logs or commits.

The default listener is `127.0.0.1:8091`. Set `SOP_AGENT_ADDR` or `-addr` to
change it. Health is available at `GET /health`; it reports no configuration or
business data. A remote deployment should use TLS between services.

Run exactly one headless service instance against a database. Before accepting
requests, the command reconciles interrupted tasks left by a previous process:
active tasks become failed, and in-flight write calls become unknown. It does
not replay their operations. This runtime recovery is independent of schema
migrations and seed data. Starting another instance on a different address or
host against the same database is unsupported in this MVP.

`repo.Open` performs only incremental schema DDL for missing tables, columns
and indexes. Production uses MySQL. Baseline schema, data migrations and seed
data follow the repository's manual SQL migration policy.
The two new table baselines are in `migrations/034_personal_agent.sql` for
operators who prepare schema manually; the service never executes that script.

## Host backend API

All `/v1/personal-agent/*` requests require these headers:

```http
Authorization: Bearer <SOP_AGENT_SERVICE_TOKEN>
X-Project-ID: <project resolved by the host backend>
X-Actor-ID: <user resolved by the host backend>
Content-Type: application/json
```

The host backend must resolve identity from its authenticated session and
verify project membership before forwarding a request. It must not forward
arbitrary browser-supplied identity headers. Tokens stay on the server. The
bundled `data-agent-chat` integration is intended for a local, trusted MVP;
applications with accounts must supply their own identity binding.

| Method | Path suffix under `/v1/personal-agent` | Result |
| --- | --- | --- |
| GET | `/capabilities` | `{ "tools": [...] }` |
| GET | `/tasks` | `{ "tasks": [...] }` scoped to the current actor and project |
| POST | `/tasks` | `202` and the created task |
| GET | `/tasks/{id}` | Latest task state, generated roles, results and events |
| POST | `/tasks/{id}/feedback` | `202` and revised task |
| POST | `/tasks/{id}/confirm` | `202` and confirmed task |
| GET | `/sops` | `{ "sops": [...] }` within the caller's authorized scope |

Create a task:

```json
{
  "request": "Describe the requested work here",
  "context": { "attachmentId": "host-owned-file-id" }
}
```

Use `sopId` to request reuse of an existing SOP. The saved roles and plan are
loaded for a new execution; current input, business facts and confirmation
belong to the new task. The service does not reuse the previous task's live
Agent conversation as the new request's state.

Submit a correction while the task is waiting for confirmation:

```json
{ "revision": 7, "message": "Clarify or correct the proposed scope" }
```

Confirm the exact proposal currently displayed:

```json
{ "revision": 12, "planHash": "<hash from the latest task>" }
```

`revision` and `planHash` must come from the latest task response. A stale
confirmation or correction returns `409`; reload and review the changed
proposal. The browser should poll active tasks and stop polling terminal or
waiting tasks. A disconnected browser does not own the background execution.

Errors are JSON objects with an `error` string. Missing or invalid credentials
return `401`; missing scope or invalid JSON returns `400`; resources outside
the caller's scope return `404`. Request bodies are limited to 2 MiB.

## Generic gateway contract

The service calls only the gateway base URL configured by the operator. A task
cannot provide an arbitrary tool URL or model endpoint. Gateway calls use
`Authorization: Bearer <SOP_AGENT_GATEWAY_TOKEN>` and trusted project and actor
scope headers. The host backend validates this token before reading data or
invoking its model.

### List tools

`GET {gateway}/capabilities`:

```json
{
  "tools": [
    {
      "name": "lookup_record",
      "description": "Look up an authorized record by its stable identifier",
      "effect": "read",
      "inputSchema": {
        "type": "object",
        "properties": { "id": { "type": "string" } },
        "required": ["id"],
        "additionalProperties": false
      }
    }
  ]
}
```

Tool names, descriptions and schemas describe project capabilities. The
Planner can create new roles and task instructions, but can only assign
registered tools. Tools marked `write` are not available before confirmation.

The host may also declare `requiredPhase: "analysis"` or
`requiredPhase: "execution"` on a tool. The Planner must assign that capability
to the named phase. Before a phase completes, the runtime requires a successful
tool-call receipt; an Agent's generated completion message is insufficient.
Unresolved analysis questions may pause for user input without those receipts,
but they block confirmation. The listing demo requires a preview before
confirmation and execution plus read-back receipts afterwards. Omit the field
for optional tools. Business tools must return an error for failed acceptance,
such as a read-back mismatch, instead of treating a model summary as evidence.

### Invoke the model

`POST {gateway}/model` receives:

```json
{
  "purpose": "planner",
  "system": "Runtime-generated instructions and the required JSON contract",
  "messages": [ { "role": "user", "content": "Task input" } ]
}
```

Return `{ "content": "<model response text>" }`. The service supplies the
required structured response contract in the system prompt. Each generated
Agent receives its own model invocation and bounded context. The gateway
holds model credentials; they are never sent to the browser or saved in role
definitions. Use the model provider and deployment region approved for the
host application.

### Execute a tool

`POST {gateway}/tools/{name}` receives:

```json
{
  "taskId": "task-id",
  "callId": "stable-call-id",
  "arguments": { "id": "record-id" },
  "approval": {
    "taskId": "task-id",
    "revision": 12,
    "planHash": "approved-proposal-hash"
  }
}
```

Return `{ "result": <structured result> }`. The `approval` field is used for
confirmed execution. The backend is responsible for domain authorization,
input validation, idempotency, concurrency checks and reading back results.
A workflow confirmation does not by itself authorize arbitrary business
writes. Production adapters must bind approval to the concrete immutable
business change plan and validate it at their write boundary.

## MVP boundaries

- Roles are generated at runtime, not selected from seeded business personas.
- Execution is bounded by the configured tool registry and runtime limits.
- SOPs preserve generated definitions and a task graph, rather than replaying
  a previous chat transcript.
- First use and SOP reuse both pause before confirmed execution.
- The business adapter in `data-agent-chat` demonstrates local business data
  and simulated operations. Real production writes require the host system's
  authorization and conditional update semantics.
- This headless path shares repository infrastructure with LoopForge. It does
  not yet import arbitrary Studio workflows or replace the existing CLI
  workflow Engine.
- Task status is durable. A single instance performs background execution;
  restart recovery records interrupted work as failed and uncertain writes as
  unknown. Inspect actual business results before retrying or creating another
  task. Durable multi-worker scheduling and automatic write retries are not
  implemented.

## Focused checks

```bash
go test ./internal/application/personalagent ./internal/httpapi/personalagentapi
go test ./cmd/personal-agent
go build ./cmd/personal-agent
```

These checks require the declared Go toolchain. Unit tests use test doubles
and the test-only repository helper, not production credentials or business
data.
