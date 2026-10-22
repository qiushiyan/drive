# Acting on a session

`drive` is the mechanics of driving Claude Code sessions in Rex for a driver standing in for their owner: a chat bot woken by webhook, or a Claude Code session. It reads where a session stands, sends its next step, answers its prompts and says when its turn ends. What to send, and when, stays the driver's judgment; the drive-session skill in the dotfiles repository carries the owner's intent and preferences. This doc is how one action on one session works; `docs/batches.md` is the record those actions keep and the wakes that bring the driver back.

## Where a turn stands

Every reader asks one function, `turnState` in `internal/drive/driver.go`, and gets one word: working, done, idle, interrupted, error, blocked:permission, blocked:question, blocked:auth. A second, local answer to that question is how drive goes wrong, so a new reader of session state calls it rather than reading Rex's record directly.

Two sources feed it. Rex holds what Claude Code's hooks report through `rex-agent` (dotfiles): a prompt submitted, a tool run, a prompt shown, a turn stopped, an API failure. The transcript (`internal/turn`) covers what no hook reports:

- **An interrupt or a denied prompt:** no hook fires, so Rex keeps reading working or blocked; the transcript's `[Request interrupted by user…]` with nothing after it reads `interrupted`.
- **An API error in a session started before the StopFailure hook:** Rex keeps reading working; the error row reads `error`, or `blocked:auth` for a rejected login.
- **A local command such as `/compact`:** it answers with its output, not an assistant entry, and that output counts as the answer.

An API error belongs to the process that wrote it. Accounts share one transcript directory (`~/.claude-accounts/*/projects` links to `~/.claude/projects`), so a session resumed on another account reads the error that ended the process it replaced; an error older than Rex's record of the new process starting does not count (`liveError`).

## Views

Every action on a session is decided on a view, `T1@3fa91c07`: the session, then a short hash of its newest prompt followed by one of its newest answer (`internal/turn/view.go`). `read`, `wait`, `status` and every wake print one; `send`, `respond`, `park` and `close` take one and refuse once the session has moved past it. That is what keeps a step decided on an old turn from landing on a new one when several drivers act at once.

A working session's view changes as it writes, so a send is decided on a turn that has ended. `--queue` sends to a working session and checks only the prompt half: nothing new was sent to it since.

## One driver at a time

An action owns its session from the read it is decided on to the record of how it went: an exclusive lock per block under the state directory's `locks/` (`Store.Own` in `internal/batch/lock.go`). A second driver is refused with what the first is doing, never queued behind it, because whatever it decided it decided on what the first is changing. `open` owns the label it opens under the same way.

Checking a hold, or listing what is in flight, probes another driver's lock for an instant; `Own` retries briefly before refusing, which is what tells a probe from a driver that holds the lock for seconds.

## Delivering a step

A step is a short typed line (a slash command too), a file the session is told to read, or one of the owner's TabType snippets (`internal/snippet`). Its path through `internal/drive/send.go`:

1. **Plan, with no effect.** The step is checked whole first: a typed line is one line of at most 500 bytes, since Claude Code keeps only the end of long typing; a skill that starts only when typed is never sent inside a file; the notice exists. A refusal (exit 1) therefore always means nothing reached the session.
2. **Admit.** Under the batch's lock: the hold is free, the session joins the batch if it was outside it, the file a step sends is saved under the batch's `sends/` (never overwritten), and an intent is recorded before anything is typed.
3. **Type and confirm.** The line is typed, Enter pressed, and arrival confirmed from the transcript by a prompt the session did not have before; a prompt already there, however recent and alike, is never taken for this one. The first seconds of the new turn are watched for an API error, a rejected login most often.
4. **Record** the outcome: `arrived`, `queued`, `recovered`, `garbled` or `unknown`; in a batch that wakes its driver, arm the session's wake.

The first step a session gets in a batch carries the notice (`~/.agents/skills/drive-session/NOTICE.md` by default). A file step carries it at its head; a typed step goes after the notice, which is sent alone in its own turn. A session resumed with `--resume` read the notice in the conversation it resumes and does not get it again.

Nothing is typed into a prompt: Enter there would answer it, so a session on a permission or question prompt takes no step, queued or not, until `respond` answers it.

## When delivery goes wrong

- **The line's start is lost in typing:** the garbled turn is interrupted and the line typed again behind eight spaces, which absorb the loss and change nothing the session reads. Only while that turn is the one running: Ctrl+C stops whatever turn runs, so a garbled turn that already ended, or a garbled line still queued behind the session's own work, is reported `garbled` for the driver to read.
- **Nothing new reaches the transcript in time:** the send is `unknown`, and the message says whether the line still sits in the input box. A line left there blocks the next send until someone clears it, since it may be the owner's.
- **The login was rejected:** a send to a session already showing it is refused; one whose step the rejection answers fails with exit 4. Either message prints the relaunch: close the block and open the session again on another account with `--resume` and the step. The account offered is one this batch has not seen rejected; with none left, the message says to tell the owner.

## Testing

The suite drives a fake Rex and fake Claude Code sessions (`internal/testkit`) that write real transcript files. A bug hides wherever the fake is kinder than the real thing, so it models what real sessions were seen to do:

- **A slash command typed while busy** is queued without its text.
- **Escape and Ctrl+C** write the transcript rows a deny and an interrupt write, and leave Rex's record as it was.
- **A resume** carries on in the shared transcript, on the account the launch names.
- **A rejected login** answers with an API error row and no Stop.
- **Typing** can lose a line's start, Enter can be lost, and a send can be caught mid-typing (`TypeGate`).
- **A watch name** already running is refused.

`TestMain` points `HOME`, `DRIVE_STATE_DIR` and `DRIVE_CONFIG_DIR` at a temporary directory, so no test reaches live state. `cmd/drive/main_test.go` runs the command itself against the fake world through `newDriver`.
