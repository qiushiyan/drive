// Command drive drives Claude Code sessions in Rex for a driver standing in
// for their owner: a chat bot or a Claude Code session.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
)

const usage = `drive — drive Claude Code sessions in Rex: read where each stands, send its
next step, answer its prompts, and be woken when its turn ends.

Every action on a session is decided on a VIEW, such as T1@3fa91c07, which
read, wait, status and every wake print, and is refused once the session has
moved past it: a step decided on an old turn never reaches a new one. A
working session's view changes as it writes, so decide on a turn that has
ended, or send with --queue, which needs only that no prompt came since. One
driver acts on a session at a time.

  drive status [--json] [--quick]          every session in the batch, live
  drive read SESSION [MODE] [--json]       its newest reply, its view, and next: where its
                                           state decides it; MODE is one of
           --prompts [N]  what it was told   --pending  the call a prompt waits on
           --user         its newest prompt  --screen   the end of its screen
  drive wait SESSION [--quiet S] [--bg S]  block until its turn ends; prints the reply's end
  drive send VIEW STEP [--queue] [--hold NAME] [--as LABEL]
  drive open LABEL [STEP] [--dir DIR] [--resume ID] [--account NAME] [--model M] [--hold NAME]
  drive respond VIEW approve|deny|interrupt|clear
  drive watch SESSION | --due | --sleep SECONDS NOTE
                                           a webhook batch: wake the driver when the turn ends;
                                           --due arms all that need it; --sleep is a timer
  drive park VIEW REASON…                  no wakes until a step is sent; the reason is kept
  drive close VIEW                         close its block after its last step
  drive record grant --source WORDS --scope WHAT
  drive record ask SESSION --question Q --recommendation R --if-unanswered WHAT
  drive record answer ASK --source WORDS --took recommended|amended|delegated|moot [--explained]
  drive record note [--session S] TEXT…
  drive log [--since TIME] [--json]        the batch's timeline
  drive metrics [BATCH] [--json]           asks, owner wait, merges, sends, failures
  drive batch [new NAME [--wake webhook|none] [--rex-session S] [--tz ZONE] [--after-wake CMD]
              | use NAME | list]               the current batch and its directory; a new one starts
                                           with no grants and stops the last one's wakes
  drive snippet KEY [DIR] [TEXT…] | drive snippet --list [DIR]

STEP is one of:
  'one short line'        typed as is, a slash command too ('/write-spec Go with D1')
  --file PATH             sent as "Read <copy> and do what it asks."; drive keeps the copy
  --snippet KEY [TEXT…]   the owner's TabType snippet for the session's directory, with
                          TEXT where the owner would type after it
The first step a session gets in a batch carries the notice ahead of it.

--queue sends to a working session, which takes it mid-turn; a session on a
prompt takes nothing until it is answered. --hold NAME is for what only one
session may do at a time, such as a release or commits into a shared
checkout: held from the send until the turn the step starts has ended,
background shells that turn started included.

Wakes, in a webhook batch: any send, open or respond arms the session's
wake. It comes about a minute after the turn ends, at once on an API error
or an interrupt, and when the session stops on a prompt; it ends with next:
where the ending decides what to do. A refused action armed nothing.

status columns:
  STATE  working | done | idle (no prompt yet) | interrupted (an interrupt or a deny)
         | error | blocked:permission | blocked:question | blocked:auth (a rejected login)
  INPUT  empty | suggestion (Claude Code's dimmed guess; typing replaces it)
         | typed (a line left unsent) | prompt (a prompt's options) | unknown (no box on screen)
  WAKE   armed | stuck | posting | woken HH:MM (nothing done since): act on it or park it
         | none, ended without a wake: no wake comes until drive watch --due | parked: why
  hold lines name who holds each hold and who waits for it with the step it was refused

SESSION is a label in the batch (T1) or a block id (its last six characters).
--batch NAME acts in that batch instead of the current one.

A refusal or failure that leaves something to do ends with next:, a command
that runs as written on the session as it stands. Exit status: 0 done;
1 refused, nothing was done; 2 usage; 3 tried and did not end as asked, the
message saying what did happen; 4 the session's login was rejected, the
message printing the relaunch.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	batchName := ""
	if len(argv) >= 2 && argv[0] == "--batch" {
		batchName, argv = argv[1], argv[2:]
	}
	if len(argv) == 0 || argv[0] == "--help" || argv[0] == "-h" || argv[0] == "help" {
		fmt.Print(usage)
		return 0
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	d := newDriver(cfg)
	d.BatchName = batchName
	cmd, rest := argv[0], argv[1:]
	for _, a := range rest {
		if a == "--help" || a == "-h" {
			fmt.Print(usage)
			return 0
		}
	}
	switch cmd {
	case "status":
		return cmdStatus(d, rest)
	case "read":
		return cmdRead(d, rest)
	case "wait":
		return cmdWait(d, rest)
	case "send":
		return cmdSend(d, rest)
	case "open":
		return cmdOpen(d, rest)
	case "respond":
		return cmdRespond(d, rest)
	case "watch":
		return cmdWatch(d, rest)
	case "park":
		return cmdPark(d, rest)
	case "close":
		return cmdClose(d, rest)
	case "record":
		return cmdRecord(d, rest)
	case "log":
		return cmdLog(d, rest)
	case "metrics":
		return cmdMetrics(d, rest)
	case "batch":
		return cmdBatch(d, rest)
	case "snippet":
		return cmdSnippet(d, rest)
	}
	return bad(fmt.Sprintf("unknown command %q", cmd))
}

// newDriver wires a driver to this machine; tests wire it to a fake one.
var newDriver = func(cfg config.Config) *drive.Driver {
	self, _ := os.Executable()
	return &drive.Driver{
		Term: rex.CLI{}, Host: session.Machine{Home: cfg.Home}, Store: batch.Store{Dir: cfg.StateDir},
		Cfg: cfg, Progress: progress.Tools{}, Watches: drive.Detached{Dir: filepath.Join(cfg.StateDir, "watches")}, Self: self,
	}
}

func bad(msg string) int {
	fmt.Fprintf(os.Stderr, "drive: %s; `drive --help` lists the commands\n", msg)
	return 2
}

// fail prints an error and returns its exit status.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "drive: "+err.Error())
	var r *drive.Refused
	var f *drive.Failed
	switch {
	case errors.As(err, &r):
		return 1
	case errors.As(err, &f) && f.Login:
		return 4
	default:
		return 3
	}
}

func parseOr(argv []string, valued, switches []string) (*args, int) {
	a, err := parse(argv, valued, switches)
	if err != nil {
		return nil, bad(err.Error())
	}
	return a, -1
}

func seconds(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a number of seconds", s)
	}
	return time.Duration(n) * time.Second, nil
}

// step reads a STEP from a command's positional words and flags.
func step(a *args, words []string) (drive.Step, error) {
	n := 0
	var s drive.Step
	if f := a.str("file"); f != "" {
		s.File, n = f, n+1
	}
	if k := a.str("snippet"); k != "" {
		s.Key, s.Text, n = k, strings.Join(words, " "), n+1
		words = nil
	}
	if len(words) > 0 {
		s.Line, n = strings.Join(words, " "), n+1
	}
	if n > 1 {
		return s, errors.New("give one step: a line, --file PATH or --snippet KEY")
	}
	return s, nil
}

func cmdSend(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"file", "snippet", "hold", "as"}, []string{"queue", "no-watch"})
	if a == nil {
		return code
	}
	if len(a.pos) == 0 {
		return bad("send needs a VIEW and a step")
	}
	s, err := step(a, a.pos[1:])
	if err != nil {
		return bad(err.Error())
	}
	if s.Line == "" && s.File == "" && s.Key == "" {
		return bad("send needs a step: a line, --file PATH or --snippet KEY")
	}
	sent, err := d.Send(a.pos[0], s, drive.SendOptions{Queue: a.on("queue"), Hold: a.str("hold"), Label: a.str("as"), NoWatch: a.on("no-watch")})
	if sent != nil && sent.Outcome != "" {
		printSent(sent)
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdOpen(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"file", "snippet", "hold", "dir", "resume", "account", "model"}, nil)
	if a == nil {
		return code
	}
	if len(a.pos) == 0 {
		return bad("open needs a LABEL")
	}
	s, err := step(a, a.pos[1:])
	if err != nil {
		return bad(err.Error())
	}
	dir := a.str("dir")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	o, err := d.Open(a.pos[0], s, drive.OpenOptions{Dir: config.Expand(d.Cfg.Home, dir), Resume: a.str("resume"), Account: a.str("account"), Model: a.str("model"), Hold: a.str("hold")})
	if o != nil {
		fmt.Printf("opened %s in block %s on account %s (session %s)\n", o.Label, rex.Short(o.Block), o.Account, o.Session)
		if o.Sent != nil && o.Sent.Outcome != "" {
			printSent(o.Sent)
		}
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func printSent(s *drive.Sent) {
	what := s.Typed
	if s.Path != "" {
		what = s.Path
	}
	extra := ""
	if s.Notice {
		extra += ", notice first"
	}
	if s.From != "" {
		extra += ", snippet from " + s.From
	}
	if s.Watched {
		extra += "; wake armed"
	}
	fmt.Printf("%s %s: %s%s\n", s.Outcome, s.Label, what, extra)
}

func cmdRespond(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, nil)
	if a == nil {
		return code
	}
	if len(a.pos) != 2 {
		return bad("respond needs a VIEW and one of approve, deny, interrupt, clear")
	}
	r, err := d.Respond(a.pos[0], a.pos[1])
	if r != nil {
		fmt.Printf("%s %s", r.Action, r.Label)
		if r.Call != "" {
			fmt.Printf(" (%s)", r.Call)
		}
		fmt.Printf("; it now reads %s\n", r.After)
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdWatch(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"sleep"}, []string{"due"})
	if a == nil {
		return code
	}
	if a.on("due") {
		armed, err := d.Due()
		for _, r := range armed {
			fmt.Printf("watching %s: %s\n", r.Label, r.Why)
			if r.Failures >= 2 {
				fmt.Printf("  %s's watch has ended without a wake %d times this batch; watches.log in the batch directory says why\n", r.Label, r.Failures)
			}
		}
		if err != nil {
			return fail(err)
		}
		if len(armed) == 0 {
			fmt.Println("no session needs its wake armed")
		}
		return 0
	}
	if s := a.str("sleep"); s != "" {
		after, err := seconds(s, 0)
		if err != nil || len(a.pos) == 0 {
			return bad("watch --sleep needs SECONDS and a NOTE, such as: drive watch --sleep 300 veto")
		}
		if err := d.Timer(after, strings.Join(a.pos, " ")); err != nil {
			return fail(err)
		}
		fmt.Printf("timer armed: the driver is woken in %s (%s)\n", after, strings.Join(a.pos, " "))
		return 0
	}
	if len(a.pos) != 1 {
		return bad("watch needs a SESSION, --due, or --sleep SECONDS NOTE")
	}
	w, err := d.Watch(a.pos[0])
	if err != nil {
		return fail(err)
	}
	if w.Restarted {
		fmt.Printf("%s's watch was stuck long after its turn ended; started it again\n", w.Label)
	} else if w.Already {
		fmt.Printf("%s is already watched\n", w.Label)
	} else {
		fmt.Printf("watching %s: the driver is woken when its turn ends\n", w.Label)
	}
	return 0
}

func cmdPark(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, nil)
	if a == nil {
		return code
	}
	if len(a.pos) < 2 {
		return bad("park needs a VIEW and the REASON")
	}
	if err := d.Park(a.pos[0], strings.Join(a.pos[1:], " ")); err != nil {
		return fail(err)
	}
	fmt.Printf("parked %s: no wake until a step is sent to it\n", strings.SplitN(a.pos[0], "@", 2)[0])
	return 0
}

func cmdClose(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, nil)
	if a == nil {
		return code
	}
	if len(a.pos) != 1 {
		return bad("close needs a VIEW")
	}
	if err := d.Close(a.pos[0]); err != nil {
		return fail(err)
	}
	fmt.Printf("closed %s\n", strings.SplitN(a.pos[0], "@", 2)[0])
	return 0
}
