# Usage

## Requirements

- CLIProxyAPI v8 with dynamic plugin support and the quota provider.
- A CPA build that accepts the plugin's declared ABI and RPC schema: the plugin
  registers ABI 1 and schema 10 from its pinned SDK, and an older host refuses it
  at registration with `plugin schema version 10 is not supported`.
- **CPA v8.0.49 or later** to load the plugin (schema 10 first shipped there), and
  **CPA v8.0.62 or later** for credential-wide cooldowns: a spent shared free
  allowance cools every model of the credential. An older host ignores the scope
  and cools only the reporting model.
- Go 1.27.1 for local development. Release builds are produced by CI.

Sign-in and a live chat are not covered by an offline test. The unit tests drive
every upstream call through a fake sender, so the requests and responses the
plugin builds are checked, but a live request needs Zen's free tier to answer.
Verify a sign-in and a chat against your own deployment before relying on a
build, and report what you observe.

## Install from the plugin store

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-opencode-zen-free:
      enabled: true
      max_concurrency: 2
      opencode_version: 2.2.5
      # Model ids this plugin never advertises. Naming one here keeps CPA from
      # routing it to this credential at all, which matters when the credential
      # has the highest priority and no other provider serves the model.
      exclude_models: []
      # exclude_models: [big-pickle]
```

Use CPA's plugin store to install `omc-opencode-zen-free`, or call:

```text
POST /v0/management/plugin-store/omc-opencode-zen-free/install
POST /v8/management/plugins/store/omc-opencode-zen-free/install
```

Add `?version=<version>` to install a specific release. The store requires the
release to carry `omc-opencode-zen-free_<version>_<os>_<arch>.zip` and
`checksums.txt`, and verifies the archive against the checksum before unpacking it.

The archive contains one library file and is installed under
`<plugins-dir>/<os>/<arch>/` as `omc-opencode-zen-free-v<version>.so`, `.dylib` or
`.dll`, depending on the target platform: the installed version is part of the
file name. CPA also loads a hand-placed unversioned
`omc-opencode-zen-free.so`; that file carries no version, so a pinned version in
`plugins.configs` does not match it. Restart CPA if it reports that the loaded
plugin must be replaced.

Manual layout, for the version you install:

```text
<plugins-dir>/linux/amd64/omc-opencode-zen-free-v<version>.so
<plugins-dir>/linux/arm64/omc-opencode-zen-free-v<version>.so
<plugins-dir>/darwin/amd64/omc-opencode-zen-free-v<version>.dylib
<plugins-dir>/darwin/arm64/omc-opencode-zen-free-v<version>.dylib
<plugins-dir>/windows/amd64/omc-opencode-zen-free-v<version>.dll
<plugins-dir>/windows/arm64/omc-opencode-zen-free-v<version>.dll
```

## Description declaration

A release may publish an optional `<plugin_id>.omc.json` (`schema_version` 1) with
localized descriptions. When it does, the file is
`omc-opencode-zen-free.omc.json` and the plugin serves the same bytes at:

```text
GET /v0/resource/plugins/omc-opencode-zen-free/omc.json
```

The parts that matter here:

- `plugin_id` is `omc-opencode-zen-free`, and `plugin_version` is the release
  version without a leading `v`. A document naming another id or version is
  ignored, not guessed at.
- `i18n.description` may contain any subset of `zh`, `en`, `zh-Hant` and `ms`; each
  value is plain text.
- It declares no menu, settings label or action, and it never changes the plugin
  name, logo or the description CPA's store already shows.
- CPA serves plugin resources without management authentication, and answers 404
  for them in Home mode. When the resource is unavailable, a reader uses the
  release asset; when neither exists, the description in the official plugin
  source is the result.

The served body is never re-serialized: the plugin embeds the same file that
packaging copies into the release, so served and published bytes are comparable by
hash. Bump the file together with the plugin version.

## Build locally

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-opencode-zen-free.so .
```

Name the output with your platform's suffix: `.so` on Linux, `.dylib` on macOS,
`.dll` on Windows. `build/` is ignored by git. Official release artifacts for
Linux, macOS and Windows on both amd64 and arm64 are built by the release
workflow when a `v*` tag is pushed.

## Sign in

The plugin registers **one keyless OAuth login entry**:

| Entry identifier | Service | Credential |
| --- | --- | --- |
| `opencode-zen-free` | OpenCode Zen's free tier | the shared public key |

Start a login through CPA's shared OAuth endpoint:

```text
GET /v8/management/oauth/auth-url?provider=opencode-zen-free
```

The caller must supply the management key in the `Authorization: Bearer ...`
header. Do not put the key in a URL.

