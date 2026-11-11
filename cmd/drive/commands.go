package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
)

// maxSeconds bounds a delay given in seconds, well short of where it would
// overflow a duration.
const maxSeconds = 30 * 24 * 60 * 60

func duration(flag string, n uint) (time.Duration, error) {
	if n > maxSeconds {
		return 0, fmt.Errorf("%s takes at most %d seconds (30 days), not %d", flag, maxSeconds, n)
	}
	return time.Duration(n) * time.Second, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// view's session: the part of a view before its @.
func sessionOf(view string) string { return strings.SplitN(view, "@", 2)[0] }

type StatusCmd struct {
	JSON  bool `help:"Print one JSON document instead of the table."`
	Quick bool `help:"Skip reading each session's pull request and merge state, which costs a gh call per session."`
}

func (c *StatusCmd) Help() string {
	return `
Each session in the batch is read live, from Claude Code's own registry,
its transcript and Rex, then the batch's holds, open asks, grants and
unfinished actions are listed under the table. Other Claude sessions
running in Rex are listed too, by window and block, so one the owner named
can be brought into the batch by its block id; one another driver's batch
drives says so, and only that driver acts on it.`
}

func (c *StatusCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "Columns", Body: `
STATE  working | done | idle (no prompt yet) | interrupted (an interrupt or a deny)
       | error | blocked:permission | blocked:question | blocked:auth (a rejected login)
       | gone (its block or its Claude Code no longer runs; a line under the table gives
       the close that records it ended and the open that brings it back)
       | starting (its open is under way)
       | unread (the read failed; run status again).
       A background job a session still runs is listed under the table.
INPUT  empty | suggestion (Claude Code's dimmed guess; typing replaces it)
       | typed (a line left unsent) | prompt (a prompt's options) | unknown (no box on screen)
WAKE   armed, or armed while a job of its holds the wake, by the time it says | stuck | posting
       | woken HH:MM (nothing done since: act on it or park it,
       or drive watch --due posts it once more, at the time it says)
       | none, or ended without a wake: no wake comes until drive watch --due
       | parked, then what ends the park, then why: on A3 (an open ask) | behind HOLD
       | until MM-DD HH:MM, "which has come" once it has | with nothing to end it
Hold lines name who holds each hold, and who waits for it with the step it was refused;
a free hold someone waits for gives the send or open to run for each waiter instead,
and the watchdog wakes you about it once.
"park of" lines name the parks whose --until has come, then those nothing ends, each
line with what to run; the watchdog wakes you about each park once.
"left by" lines name what a closed session left that stays, and why; each is a fact
to know, not a question for the owner. Run without --quick.
An open ask's "since" line names what happened after it was asked or restated: its
session closed, or its newest pull request merged (seen without --quick). Whether the
ask still holds something is yours to judge; the line gives the command for each.`},
		{Title: "JSON", Body: `
batch       name, driver (left out for a driver of no name), started, wake, webhook,
            rex_session, tz; null with no current batch
sessions    a row per session in the batch; others, a row per Claude session outside it:
            label, sid (Claude Code's session id), block, window, name (Rex's label, outside
            the batch), driven_by (outside the batch, the live batch of another driver's that
            drives it: its label there, the batch and the driver), goal (what it sets out to do, as recorded; left out when none is),
            state, for_seconds (in that state), account, input, input_text (a typed or
            suggested line), stage, pr (number, state, draft, merge, review, passed, failed,
            pending), last_sent, watch (the WAKE column's words), unhandled_wake (a wake was
            posted and nothing done since), wake_due (why --due would arm its wake; left out
            when nothing is due), view, error (why the row could not be read), next (for a
            session that is gone, the close that records it ended and the open that brings it
            back), jobs (started, command)
closed      the batch's sessions closed since
holds       a line per hold, as the table prints it
open_asks   id, session, at, question, recommendation, if_unanswered (as restated last),
            restated_at (left out until it is), overtaken (the "since" line, left out when
            there is none: closed_at, pr, repo, merged_at, each left out when it did not
            happen, and next)
grants      id, at, scope, source
in_flight   actions admitted with no outcome yet: kind, session, at, text, driver_pid (left
            out once that driver has gone)
unheard     that the driver has stopped answering its wakes and when the next try is; left
            out while it answers
timers_held a line per timer due and waiting for the driver to answer; left out when none is
parks       the "park of" lines, as the table's footer prints them; left out when there are
            none
leftovers   a line per branch a closed session left that stays, and how many closed sessions
            are not yet checked; left out with --quick or when there are none`}}
}

