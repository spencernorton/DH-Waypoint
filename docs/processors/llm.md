# Independent LLM processor

DH-Waypoint sends versioned jobs to a separate HTTP service. The service calls
an OpenAI-compatible inference endpoint and returns an attributed proposal.
Core owns queues, leases, source revisions, provenance, validation and feedback
routing. The adapter owns prompts, models, provider credentials and budgets.
`builtin/llm` and built-in online calibration are retired; see
[migration](migration.md#retired-built-in-inference-and-learning).

## Run without a provider or plugin

No processor is required for discovery, metadata storage or search. The native
binary now defaults to `PROCESSING_MODE=basic`, matching Compose. Use `raw` for
uninterpreted metadata, or explicitly choose `full` for the retained deterministic
classifier. None of these modes makes model calls.

Build the independent adapter from a checkout:

```sh
go build -o dh-waypoint-llm ./plugins/llm/cmd/dh-waypoint-llm
./dh-waypoint-llm -config plugins/llm/local.example.json
curl --fail http://127.0.0.1:8091/v1/manifest
curl --fail http://127.0.0.1:8091/v1/status
```

The adapter imports only the public contract and Go's standard library. It
does not start the backend or access its database. The backend binary does not
link the adapter. Its own [Dockerfile](../../plugins/llm/Dockerfile) builds a
separate image; the backend image does not contain this executable.

Both supplied service configs start with `enable_inference: false` and
`dry_run: true`. Jobs return a bound abstention without contacting a provider.
Readiness confirms local configuration, not upstream reachability or model
quality. After testing, set **both** `enable_inference: true` and
`dry_run: false` in your service config to permit inference. Restart the service
and re-register its changed immutable identity. Core `mode: shadow` records
proposals but still makes inference calls when the adapter enables them.

## Local and hosted setup

Copy [local.example.json](../../plugins/llm/local.example.json) or
[hosted.example.json](../../plugins/llm/hosted.example.json) to your own config.
Set the exact model ID your endpoint serves. A local server must already be
running and have the model loaded; this adapter never installs a runtime or
downloads weights. The example model names demonstrate configuration, not
quality or availability guarantees.

The adapter listens on `127.0.0.1:8091` by default. On a container network,
set `listen` to `0.0.0.0:8091`, provide a non-empty `LLM_PROCESSOR_TOKEN` in
the adapter process, and expose the port only to core. A network listener
without a service token fails startup. For local inference from a container,
change the provider URL to an address reachable from that container; Docker
Desktop supplies `host.docker.internal`, while Linux needs explicit routing.

For a hosted endpoint, provide `LLM_PROVIDER_API_KEY` **only to the adapter**.
Alternatively set `api_key_file` to a mounted secret and omit `api_key_env`.
Credential files are limited to 4 KiB. No credential value belongs in JSON,
plugin files, registration configuration, traces or source. The process checks
that its provider credential and processor service credential differ.

Register the service using the updated
[local](../../examples/plugins/04-llm-local-ollama.plugin.yml) or
[hosted](../../examples/plugins/05-llm-hosted.plugin.yml) template:

```yaml
id: local-llm
driver: http
endpoint: http://127.0.0.1:8091
auth_env: DH_WAYPOINT_PROCESSOR_TOKEN_LOCAL_LLM
mode: shadow
priority: 50
selector:
  content_types: [unknown, movie, tv_show]
  has_content_match: false
  sample_basis_points: 1000
configuration: {}
```

Set the core's `DH_WAYPOINT_PROCESSOR_TOKEN_LOCAL_LLM` to the adapter's
`LLM_PROCESSOR_TOKEN`. This is a separate service-auth secret, never a provider
API key. For native loopback testing with no service token, omit `auth_env`.
Change the template hostname to the adapter's reachable address. The manifest
ID must match the registration. Configuration must be `{}`: the adapter rejects
provider settings or any other per-job configuration from core.

## Inference API and provider differences

The adapter implements the following portable subset, rather than assuming
every provider implements the complete OpenAI API:

- One non-streaming `POST` to the configured full `/chat/completions` URL.
- Optional `Authorization: Bearer` from the adapter's environment or secret file.
- JSON request: `model`, two text `messages` (system and user), `stream: false`,
  and exactly one configured output-token limit field.
- Optional `response_format: {"type":"json_object"}` when `json_mode` is true.
- `temperature` is omitted unless explicitly configured, and is **always
  omitted for GPT-5 family names**, including gateway names such as
  `openai/gpt-5-mini`.
- JSON response with exactly one `choices[]` entry, a string
  `message.content`, `finish_reason: "stop"`, and no refusal. Additional
  provider envelope fields are permitted. Model output is decoded strictly.

| Provider | Adapter settings and documented differences |
|---|---|
| OpenAI | Use `https://api.openai.com/v1/chat/completions`, a provider key and `token_limit_field: "max_completion_tokens"`. This bound includes reasoning tokens. `max_tokens` is deprecated and incompatible with some reasoning models. JSON mode is model-dependent. [API reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create). |
| Ollama local | Use `http://127.0.0.1:11434/v1/chat/completions`, no provider key, and `token_limit_field: "max_tokens"`. It documents a subset of OpenAI fields, including JSON response format; local authentication is not required. [Compatibility](https://docs.ollama.com/api/openai-compatibility), [authentication](https://docs.ollama.com/api/authentication). |
| vLLM | Use the server's `/v1/chat/completions`, its served model ID and optional configured API key. The model needs a chat template. Server generation configuration can affect defaults; qualify the actual server/model pair. No vLLM-specific sampling extensions are sent. [Official server documentation](https://docs.vllm.ai/en/stable/serving/online_serving/openai_compatible_server/). |
| Other gateways | Use their documented full chat-completions URL and token field. Leave `json_mode` false if unsupported; the prompt still requires JSON and strict parsing remains mandatory. Native Messages, Responses, streaming, tools, batch and embeddings APIs need a different external adapter. |

These are documentation-backed dialects, verified with synthetic HTTP fixtures.
No live hosted inference, local model quality or model-specific JSON reliability
is claimed by the test suite. Reasoning models can exhaust a small output cap
without visible JSON; increase the cap deliberately or select another model.
There is no automatic endpoint fallback or hidden provider retry.

## Proposals and identity

The default prompt requests:

```json
{"outcome":"completed","proposal":{"content_type":"movie"},"uncertainty":0.1,"reasons":["metadata_evidence"]}
```

An abstention omits `proposal`. Advertised capabilities are selected from
`classify`, `extract`, `match` and `language`. The model cannot set bindings,
tags, weights, policy exclusions, liveness or embeddings. Matches must copy an
offered candidate even for multi-capability jobs. Language evidence is limited
to `metadata_claim`. Invalid, truncated, refused, oversized or out-of-capability
output becomes a bound technical failure with no proposal. Core independently
revalidates the result and rechecks source integrity before applying it.

`system_prompt_file` optionally supplies a bounded service-local prompt. Model,
prompt, endpoint and inference policy changes produce a new manifest identity.
Warm the replacement, inspect its manifest and register it under a new core
execution generation; old leases remain fenced by core. Hosted aliases are not
immutable weights: operators must pin a provider version when reproducibility
requires it. `artifact_digest: "none"` and `qualification: "unqualified"` are
intentional; generated fingerprints do not certify a model artifact.

The service stays on `dh-waypoint.processor.v1` and its 40-hex SHA-1 input.
It does not widen hashes, accept unknown identity fields or claim pure-v2
processor support. A future negotiated contract requires an explicit update.

## Budgets, deadlines and retries

Service-local defaults are one concurrent call, ten reservations per minute,
100 total call reservations per process, 1,024 output tokens, 32 KiB of input
JSON and an eight-second timeout. Configurations outside bounds fail startup.
Failures and cancellation consume reservations. The adapter also respects the
earlier core job deadline. Busy/rate-limited requests return `deferred`; an
exhausted total cap returns `failed`. Redirects are refused and provider bodies
are never copied into error messages. Responses are capped at 256 KiB.

Successful results have a bounded in-memory retry cache (256 entries by
default); concurrent retries of the same job cannot trigger duplicate calls.
Reusing a cached job ID with changed input/bindings fails. Cache eviction or a
service restart can cause a later retry to incur another provider request.
Budgets reset on restart and apply separately to each replica. These are
bounded operational controls, **not a durable monetary cap or an exactly-once
provider guarantee**. Use provider-side quotas or a persistent external budget
gateway for a cross-restart or multi-replica spending limit.

Keep core selectors and admission small during evaluation. Configure core's
request timeout above the adapter timeout, within core's existing resource
bounds. Start in shadow mode; apply mode is a separate decision after evaluation.

## Feedback

Core retains operational receipts, teacher comparisons, verified imports and
human feedback with their source/tier; an operational receipt is not an
independent correctness label. Pull/cursor APIs continue to work.

To enable adapter receipt storage, set `feedback_file` to a writable JSONL path
on a dedicated persistent volume, then enable `learning.push_enabled` on the
HTTP registration. The adapter sets `request_feedback`, accepts the existing
`dh-waypoint.processors.feedback.v1` batch and acknowledges only after appending
and syncing it. Storage is capped by `max_feedback_bytes` (16 MiB default).
Storage failure/full returns 503 so core retains/retries delivery. With no file,
the feedback endpoint returns 404 and automatic feedback requests are disabled.

Delivery is at least once. An offline trainer must deduplicate `event_id`,
respect evidence tiers and publish a replacement service identity after training.
The receipt spool does not train a model or modify core rules. Rotate/export it
under operator control before filling; it contains metadata and ground truth,
so protect it like other catalogue exports.

## Validation

```sh
go test -race ./plugins/llm/... ./scripts/processorboundary
python3 scripts/check_processor_boundary.py
```

The boundary guard checks actual backend dependencies and parsed Go syntax.
Regressions put direct provider calls inside the permitted processors package,
including the generic transport, and require failure. It also checks that the
adapter never imports backend internals. The guard supplements review; it is
not a security sandbox for deliberately obfuscated source code.
