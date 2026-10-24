# The batch record and wakes

A batch is the sessions one driver drives together, usually a night's work, and the record of everything done to them. It is what a driver with no memory between runs (the chat bot is woken fresh each time) reads to know where things stand, and what the owner reads in the morning. `docs/sessions.md` is how each action on a session works; this doc is what those actions leave behind and how the driver is brought back.

## The record

Each batch is a directory under the state directory (`~/.local/state/drive/batches/<name>/`; `DRIVE_STATE_DIR` moves it):

- **`batch.json`:** settings fixed when the batch starts: how its driver is woken and the webhook that wakes it, the Rex session new blocks open in (started with the batch if none runs), the zone times are shown in, the commands run after each wake.
- **`events.jsonl`:** the append-only log, one event per line, written whole.
- **`sends/`:** each file a session was told to read, saved before it was told and never overwritten, so what a session read stays as it was.
- **`watches.log`:** what each watch printed, so one that ended early says why.

What is true now is derived from the log and never written separately (`internal/batch/events.go`): which sessions belong to the batch and under which label, which are parked and why, who holds what and who waits for it, which asks are open. A fact with a second home drifts from the first.

`current` points at the batch actions work in; `--batch NAME` pins another. A watch carries its batch by name, so a wake from an old batch never acts in a new one, and `batch new` stops the previous batch's watches and timers. A new batch starts with no grants: authority the owner handed over lives as long as the batch it was given in.

## Intents and settling

A send or an open records an intent under the batch's lock before its first effect, and its outcome after; a first step that goes behind the notice records the notice's intent beside its own, so the step stays unsettled until it goes. An intent with no outcome means its driver still owns the session (`docs/sessions.md` § One driver at a time) or has gone, often a chat driver whose shell timed out mid-send. The next driver to send, respond, park or close on the session settles it from the transcript: its own line arriving after the intent makes it `arrived`; another prompt makes it a failure naming what arrived instead; none, a failure that never reached the session. Time never decides it, since a legitimate send can take minutes to confirm. `status` lists an unsettled intent with the driver acting on it, or as left by one that has gone.

`open` records its block the moment Rex opens it, before Claude Code has started, so a driver that ends mid-launch leaves the block on record; an open whose driver left no block on record is settled by looking for a block under its label.

## Holds

A hold is a name for something only one session may do at a time: a release, a closeout into a shared checkout, commits into a checkout others edit. `send --hold NAME` takes it when the send is admitted, under the batch's lock, so no other send can take it meanwhile. It stays held while that send is under way, while its holder works, a later step included, since a follow-up to a closeout is still the closeout, while it waits on a prompt, and while background jobs it started run: a step such as merge-when-ready ends its turn waiting on checks in a background job, and the merge comes after. While the holder's session cannot be read the hold stands; a holder that is gone or closed frees it. A refusal on a hold says which keeps it, and its `next:` what frees it from there (`holdNext` in `internal/drive/send.go`): the holder's turn ending, its prompt answered, or its job ending, with the step that has the holder stop a job that should have ended.

`open --hold` checks its hold before it opens a block. A send or an open refused on a hold is recorded with its step, so `status` lists each waiter and what it was refused (`Waiting`), and the holder's wake names each with the send or open to run once the hold is free; sending to it clears that. A parked waiter wakes no one of itself: `drive watch --due` arms it once its hold is free.

## The owner's records

Besides what drivers did, the log keeps what the owner said that grants or decides something (`drive record`):

- **Grant:** authority handed over, in the owner's words and with what it covers.
- **Ask:** a question put to the owner, with the recommendation and what happens while nobody answers.
- **Answer:** whether the owner took the recommendation, amended it, handed it to a fresh model, or it stopped mattering, and whether they needed it explained first.
- **Note:** what a driver wants the next run to know.

`drive metrics` measures a batch from the record, in the terms drivers are compared on: owner wait per merged change, an open ask counted to the batch's end; how often a recommendation was taken; the owner's snippets against typed slash commands and the driver's own words; deliveries that ended garbled, other or unknown; refusals by kind; wakes no action followed; and turns left unwatched, a send or respond that reached a session with no wake, park or close after it, which is the measure of a lost wake. A merged change is every pull request a session linked, counted once however many sessions name it, or, for a session with none, its branch once it merged into the trunk, as a personal tool's closeout merges it (`mergedBranch` in `internal/drive/records.go`). That branch is the newest one other than the trunk among those the transcript names and those checked out where `claude-steps` saw the session commit, since a session that worked in a worktree through its shell names only the trunk; gwt judges it. A closed session's verdict is the one read at its close, because the cleanup after a close removes the branch. A measure whose progress lookups failed says so.

## Wakes

A batch started with `--webhook DRIVER` drives for a chat bot that runs only when woken, so the rule is that a session an action touched is watched: whatever a `send`, `open` or `respond` typed or pressed into a session arms its wake when the action's lease ends, whatever came of it (`docs/sessions.md` § One driver at a time). A refused action touched nothing and arms nothing; its `next:` says what to do instead, which for a session left waiting is to park it. `drive watch` arms one by hand and `drive watch --sleep` a timer, such as the veto window on a plan. A batch with `--wake none` has a driver that waits itself, with `drive wait`.

A watch is `drive wait LABEL --notify` running as a process in a session of its own, with a pidfile under the state directory's `watches/`, named for its batch and session, that holds its pid, the process's start and its command line (`internal/drive/watches.go`); it counts as running only while they all match, so a reused pid is never taken for it. It is not a tmux session: a chat driver's shell commands end, and a tmux server exits taking every session in it along, so a watch outlives both. The wait ends when the turn has stayed over, or on a prompt, for a minute; at once on an API error or an interrupt; ten minutes after a turn that ended while shells it started still run, since such a turn usually resumes when they finish; and as gone after three unreadable looks in a row, one being Rex busy for a moment.

Then the watch retires, removing its pidfile, before it posts: a step sent while the post is under way arms a fresh watch rather than counting on one that is about to exit. It records `posting` before its first try, since retries can take ten minutes, then every attempt, so an empty log line is never taken for a wake that arrived. The post carries the session's label, how its turn ended, the end of its reply, the view to act on, a `next:` line where the ending decides what to do, and the sessions waiting on a hold it held. Only a wake that was posted runs the batch's after-wake commands, such as archiving the chat it went to, each killed after two minutes; their failure is noted without failing the wake.

A watch that ends without posting records why, a kill from anyone but drive included: drive's own stops (`park`, `close`, `drive watch` restarting a stuck watch) remove the pidfile before they signal, and a signalled watch whose pidfile is still there records the kill.

`status` reads each session's wake liveness first, the process table for whether its watch runs and the log for what came of the last one: armed; stuck, a watch still running fifteen minutes past the later of the turn's end and its arming; posting; woken (nothing done since), a wake that wants a decision; none, or ended without a wake; parked. `drive watch --due` is a chat driver's watchdog in one command: it arms every session that has no wake and needs one — never watched, acted on with no wake armed, a watch that died or is stuck, a session that moved on by itself since its last wake (a usage limit's reset continues a turn so), a parked waiter whose hold is free — and posts once more, ten minutes on, a wake nothing was done about, never twice for one view.

Each webhook batch names its driver's webhook, a file under `~/.config/drive/webhooks/`, mode 0600, with a `URL=` and an `AUTH=` line (`internal/config/config.go`), kept outside the repository and the batch. Two drivers on one machine so never wake each other, and a batch whose webhook holds no URL neither starts nor arms a wake or a timer, since every wake of it would fail unseen.