func (c *StatusCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	st, err := d.Status(!c.Quick)
	if err != nil {
		return err
	}
	d.Seen() // after the read, which still says what this run missed
	if c.JSON {
		return writeJSON(ctx.Stdout, st.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, st.Text(d.Time()))
	return err
}

type ReadCmd struct {
	Session string `arg:"" help:"The session: a label in the batch, or a block id for one outside it."`
	Prompts *int   `xor:"mode" placeholder:"N" help:"Print the last N prompts it was told instead of its reply: what it is working on and how it got there."`
	Pending bool   `xor:"mode" help:"Print the tool call its permission prompt waits on."`
	User    bool   `xor:"mode" help:"Print its newest prompt as it arrived, to see whether a step sent to it landed whole."`
	Screen  bool   `xor:"mode" help:"Print the end of its screen: a prompt's options, or whatever covers its input box."`
	JSON    bool   `help:"Print one JSON document instead of text."`
}

func (c *ReadCmd) Help() string {
	return `
Every mode prints what it read, then where the session stands, its goal as
recorded, its view and next:; a session another driver drives says so, with
no next:. A closed or gone session is read from its transcript as it ended,
with no view, since there is nothing left to act on but a gone one's close
by its label; a gone one's next: is that close and the open that brings it
back.`
}

func (c *ReadCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
session, sid, goal (what it sets out to do, as recorded; left out when none is), state, account,
input, input_text (a typed or suggested line), text (what was read), at (when it was written),
view (left out for a closed or gone session), next, driven_by (for a session another driver's
batch drives, its label there, the batch and the driver; it has no next), jobs (started, command)`}}
}

func (c *ReadCmd) Validate() error {
	if c.Prompts != nil && *c.Prompts < 1 {
		return fmt.Errorf("--prompts takes a count of 1 or more, not %d", *c.Prompts)
	}
	return nil
}

func (c *ReadCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	mode, n := drive.ReadLast, 10
	switch {
	case c.Prompts != nil:
		mode, n = drive.ReadPrompts, *c.Prompts
	case c.Pending:
		mode = drive.ReadPending
	case c.User:
		mode = drive.ReadUser
	case c.Screen:
		mode = drive.ReadScreen
	}
	r, err := d.Read(c.Session, mode, n)
	if err != nil {
		return err
	}
	d.Seen()
	if c.JSON {
		return writeJSON(ctx.Stdout, r.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, r.Text())
	return err
}

type WaitCmd struct {
	Session string `arg:"" help:"The session to wait for."`
	Quiet   uint   `default:"60" placeholder:"SECONDS" help:"How long the turn must stay over before the wait ends, so a session that pauses between tools is not taken for done."`
	Bg      uint   `default:"600" placeholder:"SECONDS" help:"How long to wait on a turn that ended while background jobs it started still run; such a turn usually resumes when they finish."`
	JSON    bool   `help:"Print one JSON document instead of text."`
}

func (c *WaitCmd) Help() string {
	return `
The wait ends when the turn has stayed over for --quiet, or the session
stops on a prompt; at once on an API error or an interrupt. It prints the
end of the reply, the view to act on, and next: where the ending decides
what to do. A driver that waits itself runs it in the background; a chat
driver is woken instead (drive watch).`
}

func (c *WaitCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
session, block, state, error (kind, text, at: the API error that ended the turn), shells
(jobs this turn started, still running), orphans (older jobs), jobs (started, command),
reply (its end), screen (its end, for a prompt the transcript does not show), view, next`}}
}

func (c *WaitCmd) Validate() error {
	_, err := duration("--quiet", c.Quiet)
	if err == nil {
		_, err = duration("--bg", c.Bg)
	}
	return err
}

func (c *WaitCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	e, err := d.Wait(c.Session, time.Duration(c.Quiet)*time.Second, time.Duration(c.Bg)*time.Second)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(ctx.Stdout, e.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, e.Text())
	return err
}

// Step is the step a send or open carries: a typed line, a file, or one of
// the owner's snippets.
type Step struct {
	Line    []string `arg:"" optional:"" help:"The step as one line, typed as is, a slash command too; with --snippet, the text the owner would type after the key."`
	File    string   `xor:"step" type:"existingfile" placeholder:"PATH" help:"Send this file instead: your own request, or anything longer than a line."`
	Snippet string   `xor:"step" placeholder:"KEY" help:"Send the owner's TabType snippet KEY, as it resolves in the session's directory."`
}

func (s Step) step() drive.Step {
	if s.Snippet != "" {
		return drive.Step{Key: s.Snippet, Text: strings.Join(s.Line, " ")}
	}
	return drive.Step{Line: strings.Join(s.Line, " "), File: s.File}
}

// atMostOne refuses a line beside --file: one step at a time.
func (s Step) atMostOne() error {
	if s.File != "" && len(s.Line) > 0 {
		return errors.New("give one step: a line or --file PATH, not both")
	}
	return nil
}

var stepSections = []clikit.Section{{Title: "Steps", Body: `
A step is one of:
  'one short line'        typed as is, a slash command too; an @PATH in it attaches
                          that file, as the owner's would ('/write-spec @/tmp/answers.md'),
                          except in a step queued mid-turn, where it stays a path. A line
                          typing would garble (over 500 bytes, a newline or control
                          character, a leading ! or #) goes as a file instead
  --file PATH             typed as @<copy>, which attaches it, drive keeping the copy;
                          queued mid-turn, a line asks the session to read the copy
  --snippet KEY 'TEXT'    the owner's snippet for the session's directory, with the
                          step's words, TEXT, where the owner would type after it
A file or a snippet whose first line starts a skill the session has is typed
as that line with the rest attached, so its prompt opens with the skill; one
that runs only when typed is never queued.
Text that starts with a dash goes after --, which ends the flags.
The first step a session gets in a batch carries the owner's notice: at the head
of its file, or attached after its line.`}}

type SendCmd struct {
	View  string `arg:"" help:"The view the step was decided on, as read, wait, status or a wake printed it (search@3fa91c07)."`
	Step  `embed:""`
	Queue bool   `help:"Send to a working session, which takes the step mid-turn: the view then needs only that no prompt came since. A session on a prompt takes nothing until it is answered."`
	Hold  string `placeholder:"NAME" help:"Name what this step uses that only one session may use at a time, such as a release or commits into a shared checkout. A second send or open naming it is refused, saying why, while this session works, later steps included, waits on a prompt, or runs a background job it started."`
	As    string `placeholder:"LABEL" help:"The label a session outside the batch joins it under, on its first send: a name for its work, such as search."`
}

