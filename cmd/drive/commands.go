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
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

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
	runs
	JSON  bool `help:"Print one JSON document instead of the table."`
	Quick bool `help:"Skip reading each session's pull request and merge state, which costs a gh call per session."`
}

func (c *StatusCmd) Help() string {
	return `
Each session in the batch is read live, from Claude Code's own registry,
its transcript and Rex, then the batch's holds, open asks, grants and
unfinished actions are listed under the table. Other Claude sessions
running in Rex are listed too, by window and block, so one the owner named
can be brought into the batch by its block id.`
}

func (c *StatusCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "Columns", Body: `
STATE  working | done | idle (no prompt yet) | interrupted (an interrupt or a deny)
       | error | blocked:permission | blocked:question | blocked:auth (a rejected login).
       A background job a session still runs is listed under the table.
INPUT  empty | suggestion (Claude Code's dimmed guess; typing replaces it)
       | typed (a line left unsent) | prompt (a prompt's options) | unknown (no box on screen)
WAKE   armed | stuck | posting | woken HH:MM (nothing done since: act on it or park it,
       or drive watch --due posts it once more, at the time it says)
       | none, or ended without a wake: no wake comes until drive watch --due | parked: why
Hold lines name who holds each hold, and who waits for it with the step it was refused.`},
		{Title: "JSON", Body: `
batch       name, started, wake, webhook, rex_session, tz; null with no current batch
sessions    a row per session in the batch; others, a row per Claude session outside it:
            label, sid (Claude Code's session id), block, window, name (Rex's label, outside
            the batch), goal (what it sets out to do, as recorded; left out when none is),
            state, for_seconds (in that state), account, input, input_text (a typed or
            suggested line), stage, pr (number, state, draft, merge, review, passed, failed,
            pending), last_sent, watch (the WAKE column's words), unhandled_wake (a wake was
            posted and nothing done since), wake_due (why --due would arm its wake; left out
            when nothing is due), view, error (why the row could not be read), jobs (started,
            command)
closed      the batch's sessions closed since
holds       a line per hold, as the table prints it
open_asks   id, session, at, question, recommendation, if_unanswered
grants      id, at, scope, source
in_flight   actions admitted with no outcome yet: kind, session, at, text, driver_pid (left
            out once that driver has gone)`}}
}

func (c *StatusCmd) act(ctx *kong.Context, d *drive.Driver) error {
	st, err := d.Status(!c.Quick)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(ctx.Stdout, st.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, st.Text(d.Time()))
	return err
}

type ReadCmd struct {
	runs
	Session string `arg:"" help:"The session: a label in the batch, or a block id for one outside it."`
	Prompts *int   `xor:"mode" placeholder:"N" help:"Print the last N prompts it was told instead of its reply: what it is working on and how it got there."`
	Pending bool   `xor:"mode" help:"Print the tool call its permission prompt waits on."`
	User    bool   `xor:"mode" help:"Print its newest prompt as it arrived, to see whether a step sent to it landed whole."`
	Screen  bool   `xor:"mode" help:"Print the end of its screen: a prompt's options, or whatever covers its input box."`
	JSON    bool   `help:"Print one JSON document instead of text."`
}

func (c *ReadCmd) Help() string {
	return `
Every mode prints what it read, then where the session stands, its view and
next:. A closed or gone session is read from its transcript as it ended,
with no view, since there is nothing left to act on.`
}

func (c *ReadCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
session, sid, state, account, input, input_text (a typed or suggested line), text (what was
read), at (when it was written), view (left out for a closed or gone session), next, jobs
(started, command)`}}
}

func (c *ReadCmd) Validate() error {
	if c.Prompts != nil && *c.Prompts < 1 {
		return fmt.Errorf("--prompts takes a count of 1 or more, not %d", *c.Prompts)
	}
	return nil
}

func (c *ReadCmd) act(ctx *kong.Context, d *drive.Driver) error {
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
	if c.JSON {
		return writeJSON(ctx.Stdout, r.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, r.Text())
	return err
}

type WaitCmd struct {
	runs
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

func (c *WaitCmd) act(ctx *kong.Context, d *drive.Driver) error {
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
  'one short line'        typed as is, a slash command too ('/write-spec Go with D1')
  --file PATH             sent as "Read <copy> and do what it asks."; drive keeps the copy
  --snippet KEY 'TEXT'    the owner's snippet for the session's directory, with the
                          step's words, TEXT, where the owner would type after it
Text that starts with a dash goes after --, which ends the flags.
The first step a session gets in a batch carries the owner's notice ahead of it.`}}

