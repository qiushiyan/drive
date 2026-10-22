# The batch record and wakes

A batch is the sessions one driver drives together, usually a night's work, and the record of everything done to them. It is what a driver with no memory between runs (the chat bot is woken fresh each time) reads to know where things stand, and what the owner reads in the morning. `docs/sessions.md` is how each action on a session works; this doc is what those actions leave behind and how the driver is brought back.

## The record

Each batch is a directory under the state directory (`~/.local/state/drive/batches/<name>/`; `DRIVE_STATE_DIR` moves it):

- **`batch.json`:** settings fixed when the batch starts: how its driver is woken, the Rex session new blocks open in, the zone times are shown in, the commands run after each wake.
- **`events.jsonl`:** the append-only log, one event per line, written whole.
- **`sends/`:** each file a session was told to read, saved before it was told and never overwritten, so what a session read stays as it was.
- **`watches.log`:** what each watch printed, so one that ended early says why.

What is true now is derived from the log and never written separately (`internal/batch/events.go`): which sessions belong to the batch and under which label, which are parked and why, who holds what and who waits for it, which asks are open. A fact with a second home drifts from the first.

`current` points at the batch actions work in; `--batch NAME` pins another. A watch carries its batch by name, so a wake from an old batch never acts in a new one. A new batch starts with no grants: authority the owner handed over lives as long as the batch it was given in.

## Intents and settling

A send or an open records an intent under the batch's lock before its first effect, and its outcome after. An intent with no outcome means its driver still owns the session (`docs/sessions.md` § One driver at a time) or has gone. The next driver to own the session settles it from the transcript: a prompt that arrived after the intent makes it `arrived`, none makes it a failure that never reached the session. Time never decides it, since a legitimate send can wait ten minutes for `/compact` to record itself. `status` lists an unsettled intent with the driver acting on it, or as left by one that has gone.

`open` records its block the moment Rex opens it, before Claude Code has started, so a driver that ends mid-launch leaves the block on record; an open whose driver left no block on record is settled by looking for a block under its label.

## Holds

A hold is a name for something only one session may do at a time: a release, a closeout into a shared checkout, commits into a checkout others edit. `send --hold NAME` takes it when the send is admitted, under the batch's lock, so no other send can take it meanwhile. It stays held while that send is under way, while the turn it started runs, and while background shells that turn started run: a step such as merge-when-ready ends its turn waiting on checks in a background shell, and the merge comes after. While the holder's session cannot be read the hold stands; a holder that is gone or closed frees it.

A send refused on a hold is recorded with its step, so `status` and the holder's wake name each session waiting and what it was refused (`Waiting`); sending to it clears that.

## The owner's records

Besides what drivers did, the log keeps what the owner said that grants or decides something (`drive record`):

- **Grant:** authority handed over, in the owner's words and with what it covers.
- **Ask:** a question put to the owner, with the recommendation and what happens while nobody answers.
- **Answer:** whether the owner took the recommendation, amended it, handed it to a fresh model, or it stopped mattering, and whether they needed it explained first.
- **Note:** what a driver wants the next run to know.

`drive metrics` measures a batch from the record, in the terms drivers are compared on: owner wait per merged change, how often a recommendation was taken, the owner's snippets against the driver's own words, refusals by kind, deliveries that ended unknown. A merged change counts once however many sessions name it, and a measure whose progress lookups failed says so.

## Wakes

A batch started with `--wake webhook` drives for a chat bot that runs only when woken. Every `send` and `open` arms a watch for its session; `drive watch` arms one by hand, and `drive watch --sleep` arms a timer, such as the veto window on a plan. A batch with `--wake none` has a driver that waits itself, with `drive wait`.

A watch is `drive wait LABEL --notify` running as a process in a session of its own, with a pidfile under the state directory's `watches/` (`internal/drive/watches.go`). It is not a tmux session: a chat driver's shell commands end, and a tmux server exits taking every session in it along, so a watch outlives both. The wait ends when the turn has stayed done, or on a prompt, for a minute; at once on an API error or an interrupt; and ten minutes after a turn that ended while shells it started still run, since such a turn usually resumes when they finish.

Then the watch retires, removing its pidfile, before it posts: a step sent while the post is under way arms a fresh watch rather than counting on one that is about to exit. The post carries the session's label, how its turn ended, the end of its reply, the view to act on, the sessions waiting on a hold it held, and a `next:` line where the ending decides what to do. Every attempt is recorded, so an empty log line is never taken for a wake that arrived; the batch's after-wake commands run next, such as archiving the chat the wake went to, and their failure is noted without failing the wake.

A watch that ends without posting records why, a kill from anyone but drive included: drive's own stops (`park`, `close`, `drive watch` restarting a stuck watch) remove the pidfile before they signal, and a signalled watch whose pidfile is still there records the kill. `status` reads each session's wake from the log (armed, woken, ended without a wake, parked, stuck) and asks the process table only whether a watch still runs.

The webhook is configured outside the repository and the batch: `~/.config/drive/webhook`, mode 0600, with a `URL=` and an `AUTH=` line (`internal/config/config.go`).