func (c *SendCmd) Help() string {
	return `
The send is refused, and nothing reaches the session, when the session has
moved past the view, when it waits on a prompt (answer it first), or when
another session holds what --hold names. It prints how the step arrived; in
a batch that wakes a chat driver it arms the session's wake.`
}

func (c *SendCmd) HelpSections() []clikit.Section { return stepSections }

func (c *SendCmd) Validate() error {
	if c.File == "" && c.Snippet == "" && len(c.Line) == 0 {
		return errors.New("needs a step: a line, --file PATH or --snippet KEY")
	}
	return c.atMostOne()
}

func (c *SendCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	sent, err := d.Send(c.View, c.step(), drive.SendOptions{Queue: c.Queue, Hold: c.Hold, Label: c.As})
	if sent != nil && sent.Outcome != "" {
		_, _ = io.WriteString(ctx.Stdout, sent.Text())
	}
	return err
}

type OpenCmd struct {
	Label   string `arg:"" help:"The label the new session gets in the batch and on its Rex block: a name for its work, such as search-repairs."`
	Step    `embed:""`
	Dir     string  `type:"existingdir" default:"." help:"The directory the session starts in."`
	Resume  string  `placeholder:"ID" help:"Resume this Claude Code session id instead of starting fresh, such as a session relaunched on another account."`
	Account string  `placeholder:"NAME" help:"Run it on this Claude account instead of the launcher's choice."`
	Model   string  `placeholder:"MODEL" help:"Run it on this model, such as fable for a fresh judge."`
	Effort  *string `enum:"low,medium,high,xhigh,max" placeholder:"LEVEL" help:"Run it at this effort level, for this session only; without it the owner's settings decide."`
	Hold    string  `placeholder:"NAME" help:"Name what its first turn uses that only one session may use at a time, as send's --hold does."`
	Goal    *string `placeholder:"TEXT" help:"What the session sets out to do, in plain terms for the owner: one short paragraph, at most 600 bytes."`
	From    string  `type:"existingfile" xor:"opening" placeholder:"NOTE" help:"Open from this start note: its opens: line says how the session starts (a brief, or the line to type), its goal: line is the goal, and the rest goes with the first step. --dir is the project's checkout."`
	Brief   string  `xor:"opening" placeholder:"SLUG" help:"Pick this brief up: brief places its worktree, where the session opens, and its pointer is the first step. --dir is the project's checkout."`
}

func (c *OpenCmd) Help() string {
	return `
The session opens in a new Rex block in the batch's Rex session, started
again if it no longer runs, and joins the batch under LABEL; the step, if
given, is its first. --goal is what the owner reads the session is for; an
open without it, such as a relaunch, keeps the goal LABEL already has.
Resuming LABEL's own session, as the next: for a gone one does, carries it
on: its park stands until its next step, and so does what was sent to it.

With --from or --brief, drive composes the first step: the line that starts
the session, typed as the owner would, then a line or file given beside it,
then the note's text, then a brief's Brief:, Drift: and Pickup gate: lines.
A session that opens on a line in the project's main checkout is told to
work in a worktree of its own.`
}

func (c *OpenCmd) HelpSections() []clikit.Section {
	return append(slices.Clone(stepSections), clikit.Section{Title: "Examples", Body: `
drive open sr-9531 --effort xhigh --goal 'Review #9531 before it merges.' '/strategic-review 9531'
drive open closing-reads --dir ~/dev/planlab/main --from ~/dev/.handoffs/planlab-main/_batches/2026-11-12/closing-reads.md 'This turn is onboarding only.'
drive open sonnet-failover --dir ~/dev/planlab/main --brief model-failover-sonnet-5-5-first --goal 'Fail over to Sonnet first.'`})
}

func (c *OpenCmd) Validate() error {
	if (c.From != "" || c.Brief != "") && (c.Snippet != "" || c.Resume != "") {
		return errors.New("--from and --brief compose the first step: give a line or --file beside them, not --snippet or --resume")
	}
	return c.atMostOne()
}

func (c *OpenCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	opt := drive.OpenOptions{Dir: c.Dir, Resume: c.Resume, Account: c.Account, Model: c.Model, Hold: c.Hold, Goal: c.Goal, From: c.From, Brief: c.Brief}
	if c.Effort != nil {
		opt.Effort = *c.Effort
	}
	o, err := d.Open(c.Label, c.step(), opt)
	if o != nil {
		_, _ = io.WriteString(ctx.Stdout, o.Text())
	}
	return err
}

type RespondCmd struct {
	View   string `arg:"" help:"The view the answer was decided on."`
	Action string `arg:"" enum:"approve,deny,interrupt,clear" help:"approve or deny the prompt, interrupt the turn, or clear a line left in its input box."`
}

func (c *RespondCmd) Help() string {
	return `
A deny ends the turn, and the session takes its next step as usual. On a
question, approve takes the option ❯ points at and deny dismisses it, after
which a send can answer in words; read --screen shows the options. It prints
what the session reads afterwards.`
}

func (c *RespondCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	r, err := d.Respond(c.View, c.Action)
	if r != nil {
		_, _ = io.WriteString(ctx.Stdout, r.Text())
	}
	return err
}

