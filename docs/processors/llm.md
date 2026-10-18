# Connecting an LLM (hosted or local)

DH-Waypoint core ships no model runtime. Language models plug in as
*processors* through two routes:

1. **`builtin/llm`** — zero code. Point a plugin file at any
   OpenAI-compatible chat-completions endpoint. Best for hosted
   APIs and for local servers (Ollama, LM Studio, llama.cpp, vLLM).
2. **Your own `http` service** — wrap any runtime (transformers,
   Ollama's native API, a fine-tuned classifier, an ensemble)
   behind the three-endpoint processor protocol.

Both routes get the same guarantees: durable bounded jobs, retries,
shadow mode, core-side validation of every proposal, and no path
for a model to delete catalogue records.

## Route 1: `builtin/llm` plugin file

Copy a template into your plugins directory:

- Local: `examples/plugins/04-llm-local-ollama.plugin.yml`
- Hosted: `examples/plugins/05-llm-hosted.plugin.yml`

```yaml
id: local-llm
driver: builtin/llm
mode: shadow
endpoint: http://host.docker.internal:11434/v1/chat/completions
configuration:
  model: llama3.1:8b
  temperature: 0
  capabilities: [classify, extract]
  system_prompt: "..."
```

How it works: for each selected torrent the core POSTs
`{model, temperature, response_format: json_object, messages}` to
`endpoint` (the full chat-completions URL). The user message is the
job's JSON input (name, files, size, deterministic facts, match
candidates, prior processor results). The model must reply with a
single JSON object in the message content:

```json
{"outcome": "completed",
 "proposal": {"content_type": "movie",
              "extraction": {"title": "The Matrix", "year": 1999},
              "match": {"source": "tmdb", "id": "603"}},
 "uncertainty": 0.1,
 "reasons": ["title and year in name"]}
```

- `outcome`: `completed` proposes facts; `abstained` is always
  safe and preferred over guessing.
- `proposal` fields are validated by core exactly like any other
  processor: a `match` must copy `source`/`id` from the candidates
  in the input, types must be valid content types, and identity
  rules (title/year/episode corroboration) still apply. Invalid
  output is a technical failure for that job, never a catalogue write.
- `capabilities` defaults to `[classify, extract, language, match]`.
  Declare only what your prompt actually does.
- The default system prompt (used when `system_prompt` is empty)
  asks for exactly this shape; override it to encode your own
  taxonomy notes, abstention rules, and examples.

### Endpoint cheat sheet

| Runtime | Endpoint | Model example | Key needed |
|---|---|---|---|
| Ollama (local) | `http://host.docker.internal:11434/v1/chat/completions` | `llama3.1:8b`, `qwen2.5:14b` | no |
| LM Studio (local) | `http://host.docker.internal:1234/v1/chat/completions` | loaded model id | no |
| llama.cpp server | `http://host.docker.internal:8080/v1/chat/completions` | served model id | no |
| vLLM | `http://vllm:8000/v1/chat/completions` | served model id | as configured |
| OpenAI | `https://api.openai.com/v1/chat/completions` | `gpt-4o-mini` | yes |
| OpenRouter | `https://openrouter.ai/api/v1/chat/completions` | `openai/gpt-4o-mini` | yes |

From inside the compose stack, `host.docker.internal` reaches
services on the Docker host. On Linux add
`extra_hosts: ["host.docker.internal:host-gateway"]` to the
`dh-waypoint` service.

### API keys

Keys never appear in plugin files or the registry. Name the
variable in `auth_env` and provide the value outside:

```yaml
auth_env: DH_WAYPOINT_PROCESSOR_TOKEN_OPENROUTER
```

The core resolves it from the process environment, or from a file
of the same name in `PROCESSORS_CREDENTIAL_DIRECTORY`
(default `/run/secrets/dh-waypoint-processors`) and sends it as
`Authorization: Bearer ...`. Missing secret = job fails closed.

### Cost and latency controls

- `selector.sample_basis_points`: deterministic sampling
  (1000 = 10%). Start small.
- `selector.has_content_match: false` + `content_types`: send only
  what deterministic processing could not settle.
- `selector.min_uncertainty`: escalation-only routing behind a
  cheaper earlier stage.
- `PROCESSORS_REQUEST_TIMEOUT` (default 10s, max 60s) hard-bounds
  every call; `PROCESSORS_CONCURRENCY` bounds parallelism;
  `PROCESSORS_MAX_ATTEMPTS` bounds retries. Local models that need
  more time: raise the timeout, lower concurrency.
- Pair with `PROCESSING_MODE=basic` (core stamps a coarse category)
  or `raw` (core stamps nothing) so the model — not the built-in
  classifier — owns the decisions you care about.

## Route 2: wrap your own model as a service

Use this when the runtime is not OpenAI-compatible, when you need
batching/prefix-caching/embeddings, or when several models vote.
The service owns its weights, prompts, credentials and GPU; the
core owns jobs, retries and validation.

Contract (full spec: [processor protocol v1](v1.md)):

- `GET /v1/manifest` → identity + capabilities + config schema
- `GET /v1/status` → readiness + counters
- `POST /v1/process` → one `Job` in, one `Result` out, synchronous,
  bounded by the job deadline; idempotent per job id

Start from the reference services and replace the decision logic:

- Go: `examples/processors/reference/main.go` (imports only
  `processorapi/v1`)
- Python: `examples/processors/independent/service.py` (stdlib
  only; swap the fixture rule for a call to your local runtime —
  Ollama's native `/api/chat`, a transformers pipeline, anything)

Skeleton of the Python path with a local model call:

```python
def process(job):
    name = job["input"]["name"]
    answer = ollama_chat(model="llama3.1:8b", prompt=build_prompt(name))
    proposal = parse_proposal(answer)      # your validation
    return {"outcome": "completed" if proposal else "abstained",
            "proposal": proposal,
            "uncertainty": 0.2,
            "reasons": ["local-model"]}
    # binding fields may be omitted; core fills them from the job
```

Then: containerise beside the core (`examples/processors/compose.yml`
shows resource limits and read-only hardening), probe with
`dh-waypoint plugin test <endpoint>`, and register by file
(`06-http-service.plugin.yml`) or API.

## Rollout checklist (both routes)

1. Register in `mode: shadow` with a small sample.
2. Compare proposals against outcomes you trust (your *arr grabs
   are ground truth; see [benchmarks](../project/benchmarks.md)).
3. Widen the sample, then switch to `mode: apply`.
4. Replacing a model = a new plugin identity: warm the new
   endpoint, edit the file, and the registry swaps generations
   without a core restart. Applied facts keep their provenance.