### Flow

1. The plugin mints a state and returns it with the project's explanation of the
   keyless identity as the URL. CPA's OAuth panel refuses a plugin flow whose URL
   is empty, so the plugin names that page. Opening it is optional: there is no
   page to complete, and nothing is submitted there.
2. The panel polls `GET /v8/management/oauth/status?state=<opaque-state>`. The
   **first poll succeeds** and returns the credential.
3. The credential is saved as `opencode-zen-free-public.json` with
   `"type": "opencode-zen-free"` and `{"access":"public"}`.

The flow is stateless. A poll after a plugin restart, or a second poll after a
successful one, still receives the same credential. The state carries only its own
expiry (five minutes) and a nonce, all of it public: there is no secret behind the
public key for the state to protect.

`/v0/management/get-auth-status?state=<opaque-state>` is the v0 alias. Cancel with
`DELETE /v8/management/oauth/session?state=<opaque-state>` (or
`DELETE /v0/management/oauth-session?state=<opaque-state>`). Cancellation is
handled by CPA; the plugin does not implement a separate cancellation endpoint.

### Credential

The saved file is fixed-name `opencode-zen-free-public.json`:

| Field | Meaning |
| --- | --- |
| `access` | always `public` |

There is no refresh token and no expiry. A refresh request answers the same
credential, and the host is told not to schedule another. Signing in again replaces
the same file rather than adding a second account, because only one public identity
exists. An existing `"type": "opencode-zen-free"` auth file is recognized and
republished when CPA imports it.

Personal Zen keys do not belong here. Use CPA's built-in OpenCode Zen provider, or
OpenCode's own `opencode` provider, for those.

## Models and routing

Model ids are Zen's ids, lowercased for CPA routing. The plugin reads:

- Zen's live list at `https://opencode.ai/inference/v1/models`, as OpenCode asks it
  (`Authorization: Bearer public`, OpenCode's `User-Agent`,
  `x-opencode-client: cli`). The list carries each model's name, limits,
  modalities, reasoning levels, price and the protocol it is asked at.
- models.dev's v2 catalog at `https://models.opencode.ai/api.json`, for the fields
  an older list does not carry. The shipped protocol table stands in when the
  catalog cannot be read either.

The list is reused for one minute. A request for a model the last list did not
have asks the list again, at most once a minute. The last good list is written to
`<auth-dir>/.opencode-zen-free-models.json` and read back after a restart, so a
network hiccup at start-up does not fall back to the shipped bootstrap. The
bootstrap list is only a first-run answer when Zen cannot be read at all.

- A model is free when it is not retired and either its id contains `free` or the
  catalog says its input and output cost nothing.
- Retired models and SystemOne models are excluded.
- **Only Chat Completions models are advertised.** A model Zen serves on Responses
  (`muse-spark-1.3-contributor-free`, for example) or Anthropic Messages is left
  out: this executor speaks chat completions, and a chat request to one is refused.
- A model Zen lists as `-free` that neither the catalog nor the docs describe yet
  is offered on Chat Completions, as OpenCode asks a model with no package of its
  own. A new free model is therefore offered the hour Zen lists it.
- A failed list keeps the last good list (the persisted one after a restart); with
  no list at all, the shipped bootstrap list is used. The bootstrap list is what
  the plugin publishes before a credential signs in, and it is the only part of
  the catalogue that needs a release to change.
- A `vendor/model` prefix is trimmed before lookup.
- `deepseek-v4.1-flash-free` is advertised with a 258,624-token context floor. The
  live catalog lists 200,000, but a request whose upstream usage was 258,624
  tokens was served on 2026-10-26. Reporting more than the vendor accepts routes
  requests that then fail, so the measured floor is used.
- A model the configuration excludes is never advertised, and a request that still
  names it (CPA may hold a cached list) is answered as an unknown model.

A request for a model that is not free now is answered with `400` and code
`unknown_model`, naming the free models. The code is the routing contract: CPA
reads it as this credential not having the model, so the request falls through to
another credential or provider instead of ending.

## Execution details

Every chat is sent to
`https://opencode.ai/inference/openai/v1/chat/completions` with exactly the
headers OpenCode 2.2.5 sends, in its order:

- `Authorization: Bearer public`, `Content-Type: application/json`.
- `User-Agent: opencode/latest/<version>/cli`, in the shape OpenCode 2.2.5 sent,
  where the version is the configured floor raised by OpenCode's updater
  (`https://opencode.ai/update/api/latest/cli/npm`, asked at most every six
  hours).
