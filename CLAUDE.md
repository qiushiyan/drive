# Working on drive

`drive` is the mechanics a driver uses to run Claude Code sessions in Rex for their owner: read where a session stands, send its next step, answer its prompts, be woken when its turn ends. The driver is a chat bot woken by webhook, or a Claude Code session. What to send, and when, is the driver's judgment, taught by the drive-session skill in the dotfiles repository (`~/dotfiles/claude/.claude/skills/drive-session/`: `SKILL.md`, `BOT.md`, `PLANLAB.md`).

- One maintainer's tool: commit on `main`, push at milestones, and keep changes proportional to demonstrated failures or requested features. Dependencies: the standard library, clikit and Kong, plus `BurntSushi/toml` for the configuration.
- Ship: `make check` (vet, `go fix -diff`, `go test -race`, gofmt), then `make install`. Live batches run `~/.local/bin/drive` and keep the old build until then, so install once a change is checked; the mini keeps its own until `twin sync drive`. One test: `go test -race -run TestADeniedPrompt ./internal/drive`.
- The command line runs on clikit: how help renders and how argv errors read are clikit's (`~/dev/clikit`). Help, results and errors an agent reads follow `~/dotfiles/claude/.claude/skills/prompt-engineering/SKILL.md`; library questions go to `~/.agents/skills/find-docs/SKILL.md`.

## How it fits together

`cmd/drive` declares the grammar and prints; every action is a method on `drive.Driver` (`internal/drive`), which reaches the world only through interfaces the tests replace: `rex.Terminal` (Rex's CLI), `session.Host` (Claude Code's session registry, processes and transcripts), `drive.Watches` (detached watch processes) and `progress.Source` (claude-steps, gh, and gwt for whether a change merged). What a result prints, as text and as `--json`, lives beside it in `internal/drive` (`text.go`, `json.go`). `internal/turn` reads a transcript into facts and does no I/O beyond the file; `internal/batch` keeps the batch's event log, its derived views and the per-session locks.

- **Acting on one session** (where a turn stands, views, the lock, delivery and its failures, what a driver reads and the command line, the fake world): `docs/sessions.md`
- **The batch record** (intents and settling, holds, the owner's records, metrics, wakes): `docs/batches.md`
- **Trying a change with a real bot** (drive and the skill end to end, a dogfood batch on the mini): `~/dotfiles/claude/.claude/skills/drive-session/DOGFOOD.md`

## Rules that keep it correct

- **Where a turn stands is `turnState`'s answer** (`internal/drive/state.go`): Claude Code's own registry status first, the transcript for how a turn ended, Rex for the kind of prompt. Rex alone misses what no hook reports, so code that needs the state calls it or `Observation.State()`, whether a turn is over `turnOf`, whether it waits on a prompt someone answers `State.onPrompt`, and what to run about it `nextFor`.
- **An action that changes a session runs under its owner's lock**: own the session, read it, check the view, act, record (an `action`: `onView` and `finish` in `internal/drive/action.go`). A refusal (exit 1) means nothing happened, so checks run before the first effect, and anything that fails after an effect returns `*Failed` saying what did happen. The command line's runner maps `*Refused` to 1 and a login `*Failed` to 4 for every command.
- **A session an action touched is watched.** Reach a session's keyboard only through the action's `typeInto` and `press`: its `finish` (`internal/drive/action.go`) arms the wake of whatever they reached, so a chat driver always hears back.
- **The log is the only state.** An action appends events; what is true now is derived in `internal/batch/events.go`. A send or an open records its intent before its first effect.
- **Messages are read by an agent.** A `Refused` or `Failed` that leaves something to do says it in `Next`, every command in it built by `d.run` (`internal/drive/command.go`), which quotes it, puts free text last behind `--`, and names `--batch` when the batch is not the current one. `TestEveryCommandAMessageNamesExists` fails on a message naming a command or flag the grammar lacks. Usage text, refusals and wake text are product.
- **Contracts other repositories depend on:** every command line, output line, `next:` and exit status the drive-session skill teaches, and the `--json` documents, pinned byte for byte by `TestContracts` (`cmd/drive/testdata/contract`, `DRIVE_CONTRACT_UPDATE=1` rewrites them), with every help page pinned by `TestHelp` (`make golden`). A change to either lands with the drive-session skill updated to match, in the dotfiles repository, so the bot is never taught a command that refuses. The two divide the work: a message's `next:` carries the command to run now, `--help` the words and flags, the skill the judgment of what to send and when; a mechanism a message states is not explained again in the skill.
- **An installed drive still runs what an older one started.** A watch re-execs the binary by path, so `upgrade` (`cmd/drive/main.go`) rewrites the exact command lines older drives wrote; `TestOlderDrivesCommandLinesStillRun` holds them.
- **The fake behaves like the real thing** (`internal/testkit`). A behaviour seen in a real session or transcript goes into the fake first, then into a test that goes red on it before the fix.
- **Tests never reach live state**: `TestMain` sandboxes `HOME`, `DRIVE_STATE_DIR` and `DRIVE_CONFIG_DIR`; Rex tests start a private server; the contract runs on a fixed clock under a home of its own.
- **The repository is public**: fixtures use `example.com` accounts and invented names.

## Docs and commits

Docs follow `~/dotfiles/docs/documentation-standards.md`. This file is the hot path; `docs/*.md` are the live design docs, routed from § How it fits together; paths are written from the repository root. Commit messages read `drive: <what is true now>` (or `docs:`), with review round coordinates such as `(review r1, F3)` on fixes from a review.