type WatchCmd struct {
	Session string `arg:"" optional:"" help:"The session whose wake to arm."`
	Due     bool   `help:"Arm every session whose wakes stopped coming, and post once more a wake nobody acted on: a chat driver's watchdog in one command."`
	DueAll  bool   `name:"due-all" help:"Do what --due does in every live batch that wakes a driver, and post only what needs a driver's judgment: the machine's watchdog, which runs no model when nothing is due."`
	StopAll bool   `name:"stop-all" help:"Before Rex's server restarts: stop the wake of every session in every live batch, whichever driver drives it. Timers run on."`
}

func (c *WatchCmd) Help() string {
	return `
In a batch that wakes a chat driver, any send, open or respond arms the
session's wake: a post to the webhook the batch names, the URL= and AUTH=
lines in ~/.config/drive/webhooks/DRIVER. It comes about a minute after the
turn ends, at once on an API error or an interrupt, and when the session
stops on a prompt, and ends with next: where the ending decides what to do.
A refused action armed nothing. Watch arms one by hand.

A turn that ended while background jobs it started run wakes no one unless
its reply asks something: a job's end usually resumes the turn, so the wake
comes when that turn ends, or by two hours on, and then at ever longer marks.
A reply that asks wakes the driver about a minute on, once per view a driver
heard. Either way the watch stays armed, and watch on it says the run heard
the wake. Endings close together share one post,
at most one every three minutes; every wake a driver does not answer pushes the
next further off, up to two hours, until a drive command shows it runs.

--due-all runs from the machine's scheduler, every few minutes, and costs no
model run unless something needs a driver: a session that is gone, a watch
that keeps failing, a park whose time has come or that nothing ends, a hold
that came free while a session waits for it, or open asks after six hours
with no wake.

A restart of Rex's server ends every session in it. Stop the wakes first
with --stop-all, so none wakes its driver mid-restart; it prints the
watch --due that arms each batch's again once the server runs.`
}

func (c *WatchCmd) Validate() error {
	if n := btoi(c.Session != "") + btoi(c.Due) + btoi(c.DueAll) + btoi(c.StopAll); n != 1 {
		return errors.New("give a SESSION, --due, --due-all or --stop-all")
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (c *WatchCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	if c.DueAll {
		reports, err := d.DueAll()
		if err != nil {
			return err
		}
		_, err = io.WriteString(ctx.Stdout, drive.DueText(reports))
		return err
	}
	d.Seen() // the driver's own run, armed or not
	if c.StopAll {
		stopped, err := d.StopAll()
		if err == nil || len(stopped) > 0 {
			_, _ = io.WriteString(ctx.Stdout, drive.StoppedText(stopped))
		}
		return err
	}
	if !c.Due {
		w, err := d.Watch(c.Session)
		if err != nil {
			return err
		}
		_, err = io.WriteString(ctx.Stdout, w.Text())
		return err
	}
	armed, later, err := d.Due()
	for _, r := range armed {
		_, _ = io.WriteString(ctx.Stdout, r.Text())
	}
	if err != nil {
		return err
	}
	if len(armed) == 0 {
		fmt.Fprintln(ctx.Stdout, "no session needs its wake armed")
	}
	for _, r := range later {
		_, _ = io.WriteString(ctx.Stdout, r.LaterText())
	}
	return nil
}

type TimerCmd struct {
	Seconds uint     `arg:"" help:"How long until the wake."`
	Note    []string `arg:"" help:"What the wake is for, such as veto; the wake carries it."`
}

func (c *TimerCmd) Help() string {
	return `
In a batch that wakes a chat driver, the driver is woken after SECONDS with
the note, whatever the sessions are doing: the veto window on a plan, a
read that is due later.`
}

func (c *TimerCmd) Validate() error {
	_, err := duration("SECONDS", c.Seconds)
	return err
}

func (c *TimerCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	after, note := time.Duration(c.Seconds)*time.Second, strings.Join(c.Note, " ")
	if err := d.Timer(after, note); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "timer armed: the driver is woken in %s (%s)\n", after, note)
	return nil
}

type ParkCmd struct {
	View   string   `arg:"" help:"The view the park was decided on."`
	Reason []string `arg:"" help:"Why it waits, in the owner's terms, since their page shows it."`
	Until  string   `placeholder:"WHEN" help:"When to look at it again: a date or a date and time in the batch's zone, such as 2026-11-06 or \"2026-11-06 09:00\", or a duration such as 4h. The watchdog wakes you about it then."`
}

func (c *ParkCmd) Help() string {
	return `
A park is refused unless something on record ends it, since nothing else
brings the session back to you: --until, an open ask about it, or a hold it
was refused a step for.

A wait on a date, or on something outside the batch such as a teammate's
pull request, takes --until: check what it waits for yourself when that
time comes. An ask is for a decision that only the owner can make.`
}

func (c *ParkCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "Examples", Body: `
drive park S4@… --until 4h 'waits for #10137, which a teammate is landing'  # check it yourself then
drive park S6@… --until 2026-11-06 'waits for the 6 Nov release'
drive record ask S5 --question 'Start the build without #10137?' --if-unanswered 'S5 stays parked'
drive park S5@… 'waits for your call on #10137'      # the open ask ends it
drive park S7@… 'waits for the release, held by S2'  # after a send refused on that hold`}}
}