- `b3` and `traceparent`: one trace a conversation, a new span each request.
- `x-opencode-client: cli`.
- `x-opencode-project`: the client's own header when it sends one; else
  `sha1("directory:" + folder)` for the directory the agent's prompt states, or
  the repository's remote when this host can read that repository.
- `x-opencode-session`, `x-opencode-session-id`, `x-session-affinity` and
  `x-session-id`: one OpenCode-shaped session per conversation. A client that is
  OpenCode and sends a real session id keeps it; another client's session id is
  hashed into the same shape.

The request body is shaped the way Zen's free tier expects:

- The five core OpenCode tools (`edit`, `glob`, `grep`, `read`, `shell`) are added
  when the agent did not declare them, each with OpenCode's own first parameter and
  the description "Not available. Never call this tool."
- OpenCode's `<env>` block is added to the agent's own system message when no
  system message already carries one. The block names the working directory the
  agent's prompt states (`<cwd>`, a `Working directory:` line, or "the current
  working directory is `...`"), or this host's home folder when it names none. A
  literal `Working directory: unknown` is refused by Zen, so the plugin never
  sends one. Another agent's own `<env>` block stays and OpenCode's follows it.
- `stream` is always `true` upstream, and `stream_options.include_usage` is set.
  A non-streaming client is answered from that stream.

A tool call is held until its arguments are whole, then sent once under the agent's
own name. This is what makes name mapping possible across chunk boundaries. The
agent's own names win:

- A call of a served alias (`findings_report`) comes back as the agent's name
  (`ReportFindings`).
- A call of a name the agent does not have reaches the agent as the model called
  it; the agent answers with its own unknown-tool error and the model corrects the
  name.
- A call with no name is left out, and the reply's text says so. A turn whose every
  call was left out ends as an ordinary stop.

A stream Zen closes before its `[DONE]`:

- If the reply said something and no tool call is unfinished, it ends as an
  ordinary stop with a note saying it was cut short. Asking again goes on.
- If a tool call is unfinished, or nothing was said, it fails with a `502` naming
  the reply's id and how much arrived.

Token counting is not supported and answers `501`. Zen documents no token-count
endpoint, and a guess would misbill clients that trust it.

## Tool names Zen refuses

Zen's free tier answers some tool names with `429 FreeUsageLimitError` at once.
The same request with those tools under other names is served. The plugin sends
them under the names a bisection showed served, and maps a call back:

| Offered name | Sent to Zen | When |
| --- | --- | --- |
| any name containing `terminal` | the same name with `console` in its place | always |
| any name containing `run_code` | the same name with `exec_program` in its place | always |
| `ReportFindings` | `findings_report` | only beside `ScheduleWakeup` |
| `ScheduleWakeup` | `wakeup_schedule` | only beside `ReportFindings` |
| `subagent_fork` | `conversation_delegate` | always |
| `working_directory` | `workspace_directory` | only beside `read_image` |
| `CreateWorkflow` | `workflow_create` | only beside `GetWorkflowRun` |
| `GetWorkflowRun` | `workflow_inspect` | only beside `CreateWorkflow` |
| `AmendWorkflow` | `workflow_amend` | only beside `FillWorkflowHole` |
| `FillWorkflowHole` | `workflow_answer` | only beside `AmendWorkflow` |

A rule fires only when every name it lists is offered. The name is what Zen reads:
renaming the description or parameters does not help. A new finding is one more
entry in `go/tools.go`.

## Error handling

Upstream refusals are classified by Zen's own type and status, and the client sees
the mapped status:

| Upstream | Client sees |
| --- | --- |
| `401` | `401`, sign-in expired, sign in again |
| `402` | `402`, payment required |
| `403` with `FreeTierError` ("can only be used from within OpenCode") | `403` with a message saying to update the plugin: Zen refuses the exact User-Agent older releases sent |
| `429` with `FreeUsageLimitError` ("Free models work in OpenCode") | `429`, with Zen's message and the note that the shared allowance resets at 00:00 UTC |
| `404` | `404` with Zen's message |
| `5xx` | `502` |
| Transport failure, or an unavailable upstream stream | `503` |
| The caller closed the request before Zen answered (`context canceled`) | `499`, code `client_closed` |

A `FreeUsageLimitError` (`429`) is reported as credential-wide: the shared free
allowance covers every free model the public credential reaches, so they cool
together until Zen's reset. A plain `429` without that type stays model-scoped.
| Zen did not answer within the host's limit (`deadline exceeded`) | `504`, code `upstream_timeout` |

Other behaviour worth knowing:

- A request that never got an answer is reported for what it was: `499` when the
  caller went away, `504` when the host's time limit ran out, `503` otherwise. CPA
  reads `499` as the client's own cancellation and does not rest the credential for
  it.
