# modeltrace-guard

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that watches routing and detects account "downgrading" (降智) with the [ModelTrace](https://github.com/xqy2006/ModelTrace) long-integer fingerprint.

The plugin runs in-process as a CGO dynamic library, observes every completed request through the usage hook, periodically probes your own credentials with the ModelTrace three-challenge long-integer suite, scores the digit outputs against the enrolled fingerprint bank, and serves the verdicts through the Management API plus a browser-navigable dashboard resource.

## What it does

- **Routing records (usage hook)** — every completed request is sanitized into a compact routing record: provider, requested model, upstream-reported response model, account (masked), latency/TTFT, token counters, failure codes. This answers "which account actually served this request, and what did upstream say it is?"
- **Fingerprint probes (host.model.execute)** — on a schedule or on demand, each active credential receives up to 3 long-integer generation challenges (the exact ModelTrace enrollment prompts, 1..355 closed range). Execution is locked to the exact credential via `AuthID` + `ForcedProvider`, so probing never disturbs normal routing.
- **Closed-set attribution** — digit outputs are scored with the ModelTrace pipeline (nuisance-projected Hellinger model-center similarity + 0.25 × ordered-block digit-sequence features, softmax with the bank calibration temperature). The top-1 model is compared against the model you configured for that provider: `match` / `mismatch` (降智 flag) / `unlabeled` / `insufficient`.
- **Management API** — authenticated JSON routes under `/v0/management/modeltrace-guard/...` plus an unauthenticated dashboard resource.

Port fidelity: the Go scorer attributes **462/468 (98.7%)** of the ModelTrace reference corpus back to the recorded model (`go test -run TestReferenceAttribution`).

## Build

Go 1.26+ and CGO are required (the plugin is a c-shared library).

```bash
# From the repository root (this folder)
go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.so .

# macOS
CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.dylib .

# Windows
CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.dll .
```

The build also emits a C header (`modeltrace-guard.h`); it is informational only and not needed at runtime.

The `go.mod` uses a `replace` directive pointing at `./upstream/CLIProxyAPI` (a reference clone). To build against a published release instead, remove the `replace` line and run `go mod tidy`.

## Install

1. Copy the built library into the host's plugin directory:

```text
CLIProxyAPI/
└── plugins/
    └── linux/
        └── amd64/
            └── modeltrace-guard.so
```

The docs accept `plugins/<GOOS>/<GOARCH>/` (preferred) or a flat `plugins/` folder.

2. Copy `data/unified_bank.json` somewhere the host can read (default lookup is `data/unified_bank.json` relative to the host working directory), or point `bank_path` at any absolute path. The bank ships in this repository under `data/` and comes from the ModelTrace project.

3. Enable the plugin in `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    modeltrace-guard:
      enabled: true
      bank_path: "data/unified_bank.json"
      interval_minutes: 60
      probes_per_run: 3
      providers:
        - codex
        - claude
      models:
        codex: gpt-5.6
        claude: claude-opus-5
```

4. Restart the host and verify registration:

```bash
curl -s -H "Authorization: Bearer <management-key>" http://127.0.0.1:8317/v0/management/plugins | jq '.[] | select(.id=="modeltrace-guard")'
# expect: registered: true, effective_enabled: true
```

## Configuration

| Field | Default | Description |
| --- | --- | --- |
| `bank_path` | `data/unified_bank.json` | Path to the ModelTrace unified bank file. The bank is cached and reloaded automatically when the file changes. |
| `interval_minutes` | `60` | Minutes between scheduled probe runs; `0` disables scheduled probing (manual `POST .../probe` still works). Values below 15 are clamped to 15 to protect quota. |
| `probes_per_run` | `3` | Challenge probes per run per credential (1-3). More probes = more quota usage but higher attribution accuracy (the calibration table is keyed by 1/2/3 outputs). |
| `providers` | *(all)* | Provider filter (matches `provider` or `type`), e.g. `codex`, `claude`. |
| `auth_ids` | *(all)* | Optional explicit credential filter (matches `id` or `auth_index`). |
| `models` | *(none)* | Provider → expected model id, used as the attribution baseline for verdicts. Without a mapping the run is reported as `unlabeled` (detection still shown). |
| `environments` | `[1, 5, 6]` | Which suite environments (1-12) to draw probes from; the defaults are the clean-transport ones (no system prefix, no user prefix). |
| `history_path` | *(off)* | Optional JSONL file to append every probe record to. |
| `history_size` | `200` | In-memory probe record ring size. |
| `routing_size` | `1000` | In-memory routing record ring size. |
| `entry_protocol` / `exit_protocol` | `openai` | Protocol translation applied to probe requests. |

Runtime updates are also possible via `PUT /v0/management/modeltrace-guard/config` with a JSON body of the same fields.

## Management routes

Authenticated (management key) JSON routes:

| Route | Description |
| --- | --- |
| `GET /v0/management/modeltrace-guard/status` | Plugin/bank/probe status summary. |
| `GET /v0/management/modeltrace-guard/history?limit=N` | Recent probe records (full detail, unmasked). |
| `GET /v0/management/modeltrace-guard/routing?limit=N` | Recent per-request routing records. |
| `GET /v0/management/modeltrace-guard/config` | Current runtime config. |
| `PUT /v0/management/modeltrace-guard/config` | Update runtime config fields (JSON body). |
| `POST /v0/management/modeltrace-guard/probe` | Trigger a probe run now (202, async). |
| `GET /v0/management/modeltrace-guard/bank` | Loaded bank summary (models, calibration, hash). |

Unauthenticated browser-navigable resource:

- `GET /v0/resource/plugins/modeltrace-guard/dashboard` — server-rendered dashboard: bank info, recent probe verdicts with badges, routing overview, recent requests. Account identifiers are masked on this page; use the authenticated routes for complete records.

## Verdicts and caveats

- `match` — detected top-1 equals the configured model for that provider.
- `mismatch` — a different banked model scores highest: treat as a 降智 signal and cross-check with the routing record's `response_model`.
- `unlabeled` — no `models` mapping configured for the provider; only the detection is reported.
- `insufficient` — every probe was refused or truncated; ModelTrace does not score unusable outputs.
- **System prompts bias fingerprints** (the ModelTrace README is explicit about this). CPA's own pipeline applies system prompts; the default probe environments are the clean-transport ones to minimize that bias, but treat probabilities as directional, not absolute.
- **Closed set only** — unenrolled models get arbitrary attribution. If you route a model that is not in the bank (13 models as of the shipped bank), the verdict is meaningless for it.
- **Quota** — probes are real generation requests (long outputs, ~300-700 tokens each). Keep `interval_minutes` at 60+ and `probes_per_run` at 3 or below unless you accept the consumption.
- Probes run one at a time per run and never concurrently with each other; a run in progress blocks further triggers.

## Project layout

```text
main.go            C ABI (cliproxy_plugin_init + call/free/shutdown) and RPC dispatch
config.go          plugin config parsing, defaults, runtime updates
challenges.go      ModelTrace challenge suite port (12 environments, exact prompt texts)
fingerprint.go     scoring port (parse, Hellinger, ordered blocks, softmax, JS similarity)
bank.go            unified bank loading, caching, validation, calibration lookup
prober.go          target enumeration, probe execution, verdict classification
usage_recorder.go  usage hook -> sanitized routing records
management.go      management registration + authenticated JSON routes
dashboard.go       unauthenticated server-rendered dashboard resource
port_test.go       fidelity tests against the ModelTrace reference corpus
data/unified_bank.json  fingerprint bank (from ModelTrace)
```

## Credits

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) — router-for-me; plugin ABI and examples this project is modeled on.
- [ModelTrace](https://github.com/xqy2006/ModelTrace) — xqy2006; fingerprint bank, challenge suite, and scoring pipeline this plugin ports.