func (c *ParkCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	var until time.Time
	if c.Until != "" {
		var err error
		if until, err = parseUntil(c.Until, batchZone(d), d.Time()); err != nil {
			return clikit.Usage(err)
		}
	}
	if err := d.Park(c.View, strings.Join(c.Reason, " "), until); err != nil {
		return err
	}
	if !until.IsZero() {
		fmt.Fprintf(ctx.Stdout, "parked %s until %s: no wake until a step is sent to it, and the watchdog wakes you about it then\n", sessionOf(c.View), until.Format("2006-01-02 15:04"))
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "parked %s: no wake until a step is sent to it\n", sessionOf(c.View))
	return nil
}

type CloseCmd struct {
	View    string `arg:"" help:"The view the close was decided on; a gone session, which has none, by its label."`
	Cleanup bool   `help:"Then remove its branch and worktree once merged, as drive cleanup does."`
}

func (c *CloseCmd) Help() string {
	return `
Refused while the session works, runs a background job, or holds a typed
line nobody sent: next: gives what to do first, such as the step that has
the session stop its job.

A session that is gone, its tab closed outside drive or its Claude Code
ended, has no view and is closed by its label: the close is put on record,
its park and wake end, and a block left running only a shell is closed
too. It prints the open that brings the session back.`
}

func (c *CloseCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	label := sessionOf(c.View)
	if label == c.View {
		out, err := d.CloseGone(label)
		if err != nil {
			return err
		}
		fmt.Fprint(ctx.Stdout, out.Text())
	} else if err := d.Close(c.View); err != nil {
		return err
	} else {
		fmt.Fprintf(ctx.Stdout, "closed %s\n", label)
	}
	if !c.Cleanup {
		return nil
	}
	return cleanup(ctx, d, label)
}

type FreeCmd struct {
	Hold   string   `arg:"" help:"The hold to free, as --hold named it, such as release."`
	Reason []string `arg:"" help:"What shows its holder no longer needs the hold; the log keeps it."`
}

func (c *FreeCmd) Help() string {
	return `
A hold frees itself once its holder's turn has ended and no background job
it started runs. Free hands it back sooner: the turn has ended, and the job
still running is not what the hold was taken for, such as an eval a release
step also started. The refusal on the hold prints the job's command; read
the holder's reply when the command does not say what the job is.

It is refused while the holder works, waits on a prompt, or its send is
under way; a hold nothing holds is reported free. A freed hold is free
until a send or open takes it again, so a step that uses it still names it
with --hold. It prints the step to run for each session that waits for the
hold.`
}

func (c *FreeCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "Examples", Body: `
drive free release 'S2 released at 15:37; the job it left is its eval, which releases nothing'`}}
}

func (c *FreeCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	f, err := d.Free(c.Hold, strings.Join(c.Reason, " "))
	if err != nil {
		return err
	}
	d.Seen() // a hold nothing held leaves no other trace of the run
	_, err = io.WriteString(ctx.Stdout, f.Text())
	return err
}

// cleanup runs a cleanup and prints what became of it. A refusal did nothing,
// so it prints only the refusal.
func cleanup(ctx *kong.Context, d *drive.Driver, session string) error {
	cleaned, err := d.Cleanup(session)
	if err == nil || len(cleaned) > 0 {
		_, _ = io.WriteString(ctx.Stdout, drive.CleanedText(cleaned))
	}
	return err
}

type CleanupCmd struct {
	Session string `arg:"" optional:"" help:"One closed session of the batch, instead of every closed session of the driver's batches."`
}

func (c *CleanupCmd) Help() string {
	return `
A closed session's merged branch goes with its worktree. One that is not
merged stays, as does one gwt will not remove, such as a checkout with
uncommitted work or a branch an open session is on: it prints why, status
lists it once as a fact, and nobody is asked. What stays, or could not be
checked, is judged again on the next run, since a pull request can merge after
its session closed. gwt keeps a recovery ref for anything it discards.`
}

func (c *CleanupCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	return cleanup(ctx, d, c.Session)
}

type RecordCmd struct {
	Grant   GrantCmd   `cmd:"" help:"Record authority the owner handed over, in their words, with what it covers."`
	Ask     AskCmd     `cmd:"" help:"Record a question put to the owner, with its recommendation and what happens unanswered."`
	Answer  AnswerCmd  `cmd:"" help:"Record the owner's answer to an ask."`
	Restate RestateCmd `cmd:"" help:"Record what an open ask waits on now, once its session closed or its change merged."`
	Note    NoteCmd    `cmd:"" help:"Record a note, such as the plan, for the run after a reset to read."`
	Goal    GoalCmd    `cmd:"" help:"Record what a session sets out to do, in plain terms for the owner."`
}

func (RecordCmd) Help() string {
	return `
The record is the batch's memory: status lists the grants and open asks,
log has every record, and metrics measures the asks. A grant, ask, answer
or note prints the id it recorded, such as A3, which record answer takes.`
}

// recorded prints the id a record got.
func recorded(w io.Writer, kind, id string, err error) error {
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "recorded %s %s\n", kind, id)
	return err
}

type GrantCmd struct {
	Source string `required:"" help:"The owner's words, and where and when they said them."`
	Scope  string `required:"" help:"What the grant covers."`
}

func (c *GrantCmd) Help() string {
	return `
A grant lives as long as the batch it was given in: a new batch starts with
none.`
}

func (c *GrantCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Grant(c.Source, c.Scope)
	return recorded(ctx.Stdout, "grant", id, err)
}

