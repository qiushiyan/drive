# Acting on a session

`drive` is the mechanics of driving Claude Code sessions in Rex for a driver standing in for their owner: a chat bot woken by webhook, or a Claude Code session. It reads where a session stands, sends its next step, answers its prompts and says when its turn ends. What to send, and when, stays the driver's judgment, which the drive-session skill in the dotfiles repository teaches; drive's side of that division is the `next:` each message ends with, the command to run on the session as it stands. This doc is how one action on one session works; `docs/batches.md` is the record those actions keep and the wakes that bring the driver back.

## Where a turn stands

Every reader asks one function, `turnState` in `internal/drive/state.go`, and gets one `State`, the word drive prints: working, done, idle, interrupted, error, blocked:permission, blocked:question, blocked:auth. A second, local answer to that question is how drive goes wrong, so a new reader of session state calls it, `turnOf` for whether the turn is over, `State.onPrompt` for whether it waits on a prompt someone answers (a rejected login reads blocked, but only a relaunch moves it), and `nextFor` for what to run about it.

Its sources each answer what only they know:

- **Claude Code's registry** (`<config>/sessions/<pid>.json`, read by `internal/session`): `status` is busy, waiting (with what it waits for), idle, or shell, idle at its prompt with a background job running, and changes within a fraction of a second of the session itself. It is the answer to whether the session works, waits or is idle.
- **The transcript** (`internal/turn`): how an idle session's turn ended — answered, interrupted (a deny or Ctrl+C), or an API error, a rejected login reading `blocked:auth`.
- **Rex's record**, from Claude Code's hooks through `rex-agent` (dotfiles): what kind of prompt a waiting session shows.

Rex alone is not enough: no hook reports a deny, an interrupt, a typed `/compact`, a queued turn starting after the turn before it stopped, a permission prompt in its first seconds, an approved command before its PostToolUse, a held message from another session (Rex reads blocked while the session sits idle at its input box), or an API error where the StopFailure hook is not registered. A Claude Code that keeps no registry status falls back to folding the transcript into Rex's record.

A session's background jobs are the Bash shells it left running, each known by the command it runs (`session.Shell`): `status`, `read` and the wake list them, and wherever a job bears on what comes next, a turn that ended while it runs or a hold, interrupt or close it stands in the way of, the `next:` gives the step that has the session stop it, since the session owns its jobs and a driver only reads and sends. A job the session detached itself, `nohup … &` inside a Bash call, is invisible to Claude Code's registry and to drive alike: Claude Code runs each call in a process group of its own with no terminal, so once that call's shell exits nothing ties the job to the session that started it.

A session whose process has gone, or whose pid another process now holds, is no session: `Locate` checks the registry's `procStart`, which Claude Code writes as `ps` prints `lstart` in UTC, against the live process, and treats a start it cannot read as a match rather than hide a live session. An API error belongs to the process that wrote it: accounts share one transcript directory (`~/.claude-accounts/*/projects` links to `~/.claude/projects`), so a session resumed on another account reads the error that ended the one it replaced, and an error older than the process's start does not count (`liveError`). Its transcript outlives both the process and the block, so `read` of a session closed or gone reads that (`Host.Transcript`), saying which, with no view, since nothing can act on it: a report run after a reset still has the recaps.

## Views

Every action on a session is decided on a view, `T1@3fa91c07`: the session, then a short hash of its newest prompt followed by one of its newest answer (`internal/turn/view.go`). `read`, `wait`, `status` and every wake print one; `send`, `respond`, `park` and `close` take one and refuse once the session has moved past it. That is what keeps a step decided on an old turn from landing on a new one when several drivers act at once, and why the refusal names the prompt the session moved on and, when drive sent it, which step and when (`checkView`): a run refused over another run's step learns the session did not move by itself.

A working session's view changes as it writes, so a send is decided on a turn that has ended. `--queue` sends to a working session and checks only the prompt half: nothing new was sent to it since.