- A streaming request is answered as soon as Zen's stream opens: the plugin spends
  at most two seconds looking for a refusal in the stream's first event, so a
  reasoning model's thinking is never spent before the client's headers go out.
- A refusal that arrives inside an `OK` stream keeps its real status, including a
  `429` with no HTTP status of its own: its type is what says which refusal it is.
- A stream event is forwarded as its bare data payload. The host's OpenAI handler
  adds the `data:` prefix, the blank lines and the terminal `[DONE]` marker itself,
  so the payload reaches the client exactly once whatever the host's own forwarding
  does.
- A non-streaming answer with no text, no reasoning and no tool call is reported as
  `502` rather than an empty `200`.
- Panics are caught at the plugin boundary and answered as a machine-coded error,
  and client-facing messages never carry credential bytes or internal paths.

## Quota

```text
GET  /v8/management/plugins/omc-opencode-zen-free/quota
POST /v8/management/plugins/omc-opencode-zen-free/quota
{"auth_index":"<auth index>"}
```

`GET` describes the provider: `opencode-zen-free`, display name
`OpenCode Zen Free`, reset not supported. `POST` answers the plan name
`OpenCode Zen Free` and **no buckets**.

Zen's free tier publishes no remaining-quota data: availability and rate limits
are set by Zen, and an account has no credit balance to read. The plugin reports
the plan rather than inventing a fraction. A client that needs to know whether an
allowance is spent learns it from the `429` on the next request.

## Troubleshooting

- **The provider is absent:** enable the global `plugins` switch and
  `plugins.configs.omc-opencode-zen-free.enabled`, then restart CPA if the library
  was replaced.
- **`unknown login entry: use opencode-zen-free`:** the login was started with
  another provider key. Start it from the `opencode-zen-free` entry.
- **The panel shows a link to the project README after starting a sign-in:** that
  is expected. The free tier needs no browser page; the link is the plugin's
  explanation because CPA's OAuth panel refuses an empty URL. The first poll
  completes the flow, so there is nothing to do on that page. If the panel still
  waits, poll the state once manually with
  `GET /v8/management/oauth/status?state=<state>`.
- **The panel briefly shows a callback box:** expected. Because the panel requires
  a URL, it classifies the keyless flow as a redirect until its first status poll
  (about three seconds later) returns the credential. Do not paste anything; the
  box disappears when that poll succeeds.
- **`sign-in expired` on poll:** the state is older than five minutes. Start a new
  sign-in; the credential itself does not expire.
- **No models appear:** the live list could not be read and neither a stored nor a
  bootstrap list was available, or the credential is not active. Check CPA logs for
  `model_error` and the credential status. A model served only on Responses or
  Anthropic Messages is not advertised on purpose.
- **`unknown_model` for a model that used to work:** Zen no longer gives it free.
  The message lists the current free models. If it is new, ask the model list again
  (the plugin re-reads at most once a minute).
- **`429` saying the free allowance is spent:** Zen's shared free tier allows a
  small number of requests a day per caller, and it resets at 00:00 UTC. The plugin
  cannot raise it, and signing in again changes nothing. Wait for the reset or use
  another provider.
- **`403` with `FreeTierError`:** Zen refused the request's identity. Update the
  plugin; a release that sends OpenCode's current headers is what Zen serves. If
  you pinned `opencode_version` to an old release, clear it and let the updater
  name the current one.
- **`499` with `context canceled`:** the caller stopped waiting, not Zen. The
  plugin reports it as `499` / `client_closed`, and CPA does not count it against
  the credential. Read the stream with a longer timeout, or stream it.
- **`502` saying the stream was cut short with a tool call unfinished:** Zen closed
  the stream mid-call. Ask again; the partial call is not forwarded, because half
  its arguments would be worse than an error.
- **`model_error` while listing models:** the catalog or OpenCode's docs could not
  be read (GitHub's raw host is unreachable from some networks). Zen's own list is
  still authoritative, and a listed `-free` model is offered without the catalog.
  The last good list is kept.
- **The sign-in card shows one account and signing in again does not add another:**
  correct. There is one public identity, so a second sign-in replaces the first.

## Verification notes

The Go test suite covers the state shape, the credential, model filtering and
metadata, request headers and body shaping, tool renaming and mapping, stream
aggregation and pumping, cut-short handling, error mapping, quota and the native
resource routes. All of it runs offline through a fake sender.

Not covered by the suite: a live Zen request, the updater's live answer, and the
models.dev/OpenCode documents as they stand today. Those are operator verification.
Record what you observe, and do not commit the response bodies or any auth file.