type AskCmd struct {
	Session        string `arg:"" help:"The session the question holds, or - for the batch."`
	Question       string `required:"" help:"The question, as the owner reads it."`
	Recommendation string `help:"What you recommend."`
	IfUnanswered   string `required:"" help:"What happens while nobody answers."`
	Replaces       string `placeholder:"ASK" help:"The open ask this question replaces, such as A4, recorded moot in the same write."`
}

func (c *AskCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Ask(strings.TrimPrefix(c.Session, "-"), c.Question, c.Recommendation, c.IfUnanswered, c.Replaces)
	if err := recorded(ctx.Stdout, "ask", id, err); err != nil || c.Replaces == "" {
		return err
	}
	_, err = fmt.Fprintf(ctx.Stdout, "recorded %s moot, replaced by %s\n", c.Replaces, id)
	return err
}

type RestateCmd struct {
	Ask          string `arg:"" help:"The open ask, such as A3."`
	IfUnanswered string `required:"" help:"What happens while nobody answers, as it stands now."`
}

func (c *RestateCmd) Help() string {
	return `
When status gives an ask a "since" line and the ask still holds something,
such as a production write or a message, restate it: say what it waits on
now, as the owner reads it on the page. One that holds nothing is moot
instead (record answer --took moot).`
}

func (c *RestateCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	if err := d.Restate(c.Ask, c.IfUnanswered); err != nil {
		return err
	}
	_, err := fmt.Fprintf(ctx.Stdout, "recorded restate %s\n", c.Ask)
	return err
}

type AnswerCmd struct {
	Ask       string `arg:"" help:"The ask it answers, such as A3."`
	Source    string `required:"" help:"The owner's words, or what showed the ask no longer holds; words after the ask, outside any flag, are the step its session is sent."`
	Took      string `required:"" enum:"recommended,amended,delegated,moot" help:"Whether the owner took the recommendation, amended it, handed the question to someone else, or it stopped mattering."`
	Explained bool   `help:"The owner needed the question explained before answering."`
	Step      `embed:""`
	Hold      string `placeholder:"NAME" help:"With a step: name what it uses that only one session may use at a time, as send's --hold does."`
}

func (c *AnswerCmd) Help() string {
	return `
A step given here goes to the session the ask is about, at the view it stands
at now, so one command answers an ask and sends what follows from it. Refused,
it records nothing and sends nothing.`
}

func (c *AnswerCmd) HelpSections() []clikit.Section { return stepSections }

// Validate refuses an answer without the owner's words: on 2026-11-03 an
// answer given them as its last words typed them into the session as its
// step, three times.
func (c *AnswerCmd) Validate() error {
	if strings.TrimSpace(c.Source) == "" {
		return errors.New("an answer needs --source, the owner's words or what showed the ask no longer holds; words after the ask are the step sent to its session")
	}
	return c.atMostOne()
}

func (c *AnswerCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	if c.File == "" && c.Snippet == "" && len(c.Line) == 0 {
		id, err := d.Answer(c.Ask, c.Source, c.Took, c.Explained)
		if err := recorded(ctx.Stdout, "answer", id, err); err != nil {
			return err
		}
		if next := d.LeftParked(id); next != "" {
			fmt.Fprintf(ctx.Stdout, "next: %s\n", next)
		}
		return nil
	}
	sent, id, err := d.AnswerAndSend(c.Ask, c.Source, c.Took, c.Explained, c.step(), drive.SendOptions{Hold: c.Hold})
	if sent != nil && sent.Outcome != "" {
		_, _ = io.WriteString(ctx.Stdout, sent.Text())
	}
	if id != "" {
		_ = recorded(ctx.Stdout, "answer", id, nil)
	}
	return err
}

type NoteCmd struct {
	Session string   `placeholder:"SESSION" help:"The session the note is about."`
	Text    []string `arg:"" help:"The note."`
}

func (c *NoteCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Note(c.Session, strings.Join(c.Text, " "))
	return recorded(ctx.Stdout, "note", id, err)
}

type GoalCmd struct {
	Session string   `arg:"" help:"The session, by its label in the batch; a closed one too."`
	Text    []string `arg:"" help:"What it sets out to do, in plain terms for the owner: one short paragraph, at most 600 bytes."`
}

func (c *GoalCmd) Help() string {
	return `
Give a goal to a session that has none, which status names (it joined the
batch on a send, or opened without --goal), and replace one once the
session's purpose or scope changes. read and the owner's page show the
latest.`
}

func (c *GoalCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	if err := d.Goal(c.Session, strings.Join(c.Text, " ")); err != nil {
		return err
	}
	_, err := fmt.Fprintf(ctx.Stdout, "recorded goal %s\n", c.Session)
	return err
}

type LogCmd struct {
	Since string `placeholder:"TIME" help:"Print only what happened from this time on, a date or a date and time such as \"2026-10-22 16:00\", read in the zone drive prints times in."`
	JSON  bool   `help:"Print the events as the batch recorded them, as one JSON array."`
}