## One driver at a time

An action owns its session from the read it is decided on to the record of how it went (`action` in `internal/drive/action.go`): an exclusive lock per block under the state directory's `locks/` (`Store.Own` in `internal/batch/lock.go`), and the batch it runs in, fixed for the whole run, so what it records, the wake it arms and every message it builds belong to that batch whatever its driver starts meanwhile; an `Observation` keeps the batch it was read in for the same reason. A second driver is refused with what the first is doing, never queued behind it, because whatever it decided it decided on what the first is changing. `open` owns the label it opens the same way, then the block Rex opened for it. Checking a hold, or listing what is in flight, probes another driver's lock for an instant; `Own` retries briefly before refusing, which is what tells a probe from a driver that holds the lock for seconds.

A session belongs to the live batch that brought it in last (`docs/batches.md` § Drivers and their batches), so a driver carries its sessions over into its next batch and no other driver acts on them: the lock keeps two drivers from acting on a session at once, and under it each action refuses a session another live batch drives (`ownedHere`, from `take` and `Watch`), before anything reaches it. `read` and `status` name the batch and driver that drive such a session, `read` with no `next:`.

`onView` takes the session a view names, settles what an earlier driver left, reads it and checks the view. `finish` ends the action: it records the refusal the action ended on, once and while the session is still owned, so the log has it before another driver acts, then arms the wake of a session the action touched (`docs/batches.md` § Wakes). An action reaches the session's keyboard only through its `typeInto` and `press`, which is how `finish` knows it touched the session.

## Delivering a step

A step is a short typed line (a slash command too), a file the session is told to read, or one of the owner's TabType snippets (`internal/snippet`). Its path through `internal/drive/send.go`:

1. **Plan, with no effect.** The step is checked whole first: a typed line is one trimmed line of at most 500 bytes with no control character and no leading `!` or `#`, since Claude Code keeps only the end of long typing and takes those as keys, a shell command or a memory; a skill that starts only when typed is never sent inside a file; the notice exists. A refusal (exit 1) therefore always means nothing reached the session.
2. **Admit.** Under the batch's lock: the hold is free, the session joins the batch if it was outside it, the file a step sends is saved under the batch's `sends/` (never overwritten), and the step's intent is recorded before anything is typed, the notice's beside it under its own.
3. **Type and confirm.** Only into an input box that reads empty or holds Claude Code's suggestion: nothing is typed into a prompt, whose Enter would answer it, or into a screen with no box. The line is typed, Enter pressed, and arrival confirmed from the transcript by a prompt the session did not have before; a prompt already there, however recent and alike, is never taken for this one. The first seconds of the new turn are watched for an API error, a rejected login most often.
4. **Record** the outcome: `arrived`, `queued`, `recovered`, `garbled`, `other` (it received something else, and the step reached it) or `unknown`.

The first step a session gets in a batch carries the notice (`~/.agents/skills/drive-session/NOTICE.md` by default), and so does each step after it until a notice has reached the session. A file step carries it at its head; a typed step goes after the notice, which is sent alone in its own turn. A session resumed with `--resume` is spared it only if this batch saw that session read it.

## When delivery goes wrong

- **The line's start is lost in typing:** it comes back as a line break where the start was. The garbled turn is interrupted and the line typed again, prose behind eight spaces that absorb the loss, a slash command as it is, since one behind spaces is only words. Only while that turn is the one running, as `turnState` reads it, since Rex can still be on the turn before: Ctrl+C stops whatever turn runs, so a garbled turn that already ended, or a garbled line still queued behind the session's own work, is reported `garbled` for the driver to read.
- **The session received something else:** whitespace or a typed line Claude Code recorded its own way. It is `other`, a send that reached the session, so its hold stands, and the message says to read it before interrupting.
- **Nothing new reaches the transcript in time:** the send is `unknown`, and the message says whether the line still sits in the input box. A line left there blocks the next send until someone clears it, since it may be the owner's.
- **Rex refuses the Enter after the line is typed:** a failure, the line left in the box, whose `next:` clears it before the step goes again.
- **The login was rejected:** a send to a session already showing it is refused; one whose step the rejection answers fails with exit 4; a wake for it ends the same way. Each prints the relaunch, close on the view the session stands at and open again on another account with `--resume` and the step it was last sent. The account offered is one this batch has not seen rejected, the primary among them under the launcher name `primary_name` gives (`internal/config`); with none left, the message says to tell the owner.

