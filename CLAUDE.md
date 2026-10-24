# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`drive` is the mechanics a driver uses to run Claude Code sessions in Rex for their owner: read where a session stands, send its next step, answer its prompts, be woken when its turn ends. The driver is a chat bot woken by webhook, or a Claude Code session. What to send, and when, is the driver's judgment, taught by the drive-session skill in the dotfiles repository (`~/dotfiles/claude/.claude/skills/drive-session/`: `SKILL.md`, `BOT.md`, `PLANLAB.md`).

## Commands

```bash
make check                                    # go test -race ./..., go vet, gofmt; run before every commit
go test -race -run TestADeniedPrompt ./internal/drive   # one test
make install                                  # builds into ~/.local/bin/drive, the binary live drivers run
```

`make install` swaps the tool under any batch running now; install once a change is checked.

## How it fits together

`cmd/drive` parses arguments and renders; every action is a method on `drive.Driver` (`internal/drive`), which reaches the world only through interfaces the tests replace: `rex.Terminal` (Rex's CLI), `session.Host` (Claude Code's session registry, processes and transcripts), `drive.Watches` (detached watch processes) and `progress.Source` (claude-steps, gh, and gwt for whether a branch merged). `internal/turn` reads a transcript into facts and does no I/O beyond the file; `internal/batch` keeps the batch's event log, its derived views and the per-session locks.

- **Acting on one session** (where a turn stands, views, the lock, delivery and its failures, the fake world): `docs/sessions.md`
- **The batch record** (intents and settling, holds, the owner's records, metrics, wakes): `docs/batches.md`

## Rules that keep it correct

- **Where a turn stands is `turnState`'s answer** (`internal/drive/driver.go`): Claude Code's own registry status first, the transcript for how a turn ended, Rex for the kind of prompt. Rex alone misses what no hook reports, so code that needs the state calls it or `Observation.State()`, whether a turn is over `turnOf`, and what to run about it `nextFor`.
- **An action that changes a session runs under its owner's lock**: own the session, read it, check the view, act, record (`ownView` in `internal/drive/respond.go`, `Send` in `internal/drive/send.go`). A refusal (exit 1) means nothing happened, so checks run before the first effect, and anything that fails after an effect returns `*Failed` saying what did happen.
- **A session an action touched is watched.** Reach a session's keyboard only through `typeInto` and `press`: the action's lease (`own` in `internal/drive/driver.go`) arms the wake of whatever they reached when it ends, so a chat driver always hears back.
- **The log is the only state.** An action appends events; what is true now is derived in `internal/batch/events.go`. A send or an open records its intent before its first effect.
- **Messages are read by an agent.** A `Refused` or `Failed` that leaves something to do says it in `Next`, as a command that runs as written, with a view wherever the command takes one; usage text, refusals and wake text are product.
- **The fake behaves like the real thing** (`internal/testkit`). A behaviour seen in a real session or transcript goes into the fake first, then into a test that goes red on it before the fix.
- **Tests never reach live state**: `TestMain` sandboxes `HOME`, `DRIVE_STATE_DIR` and `DRIVE_CONFIG_DIR`; Rex tests start a private server.
- **A change to the CLI's interface or messages lands with the drive-session skill updated to match**, in the dotfiles repository, so the bot is never taught a command that refuses. The two divide the work: a message's `next:` carries the command to run now, `--help` the words and flags, the skill the judgment of what to send and when; a mechanism a message states is not explained again in the skill.
- **The repository is public**: fixtures use `example.com` accounts and invented names.

## Docs and commits

Docs follow `~/dotfiles/docs/documentation-standards.md`. This file is the hot path; `docs/*.md` are the live design docs, routed from § How it fits together; paths are written from the repository root. Commit messages read `drive: <what is true now>` (or `docs:`), with review round coordinates such as `(review r1, F3)` on fixes from a review.