func (c *LogCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
The batch's events, oldest first, as recorded: at, kind, and the fields its kind carries,
among session, block, sid, account, dir, model, effort, intent, mode, key, path, text, notice,
outcome, reason, hold, view, action, call, id, ref, source, scope, question, recommendation,
if_unanswered, took, explained, note, goal, http, try, via, branch, jobs, why, due, held_by.`}}
}

func (c *LogCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	var since time.Time
	if c.Since != "" {
		var err error
		if since, err = parseWhen(c.Since, batchZone(d)); err != nil {
			return clikit.Usage(err)
		}
	}
	b, evs, err := d.Log(since)
	if err != nil {
		return err
	}
	d.Seen()
	if c.JSON {
		return writeJSON(ctx.Stdout, drive.LogJSON(evs))
	}
	_, err = io.WriteString(ctx.Stdout, drive.LogText(b, evs, d.Time()))
	return err
}

// batchZone is the zone drive prints the batch's times in, so a time copied
// from its output means what it said.
func batchZone(d *drive.Driver) *time.Location {
	if b, err := d.Batch(); err == nil {
		return b.Zone()
	}
	return time.Local
}

// parseUntil reads park's --until: a duration from now, or a date or a date
// and time in zone.
func parseUntil(s string, zone *time.Location, now time.Time) (time.Time, error) {
	if dur, err := time.ParseDuration(s); err == nil {
		return now.Add(dur).In(zone).Truncate(time.Minute), nil
	}
	if t, err := parseWhen(s, zone); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--until %q: give a date, a date and time, such as \"2026-11-06 09:00\", or a duration, such as 4h", s)
}

func parseWhen(s string, zone *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, zone); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: give a date or a date and time, such as \"2026-10-22 16:00\"", s)
}

type PageCmd struct {
	Listen string `default:"127.0.0.1:8792" placeholder:"ADDR" help:"The address to serve it on."`
}

func (c *PageCmd) Help() string {
	return `
Each request reads every live batch, each driver's current one, on one
page: the questions waiting on the owner first, then each batch under a
tab named for its project, the Rex session its sessions open in, beside
All, which lists the sessions waiting on a driver. Each session opens to
its stages and its history in the batch's record, the driver's notes
among it, closed sessions and answered questions included. --batch or
--driver shows one batch instead. It changes nothing. A session's anchor is
the batch's name and its label, such as #2026-10-27/search, so a link opens
that batch's tab and the session; #2026-10-27 opens the tab. A read is
reused for 30 seconds; one that takes longer than 20 seconds is answered
from the batch's record while it finishes.

It runs until stopped, and exits once the drive binary it runs is
replaced, so a supervisor such as launchd restarts it on the new one. It
answers anyone who reaches the address: on Mini 2, a loopback address
that tailscale serve publishes on the tailnet.`
}

// replacedEvery is how often a running page checks whether its binary was
// replaced by an install.
var replacedEvery = 30 * time.Second

func (c *PageCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: drive.NewPage(d), ReadHeaderTimeout: 10 * time.Second}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		was, err := os.Stat(d.Self)
		for err == nil {
			select {
			case <-stop.Done():
				return
			case <-time.After(replacedEvery):
			}
			if now, err := os.Stat(d.Self); err != nil || !os.SameFile(was, now) {
				fmt.Fprintf(ctx.Stdout, "%s was replaced: stopping, for the new one to start\n", d.Self)
				cancel()
				return
			}
		}
	}()
	go func() {
		<-stop.Done()
		shut, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shut)
	}()
	fmt.Fprintf(ctx.Stdout, "serving the batch's page at http://%s/\n", ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type MetricsCmd struct {
	JSON bool `help:"Print one JSON document instead of text."`
}

func (c *MetricsCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
batch, from, to, sessions; asks, answered, unanswered, took (counts of recommended,
amended, delegated, moot), explained; owner_wait_seconds, median_answer_seconds,
per_merge_seconds; merged (repo, number or branch and commit, at, sessions), unread;
sends, snippets, by_key, commands, own_words, recovered, garbled, other, unknown;
refusals (counts by kind), failures; wakes, timers, wake_fails, carried, wakes_by_why (counts
by what made them go); wakes_acted_on, wakes_heard_only, wakes_untraced (each session's wake
by what it drew before its next: a step, a prompt answered, a park or a close; a driver's run
that left the session as it was; no trace of a run); unwatched`}}
}