## What a driver reads

Every message drive prints is read by an agent, so drive's half of the division is exact: a result, refusal or failure that leaves something to do ends with `next:`, prose around whole commands that run as written on the session as it stands. Each command in it is built by `run` (`internal/drive/command.go`), which quotes every word, puts the driver's free text after the flags and behind `--` when it starts with a dash, since drive would read it as a flag, and names the batch the action ran in, since the line may run after its driver has started another batch, and a driver would otherwise have to name itself. A command written into a message by hand skips all of that; `TestEveryCommandAMessageNamesExists` catches it, and any command or flag the grammar lacks.

The exit status says what happened: 0 done, 1 refused with nothing done, 2 a command line drive could not use, 3 tried and did not end as asked, 4 a rejected login, whose `next:` is the relaunch. One runner in `cmd/drive/main.go` maps `*Refused` and a login `*Failed` for every command, so no command can let a refusal exit as a failure. Checks the grammar's tags cannot state run in each command's `Validate`, before the configuration loads, so help and usage errors work whatever state it is in.

The grammar runs on clikit (`~/dev/clikit`): help is rendered from each command's declaration, and how an argv error reads and which errors are usage errors are clikit's, so a change there is a clikit release and a `go.mod` bump, reviewed through the help pages `TestHelp` pins (`make golden`). What a driver reads is a contract with the drive-session skill: `TestContracts` pins every command line the skill teaches, and each refusal and failure, byte for byte (`cmd/drive/testdata/contract`), and `--json` prints one document per command (`internal/drive/json.go`), each key named on that command's help page.

## Testing

The suite drives a fake Rex and fake Claude Code sessions (`internal/testkit`) that write real transcript files and keep a registry status. A bug hides wherever the fake is kinder than the real thing, so it models what real sessions were seen to do:

- **The registry moves with the session, Rex only with a hook:** Rex lags a permission prompt (`AskQuietly`), reads blocked through an approved slow command (`SlowTool`, `ToolDone`), reads done through a queued turn, hears nothing of a `/compact` (`SlowCompact`, `Compacted`), a deny or an interrupt, and can be told of a held message the session does not show (`HoldMessage`); a job left running reads `shell` (`Job`); a crash leaves Rex's record behind (`Crash`).
- **A slash command typed while busy** is queued without its text; one behind spaces is words.
- **A resume** carries on in the shared transcript, on the account the launch names.
- **A rejected login** answers with an API error row and no Stop.
- **Typing** can lose a line's start to a line break, Enter can be lost, a send can be caught mid-typing (`TypeGate`), a prompt draws its options where the input box would be, and Rex can fail a read, a key or its block list, as it does while its server is down (`StatusFails`, `KeyFails`, `BlocksFail`).
- **A watch name** already running is refused, and one watch can fail to start while others do (`Watches.Refuse`).
- **A closed block** leaves its transcript behind, as the real one does.

`TestMain` points `HOME`, `DRIVE_STATE_DIR` and `DRIVE_CONFIG_DIR` at a temporary directory, so no test reaches live state. The command tests run drive as `main` does, through clikit's App with the driver's wiring bound to the fake world (`fake` in `cmd/drive/main_test.go`). The contract runs on a fixed clock (`NewClockAt`) under a home of its own, and each fake world counts its own transcript entries, so a view's hash, and the output that prints it, never depends on which tests ran first.
