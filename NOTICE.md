# Source note

The plugin's OpenCode Zen protocol support is ported from the MIT-licensed
`zen-free` package in `magpie-community/plugins`
(`packages/zen-free`, provider id `opencode-zen-free`). The Go implementation in
`go/` is independent code, but its wire behaviour follows that source and what
OpenCode itself sends:

- **Identity**: the shared public key (`Authorization: Bearer public`), OpenCode's
  `User-Agent` (`opencode/latest/<version>/cli`), `x-opencode-client: cli`, the
  `b3` and `traceparent` trace pair, the project id
  (`sha1("directory:" + folder)` or `sha1("git-remote:" + host/path)`), and the
  session id under `x-opencode-session`, `x-opencode-session-id`,
  `x-session-affinity` and `x-session-id`.
- **Version**: OpenCode's updater
  (`https://opencode.ai/update/api/latest/cli/npm`), asked at most every six hours,
  because Zen's free tier refuses the exact `User-Agent` older releases sent.
- **Environment block**: OpenCode 2.2.5's own `<env>` text, including its
  `Working directory:`, `Workspace root folder:`, `Is directory a git repo:`,
  `Platform:` and temp-folder lines. Zen answers the literal placeholder
  `Working directory: unknown` with a 429.
- **Endpoints**: the live list at `https://opencode.ai/inference/v1/models`, the
  chat endpoint at `https://opencode.ai/inference/openai/v1/chat/completions`, and
  models.dev's v2 catalog at `https://models.opencode.ai/api.json`. Each model's
  protocol comes from the live list's own `api` map, then the catalog's package
  name, then the shipped protocol table.
- **Model catalogue**: the live list's own metadata (name, limits, modalities,
  reasoning levels, price and the `api` map that names each model's protocol),
  the free/retired rules, the `-free` fallback, the `deepseek-v4.1-flash-free`
  context floor (258,624 tokens), the last-good list persisted beside the auth
  material, and the bootstrap tables that stand in only for a first run whose
  live read fails.
- **Tools**: the five core stand-ins (`edit`, `glob`, `grep`, `read`, `shell`) with
  OpenCode's first parameter each, the refused-name rules and their served names,
  and the case/separator fold that maps a model's call to the agent's tool.
- **Streams**: holding a tool call until its arguments are whole, ending a stream
  Zen closed before `[DONE]` as a cut-short stop, and forwarding a bare data
  payload because the host owns the SSE framing.

The findings behind the refused-name rules were made by bisecting real agent
requests against Zen, as recorded in the reference package and its issues:

- a name containing `terminal`, in any case, and `ReportFindings` offered beside
  `ScheduleWakeup` (plugins#624);
- `subagent_fork`, and `read_image` offered beside `working_directory`
  (plugins#632, Ebony-Vinyl/dsh-our-free-model#709);
- ZCode's `CreateWorkflow`/`GetWorkflowRun` and
  `AmendWorkflow`/`FillWorkflowHole` pairs (plugins#693);
- a name containing `run_code`, in any case (plugins#704).

OpenCode and OpenCode Zen belong to their owner. This project is not affiliated
with or endorsed by them.

## Marks

`assets/zen.png` is OpenCode's own mark, drawn from the glyph in its public
`logo-dark.svg`. It is the sign-in card.

`assets/logo.png` is this plugin's own face in a store: the OpenCode Zen mark on
the OMC sticker the ecosystem's plugins share. It identifies the plugin rather than
the service.

`go/assets/` holds the copies the plugin embeds and serves, and they must stay
byte-identical to the files under `assets/`; the test suite compares them.