type SendCmd struct {
	runs
	View  string `arg:"" help:"The view the step was decided on, as read, wait, status or a wake printed it (T1@3fa91c07)."`
	Step  `embed:""`
	Queue bool   `help:"Send to a working session, which takes the step mid-turn: the view then needs only that no prompt came since. A session on a prompt takes nothing until it is answered."`
	Hold  string `placeholder:"NAME" help:"Name what this step uses that only one session may use at a time, such as a release or commits into a shared checkout. A second send or open naming it is refused, saying why, while this session works, later steps included, waits on a prompt, or runs a background job it started."`
	As    string `placeholder:"LABEL" help:"The label a session outside the batch joins it under, on its first send."`
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

func (c *SendCmd) act(ctx *kong.Context, d *drive.Driver) error {
	sent, err := d.Send(c.View, c.step(), drive.SendOptions{Queue: c.Queue, Hold: c.Hold, Label: c.As})
	if sent != nil && sent.Outcome != "" {
		_, _ = io.WriteString(ctx.Stdout, sent.Text())
	}
	return err
}

type OpenCmd struct {
	runs
	Label   string `arg:"" help:"The label the new session gets in the batch."`
	Step    `embed:""`
	Dir     string  `type:"existingdir" default:"." help:"The directory the session starts in."`
	Resume  string  `placeholder:"ID" help:"Resume this Claude Code session id instead of starting fresh, such as a session relaunched on another account."`
	Account string  `placeholder:"NAME" help:"Run it on this Claude account instead of the launcher's choice."`
	Model   string  `placeholder:"MODEL" help:"Run it on this model, such as fable for a fresh judge."`
	Hold    string  `placeholder:"NAME" help:"Name what its first turn uses that only one session may use at a time, as send's --hold does."`
	Goal    *string `placeholder:"TEXT" help:"What the session sets out to do, in plain terms for the owner: one short paragraph, at most 600 bytes."`
}

func (c *OpenCmd) Help() string {
	return `
The session opens in a new Rex block in the batch's Rex session and joins
the batch under LABEL; the step, if given, is its first. Its goal stands
once the block opens; an open without --goal keeps the goal LABEL already
has, as a relaunch does.`
}

func (c *OpenCmd) HelpSections() []clikit.Section { return stepSections }

func (c *OpenCmd) Validate() error { return c.atMostOne() }

func (c *OpenCmd) act(ctx *kong.Context, d *drive.Driver) error {
	o, err := d.Open(c.Label, c.step(), drive.OpenOptions{Dir: c.Dir, Resume: c.Resume, Account: c.Account, Model: c.Model, Hold: c.Hold, Goal: c.Goal})
	if o != nil {
		_, _ = io.WriteString(ctx.Stdout, o.Text())
	}
	return err
}

type RespondCmd struct {
	runs
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

func (c *RespondCmd) act(ctx *kong.Context, d *drive.Driver) error {
	r, err := d.Respond(c.View, c.Action)
	if r != nil {
		_, _ = io.WriteString(ctx.Stdout, r.Text())
	}
	return err
}

type WatchCmd struct {
	runs
	Session string `arg:"" optional:"" help:"The session whose wake to arm."`
	Due     bool   `help:"Arm every session whose wakes stopped coming, and post once more a wake nobody acted on: a chat driver's watchdog in one command."`
}

func (c *WatchCmd) Help() string {
	return `
In a batch that wakes a chat driver, any send, open or respond arms the
session's wake: a post to the webhook the batch names, the URL= and AUTH=
lines in ~/.config/drive/webhooks/DRIVER. It comes about a minute after the
turn ends, at once on an API error or an interrupt, and when the session
stops on a prompt, and ends with next: where the ending decides what to do.
A refused action armed nothing. Watch arms one by hand.`
}

func (c *WatchCmd) Validate() error {
	if (c.Session == "") == !c.Due {
		return errors.New("give a SESSION or --due")
	}
	return nil
}

func (c *WatchCmd) act(ctx *kong.Context, d *drive.Driver) error {
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
	runs
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

func (c *TimerCmd) act(ctx *kong.Context, d *drive.Driver) error {
	after, note := time.Duration(c.Seconds)*time.Second, strings.Join(c.Note, " ")
	if err := d.Timer(after, note); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "timer armed: the driver is woken in %s (%s)\n", after, note)
	return nil
}

type ParkCmd struct {
	runs
	View   string   `arg:"" help:"The view the park was decided on."`
	Reason []string `arg:"" help:"Why it waits, such as what only the owner can give; the record keeps it."`
}

func (c *ParkCmd) Help() string {
	return `
A parked session sends no wakes until a step is sent to it, so a session
waiting on the owner, or on another session's hold, stops waking the driver.`
}

func (c *ParkCmd) act(ctx *kong.Context, d *drive.Driver) error {
	if err := d.Park(c.View, strings.Join(c.Reason, " ")); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "parked %s: no wake until a step is sent to it\n", sessionOf(c.View))
	return nil
}

type CloseCmd struct {
	runs
	View string `arg:"" help:"The view the close was decided on."`
}

func (c *CloseCmd) Help() string {
	return `
Refused while the session works, runs a background job, or holds a typed
line nobody sent: next: gives what to do first, such as the step that has
the session stop its job.`
}

func (c *CloseCmd) act(ctx *kong.Context, d *drive.Driver) error {
	if err := d.Close(c.View); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "closed %s\n", sessionOf(c.View))
	return nil
}

