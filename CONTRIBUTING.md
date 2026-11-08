# Contributing to OMC OpenCode Zen Free

`omc-opencode-zen-free` is part of the [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa)
ecosystem. It provides OpenCode Zen's free models to CLIProxyAPI (CPA): keyless
public-credential sign-in, free-model discovery, OpenAI-compatible execution, tool
call preservation, and normalized quota metadata.

We welcome issues, feedback, and pull requests. Please adhere to the ecosystem
standards described below.

---

## Local Development

### Prerequisites

- Go 1.27.1+ with `CGO_ENABLED=1`
- `gcc` (or an equivalent C toolchain for c-shared builds)
- Docker (optional, for end-to-end testing with a local CPA container)

### Building and Testing

All dependencies are pre-cached after `go mod download`; tests run without network
access:

```bash
cd go
go mod download

# Run unit tests and static checks offline
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...

# Build the native shared library for your current platform
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-opencode-zen-free.so .
```

Use `.dylib` on macOS and `.dll` on Windows. `build/` is ignored by git.

---

## Commit Guidelines

This project follows the **Oh-My-CPA Conventional Commits** standard. Commits must
be atomic, focused, and independently verified.

### Subject Format

```text
<type>(<scope>): <imperative subject>
```

- **Types**:
  - `feat`: A new user-facing feature or configuration option.
  - `fix`: A bug fix or behavioral correction.
  - `refactor`: Code change that neither fixes a bug nor adds a feature.
  - `perf`: A code change that improves performance.
  - `test`: Adding missing tests or correcting existing tests.
  - `docs`: Documentation-only changes.
  - `chore`: Maintenance tasks, build tooling, or release preparations.

- **Scopes**:
  - `auth`: Sign-in, the public credential, and auth import/refresh.
  - `models`: Free-model filtering, bootstrap lists, and model info mapping.
  - `discovery`: The live list, models.dev catalog, OpenCode docs, and the
    OpenCode version updater.
  - `executor`: Request shaping, upstream headers, stream pumps, and SSE framing.
  - `tools`: Core tool injection, refused-name rules, and call-name mapping.
  - `quota`: Quota description and the no-buckets answer.
  - `config`: Plugin configuration fields, parsing, and validation.
  - `registry`: Release packaging and plugin store publication.
  - `readme`: Project overview, documentation links, and branding.
  - `tooling`: CI workflows, build scripts, and release packaging.
  - `release`: Release tags and version bumps.

### Commit Body Structure

Each non-trivial commit body should contain:

1. **Why**: The problem, bug symptom, or user need that prompted the change.
   Reference upstream issues or error messages where applicable.
2. **What changed**: The concrete architectural or behavioral changes made across
   the affected subsystems.
3. **Boundaries / Exclusions**: What was intentionally not changed or left out of
   scope.
4. **Verification**: A clear record of tests run, edge cases exercised, and live
   verification results. Say plainly when something was only tested offline.

#### Example

```text
fix(tools): hold a streamed tool call until its name is whole

Zen streams a tool name across chunks. Renaming a call as its fragments arrived
could map a half name and send the agent a tool it never declared, so a streamed
call was assembled before it was checked.

Buffer each choice's tool fragments until its finish_reason, map the complete
name once, and emit the call in that same frame. A call with no name is left out
and the reply says so.

Verification:
- Go test suite: TestStreamGuardHoldsToolCallsUntilFinish,
  TestExecuteStreamForwardsBareFramesAndMapsTools.
- Live verification: not run for this change; Zen's stream shape is exercised
  through the fake sender and the stream seams.
```

---

## Testing & Quality Standards

- **Lowest layer first**: Write regression tests at the lowest possible layer
  (`go/*_test.go`). A bug fix must include a test that reproduces the failure
  without the fix.
- **Offline & hermetic**: Unit tests must pass with `GOPROXY=off GOSUMDB=off`.
  Never make live network calls in unit tests; use `fakeSender` for HTTP and inject
  the stream seams (`readStreamChunk`, `emitStreamFrame`, `openHTTPStream`). A test
  that replaces the seams waits on `pumpFinished` before restoring them, so a pump
  that is still running never races the restore.
- **No flaky sleeps**: Use synchronous channels or dependency injection instead of
  `time.Sleep`.
- **No real accounts**: A live Zen request is operator verification, not a unit
  test. Record it in the commit body when you run one, and never commit response
  bodies or auth files.
- **One protocol, on purpose**: The executor declares and serves Chat Completions
  only. A change to model filtering must keep a Responses or Anthropic Messages
  model out of the advertised list: a chat request to one is refused.
- **Every tool rule needs evidence**: A refused-name rule exists because a real
  agent's request was bisected against Zen. State which request and which finding
  in the commit body; do not add a rule from suspicion.
- **CPA ABI safety**:
  - Catch panics and return `pluginabi.Error` with machine codes and HTTP status.
  - Do not expose credential bytes, auth directory paths, prompts, or upstream
    network errors to client-facing responses.
  - Model IDs for CPA routing must always be lowercase (`strings.ToLower(key)`),
    because CPA registry lookups are case-sensitive.
  - Zen's own model key goes upstream untouched: the lowercase id is the routing
    name, the catalogue key is what the endpoint expects.
  - A model this credential cannot serve is answered with code `unknown_model`,
    which is the routing contract; the message is for the person reading it.

---

## Documentation & Language Rules

- **Code & technical prose**: Code comments, commit messages, and primary technical
  documentation (`README.md`, `docs/USAGE.md`, `CONTRIBUTING.md`, `NOTICE.md`) are
  written in English.
- **Chinese translations**: User-facing documentation provides a Chinese mirror
  (`README.zh-CN.md`). Keep the English and Chinese versions in sync.
- **Domain vocabulary**: Preserve exact upstream and CPA terminology:
  - Provider identifier: `opencode-zen-free`
  - Plugin identifier: `omc-opencode-zen-free`
  - Credential: the shared public key, `public`
  - Service: OpenCode Zen, its free tier
  - Client identity: OpenCode's `User-Agent`, session id, project id, and
    `<env>` block
  - Refusals: `FreeTierError`, `FreeUsageLimitError`
  - Tool handling: core tools, refused names, served names, cut short
- **No unverified claims**: State which artifacts and behaviours were observed, and
  mark anything that was only exercised offline as such. Do not describe a live
  Zen response as observed unless you observed it.

---

## Security & Secrets

- **The public key is not a secret**: `public` is shared by every OpenCode install.
  Never add a personal Zen key to this plugin's credential; those belong to CPA's
  built-in OpenCode Zen provider.
- **Zero credential leaks**: Never commit auth files, personal keys, prompts, or
  upstream response bodies. Test fixtures must use synthetic ids or an ephemeral
  `t.TempDir()`.
- **No prompt logging**: The plugin sees agent prompts and tool arguments. Do not
  log them, and do not add a management route that echoes them.
- Report security issues privately through GitHub Security Advisories or by
  contacting the maintainer directly.