func (c *MetricsCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	m, err := d.Metrics()
	if err != nil {
		return err
	}
	d.Seen()
	if c.JSON {
		return writeJSON(ctx.Stdout, m.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, m.Text())
	return err
}

type BatchCmd struct {
	Show BatchShowCmd `cmd:"" default:"1" help:"Print the batch: its driver, start, wakes, Rex session, zone, sessions and directory."`
	New  BatchNewCmd  `cmd:"" help:"Start a batch and make it its driver's current one."`
	Use  BatchUseCmd  `cmd:"" help:"Make a batch its driver's current one again."`
	End  BatchEndCmd  `cmd:"" help:"End a batch whose drive is over and whose record the owner has read: its wakes stop and it leaves the owner's page."`
	List BatchListCmd `cmd:"" help:"List every batch, with its driver, each driver's current one starred."`
}

type BatchShowCmd struct{}

func (c *BatchShowCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	b, err := d.Batch()
	if err != nil {
		return err
	}
	_, drivers, err := d.Batches()
	if err != nil {
		return err
	}
	d.Seen()
	_, err = io.WriteString(ctx.Stdout, drive.BatchText(b, drivers.Ended[b.Meta.Name]))
	return err
}

type BatchNewCmd struct {
	Name       string `arg:"" help:"The batch's name, such as the date."`
	Webhook    string `placeholder:"DRIVER" help:"Wake this chat driver by its webhook, the file ~/.config/drive/webhooks/DRIVER that the owner writes; DRIVER is also the driver's name. Without it, the driver waits itself."`
	RexSession string `placeholder:"SESSION" help:"The Rex session new sessions open in, recorded by its label and started whenever it is not running; defaults to the configuration's, then the one drive runs in."`
	TZ         string `name:"tz" placeholder:"ZONE" help:"The zone drive prints the batch's times in, such as Europe/London; defaults to the configuration's, then the machine's."`
	AfterWake  string `placeholder:"COMMAND" help:"A shell command to run after each wake is posted, such as archiving the chat it went to."`
}

func (c *BatchNewCmd) Help() string {
	return `
A new batch starts with no grants, however recently one was given, and
stops the wakes of its driver's last batch, no other driver's. Start one per
drive, never after a reset: ` + "`drive --driver DRIVER batch`" + ` names yours. It starts
before the owner writes its webhook; no wake starts until then.`
}

func (c *BatchNewCmd) Validate() error {
	if c.TZ != "" {
		if _, err := time.LoadLocation(c.TZ); err != nil {
			return fmt.Errorf("--tz %q is not a zone name such as Europe/London", c.TZ)
		}
	}
	return nil
}

func (c *BatchNewCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	wake := "none"
	if c.Webhook != "" {
		wake = "webhook"
	}
	var after []string
	if c.AfterWake != "" {
		after = []string{c.AfterWake}
	}
	st, err := d.StartBatch(drive.BatchOptions{Name: c.Name, Wake: wake, Webhook: c.Webhook, Driver: d.DriverName, RexSession: c.RexSession, TZ: c.TZ, AfterWake: after})
	if err != nil {
		return err
	}
	_, err = io.WriteString(ctx.Stdout, st.Text())
	return err
}

type BatchUseCmd struct {
	Name string `arg:"" help:"The batch to make its driver's current one."`
}

func (c *BatchUseCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	b, err := d.UseBatch(c.Name)
	if err != nil {
		return err
	}
	if driver := batch.DriverOf(b.Meta); driver != "" {
		fmt.Fprintf(ctx.Stdout, "batch %s is driver %s's current batch\n", c.Name, driver)
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "batch %s is current\n", c.Name)
	return nil
}

type BatchEndCmd struct{}

func (c *BatchEndCmd) Help() string {
	return `
Its record stays, read with --batch NAME. A batch left running stays on the
page until its driver's next one replaces it.`
}

func (c *BatchEndCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	e, err := d.EndBatch()
	if e != nil {
		_, _ = io.WriteString(ctx.Stdout, e.Text())
	}
	return err
}

type BatchListCmd struct{}

func (c *BatchListCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	metas, drivers, err := d.Batches()
	if err != nil {
		return err
	}
	_, err = io.WriteString(ctx.Stdout, drive.BatchesText(metas, drivers))
	return err
}

type SnippetCmd struct {
	Key  string   `arg:"" help:"The snippet's key, such as land-pr."`
	Dir  string   `arg:"" optional:"" default:"." help:"The directory it resolves in: a project's own snippets win over the owner's global ones."`
	Text []string `arg:"" optional:"" help:"The text the owner would type after the key."`
}

func (c *SnippetCmd) Help() string {
	return `
It prints the expanded text on stdout and the file it came from on stderr,
to check what a send --snippet would type before sending it.`
}

func (c *SnippetCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	s, err := d.Snippet(c.Dir, c.Key, strings.Join(c.Text, " "))
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stderr, "from: %s\n", s.From)
	fmt.Fprintln(ctx.Stdout, s.Text)
	return nil
}

type SnipsCmd struct {
	Dir string `arg:"" optional:"" default:"." help:"The directory whose keys to list."`
}

func (c *SnipsCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	from, keys, err := d.Snippets(c.Dir)
	if err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Fprintf(ctx.Stdout, "%s\t%s\n", k, from[k])
	}
	return nil
}

// onKill records a signal that ends a watch drive started, which carries
// its name in $DRIVE_WATCH, so a watch never ends without a word.
func onKill(d *drive.Driver, label string) string {
	watch := os.Getenv("DRIVE_WATCH")
	if watch == "" {
		return ""
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		sig := <-sigs
		if d.Killed(watch, label, sig) {
			os.Exit(3)
		}
		os.Exit(0)
	}()
	return watch
}

type NotifyCmd struct {
	Label string `arg:"" help:"The session to wait for."`
}

func (c *NotifyCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	watch := onKill(d, c.Label)
	e, err := d.Notify(watch, c.Label, drive.WakeQuiet)
	if err != nil {
		return err
	}
	if e.Covered {
		fmt.Fprintf(ctx.Stdout, "%s another session's wake carried %s (%s)\n", d.Time().Format(time.RFC3339), e.Label, e.State)
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "%s woke the driver for %s (%s)\n", d.Time().Format(time.RFC3339), e.Label, e.State)
	_, err = io.WriteString(ctx.Stdout, e.Text())
	return err
}

type RingCmd struct {
	Seconds uint     `arg:"" help:"How long to sleep."`
	Note    []string `arg:"" help:"The timer's note."`
}

func (c *RingCmd) Run(ctx *kong.Context, d *drive.Driver) error {
	note := strings.Join(c.Note, " ")
	watch := onKill(d, "timer "+note)
	time.Sleep(time.Duration(c.Seconds) * time.Second)
	if err := d.Ring(watch, note); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "timer done: %s\n", note)
	return nil
}