type RecordCmd struct {
	Grant  GrantCmd  `cmd:"" help:"Record authority the owner handed over, in their words, with what it covers."`
	Ask    AskCmd    `cmd:"" help:"Record a question put to the owner, with its recommendation and what happens unanswered."`
	Answer AnswerCmd `cmd:"" help:"Record the owner's answer to an ask."`
	Note   NoteCmd   `cmd:"" help:"Record a note, such as the plan, for the run after a reset to read."`
	Goal   GoalCmd   `cmd:"" help:"Record what a session sets out to do, in plain terms for the owner."`
}

func (RecordCmd) Help() string {
	return `
The record is the batch's memory: status lists the grants and open asks,
log has every record, and metrics measures the asks. Each prints the id it
recorded, such as A3, which record answer takes.`
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
	runs
	Source string `required:"" help:"The owner's words, and where and when they said them."`
	Scope  string `required:"" help:"What the grant covers."`
}

func (c *GrantCmd) Help() string {
	return `
A grant lives as long as the batch it was given in: a new batch starts with
none.`
}

func (c *GrantCmd) act(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Grant(c.Source, c.Scope)
	return recorded(ctx.Stdout, "grant", id, err)
}

type AskCmd struct {
	runs
	Session        string `arg:"" help:"The session the question holds, or - for the batch."`
	Question       string `required:"" help:"The question, as the owner reads it."`
	Recommendation string `help:"What you recommend."`
	IfUnanswered   string `required:"" help:"What happens while nobody answers."`
}

func (c *AskCmd) act(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Ask(strings.TrimPrefix(c.Session, "-"), c.Question, c.Recommendation, c.IfUnanswered)
	return recorded(ctx.Stdout, "ask", id, err)
}

type AnswerCmd struct {
	runs
	Ask       string `arg:"" help:"The ask it answers, such as A3."`
	Source    string `help:"The owner's words, or what showed the ask no longer holds."`
	Took      string `required:"" enum:"recommended,amended,delegated,moot" help:"Whether the owner took the recommendation, amended it, handed the question to someone else, or it stopped mattering."`
	Explained bool   `help:"The owner needed the question explained before answering."`
}

func (c *AnswerCmd) act(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Answer(c.Ask, c.Source, c.Took, c.Explained)
	return recorded(ctx.Stdout, "answer", id, err)
}

type NoteCmd struct {
	runs
	Session string   `placeholder:"SESSION" help:"The session the note is about."`
	Text    []string `arg:"" help:"The note."`
}

func (c *NoteCmd) act(ctx *kong.Context, d *drive.Driver) error {
	id, err := d.Note(c.Session, strings.Join(c.Text, " "))
	return recorded(ctx.Stdout, "note", id, err)
}

type GoalCmd struct {
	runs
	Session string   `arg:"" help:"The session, by its label in the batch; a closed one too."`
	Text    []string `arg:"" help:"What it sets out to do, in plain terms for the owner: one short paragraph, at most 600 bytes."`
}

func (c *GoalCmd) Help() string {
	return `
It replaces the session's goal, which open --goal set or no command did: for
a session that joined the batch on a send, or once its purpose or scope
changed. status --json and the page show the latest.`
}

func (c *GoalCmd) act(ctx *kong.Context, d *drive.Driver) error {
	if err := d.Goal(c.Session, strings.Join(c.Text, " ")); err != nil {
		return err
	}
	_, err := fmt.Fprintf(ctx.Stdout, "recorded goal %s\n", c.Session)
	return err
}

type LogCmd struct {
	runs
	Since string `placeholder:"TIME" help:"Print only what happened from this time on, a date or a date and time such as \"2026-10-22 16:00\", read in the zone drive prints times in."`
	JSON  bool   `help:"Print the events as the batch recorded them, as one JSON array."`
}

func (c *LogCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
The batch's events, oldest first, as recorded: at, kind, and the fields its kind carries,
among session, block, sid, account, dir, intent, mode, key, path, text, notice, outcome,
reason, hold, view, action, call, id, ref, source, scope, question, recommendation,
if_unanswered, took, explained, note, goal, http, try.`}}
}

func (c *LogCmd) act(ctx *kong.Context, d *drive.Driver) error {
	var since time.Time
	if c.Since != "" {
		// Read in the zone drive prints times in, so a time copied from its output means what it said.
		zone := time.Local
		if b, err := d.Batch(); err == nil {
			zone = b.Zone()
		}
		var err error
		if since, err = parseWhen(c.Since, zone); err != nil {
			return clikit.Usage(err)
		}
	}
	b, evs, err := d.Log(since)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(ctx.Stdout, drive.LogJSON(evs))
	}
	_, err = io.WriteString(ctx.Stdout, drive.LogText(b, evs, d.Time()))
	return err
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
	runs
	Listen string `default:"127.0.0.1:8792" placeholder:"ADDR" help:"The address to serve it on."`
}

func (c *PageCmd) Help() string {
	return `
Each request reads the batch: the current one, or the one --batch pins.
The page shows each session's goal, where it stands, the pull requests it
opened or named and their state, the questions waiting on the owner, and
the sessions closed since; it changes nothing. A read is reused for 30
seconds, and one that takes longer than 20 is answered from the batch's
record while it finishes.

It runs until stopped, and exits once the drive binary it runs is
replaced, so a supervisor such as launchd restarts it on the new one. It
answers anyone who reaches the address: on the mini, a loopback address
that tailscale serve publishes on the tailnet.`
}

// replacedEvery is how often a running page checks whether its binary was
// replaced by an install.
var replacedEvery = 30 * time.Second

func (c *PageCmd) act(ctx *kong.Context, d *drive.Driver) error {
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
	runs
	JSON bool `help:"Print one JSON document instead of text."`
}

func (c *MetricsCmd) HelpSections() []clikit.Section {
	return []clikit.Section{{Title: "JSON", Body: `
batch, from, to, sessions; asks, answered, unanswered, took (counts of recommended,
amended, delegated, moot), explained; owner_wait_seconds, median_answer_seconds,
per_merge_seconds; merged (repo, number or branch and commit, at, sessions), unread;
sends, snippets, by_key, commands, own_words, recovered, garbled, other, unknown;
refusals (counts by kind), failures; wakes, timers, wake_fails, unhandled, unwatched`}}
}

func (c *MetricsCmd) act(ctx *kong.Context, d *drive.Driver) error {
	m, err := d.Metrics()
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(ctx.Stdout, m.JSON())
	}
	_, err = io.WriteString(ctx.Stdout, m.Text())
	return err
}

type BatchCmd struct {
	Show BatchShowCmd `cmd:"" default:"1" help:"Print the batch: its start, wakes, Rex session, zone, sessions and directory."`
	New  BatchNewCmd  `cmd:"" help:"Start a batch and make it current."`
	Use  BatchUseCmd  `cmd:"" help:"Make a batch current."`
	List BatchListCmd `cmd:"" help:"List every batch, the current one starred."`
}

type BatchShowCmd struct{ runs }

func (c *BatchShowCmd) act(ctx *kong.Context, d *drive.Driver) error {
	b, err := d.Batch()
	if err != nil {
		return err
	}
	_, err = io.WriteString(ctx.Stdout, drive.BatchText(b))
	return err
}

type BatchNewCmd struct {
	runs
	Name       string `arg:"" help:"The batch's name, such as the date."`
	Webhook    string `placeholder:"DRIVER" help:"Wake this chat driver by its webhook, the file ~/.config/drive/webhooks/DRIVER that the owner writes. Without it, the driver waits itself."`
	RexSession string `placeholder:"SESSION" help:"The Rex session new sessions open in, started if it is not running; defaults to the configuration's, then $REX_SESSION."`
	TZ         string `name:"tz" placeholder:"ZONE" help:"The zone drive prints the batch's times in, such as Europe/London; defaults to the configuration's, then the machine's."`
	AfterWake  string `placeholder:"COMMAND" help:"A shell command to run after each wake is posted, such as archiving the chat it went to."`
}

func (c *BatchNewCmd) Help() string {
	return `
A new batch starts with no grants, however recently one was given, and
stops the last batch's wakes. Start one per drive, never after a reset:
` + "`drive batch`" + ` names the current one.`
}

func (c *BatchNewCmd) Validate() error {
	if c.TZ != "" {
		if _, err := time.LoadLocation(c.TZ); err != nil {
			return fmt.Errorf("--tz %q is not a zone name such as Europe/London", c.TZ)
		}
	}
	return nil
}

func (c *BatchNewCmd) act(ctx *kong.Context, d *drive.Driver) error {
	wake := "none"
	if c.Webhook != "" {
		wake = "webhook"
	}
	var after []string
	if c.AfterWake != "" {
		after = []string{c.AfterWake}
	}
	st, err := d.StartBatch(drive.BatchOptions{Name: c.Name, Wake: wake, Webhook: c.Webhook, RexSession: c.RexSession, TZ: c.TZ, AfterWake: after})
	if err != nil {
		return err
	}
	_, err = io.WriteString(ctx.Stdout, st.Text())
	return err
}

type BatchUseCmd struct {
	runs
	Name string `arg:"" help:"The batch to make current."`
}

func (c *BatchUseCmd) act(ctx *kong.Context, d *drive.Driver) error {
	if err := d.UseBatch(c.Name); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "batch %s is current\n", c.Name)
	return nil
}

type BatchListCmd struct{ runs }

func (c *BatchListCmd) act(ctx *kong.Context, d *drive.Driver) error {
	metas, cur, err := d.Batches()
	if err != nil {
		return err
	}
	_, err = io.WriteString(ctx.Stdout, drive.BatchesText(metas, cur))
	return err
}

type SnippetCmd struct {
	runs
	Key  string   `arg:"" help:"The snippet's key, such as land-pr."`
	Dir  string   `arg:"" optional:"" default:"." help:"The directory it resolves in: a project's own snippets win over the owner's global ones."`
	Text []string `arg:"" optional:"" help:"The text the owner would type after the key."`
}

func (c *SnippetCmd) Help() string {
	return `
It prints the expanded text on stdout and the file it came from on stderr,
to check what a send --snippet would type before sending it.`
}

func (c *SnippetCmd) act(ctx *kong.Context, d *drive.Driver) error {
	s, err := d.Snippet(c.Dir, c.Key, strings.Join(c.Text, " "))
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stderr, "from: %s\n", s.From)
	fmt.Fprintln(ctx.Stdout, s.Text)
	return nil
}

type SnipsCmd struct {
	runs
	Dir string `arg:"" optional:"" default:"." help:"The directory whose keys to list."`
}

func (c *SnipsCmd) act(ctx *kong.Context, d *drive.Driver) error {
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
	runs
	Label string `arg:"" help:"The session to wait for."`
}

func (c *NotifyCmd) act(ctx *kong.Context, d *drive.Driver) error {
	watch := onKill(d, c.Label)
	e, err := d.Notify(watch, c.Label, time.Minute, 10*time.Minute)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "%s woke the driver for %s (%s)\n", d.Time().Format(time.RFC3339), e.Label, e.State)
	_, err = io.WriteString(ctx.Stdout, e.Text())
	return err
}

type RingCmd struct {
	runs
	Seconds uint     `arg:"" help:"How long to sleep."`
	Note    []string `arg:"" help:"The timer's note."`
}

func (c *RingCmd) act(ctx *kong.Context, d *drive.Driver) error {
	note := strings.Join(c.Note, " ")
	watch := onKill(d, "timer "+note)
	time.Sleep(time.Duration(c.Seconds) * time.Second)
	if err := d.Ring(watch, note); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "timer done: %s\n", note)
	return nil
}
