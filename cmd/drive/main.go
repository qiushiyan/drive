// Command drive drives Claude Code sessions in Rex for a driver standing in
// for their owner: a chat bot or a Claude Code session.
package main

import (
	"errors"
	"fmt"
	"os"
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

Every action on a session is decided on a VIEW: read, wait and status print
one (T1@3fa91c07), and an action is refused once the session has moved past
it, so a step decided on an old turn never reaches a new one.

  drive status [--json] [--quick]          every session in the batch, live
  drive read SESSION [--prompts [N] | --pending | --user] [--json]
                                           its newest reply, or what it was told, the
                                           call a prompt waits on, its newest prompt
  drive wait SESSION [--quiet S] [--bg S]  block until its turn ends; prints the reply's end
  drive send VIEW STEP [--queue] [--hold NAME] [--as LABEL]
  drive open LABEL [STEP] [--dir DIR] [--resume ID] [--account NAME] [--hold NAME]
  drive respond VIEW approve|deny|interrupt|clear
  drive watch SESSION | drive watch --sleep SECONDS NOTE
                                           a webhook batch: wake the driver when the turn
                                           ends (send and open arm this themselves)
  drive park SESSION REASON…               stop waking for it; the reason is kept
  drive close VIEW                         close its block after its last step
  drive record grant --source WORDS --scope WHAT
  drive record ask SESSION --question Q --recommendation R --if-unanswered WHAT
  drive record answer ASK --source WORDS --took recommended|amended|delegated|moot [--explained]
  drive record note [--session S] TEXT…
  drive log [--since TIME] [--json]        the batch's timeline
  drive metrics [BATCH] [--json]           asks, owner wait, merges, sends, failures
  drive batch [new NAME [--wake webhook|none] [--rex-session S] [--tz ZONE] | use NAME | list]
  drive snippet KEY [DIR] [TEXT…] | drive snippet --list [DIR]

STEP is one of:
  'one short line'        typed as is, a slash command too ('/write-spec Go with D1')
  --file PATH             sent as "Read <copy> and do what it asks."; drive keeps the copy
  --snippet KEY [TEXT…]   the owner's TabType snippet for the session's directory, with
                          TEXT where the owner would type after it
The first step a session gets in a batch carries the notice ahead of it.

--queue sends to a working session, which takes it mid-turn. --hold NAME takes
a resource only one session may use at a time (a release, a closeout into one
checkout) until the turn this step starts has ended.

SESSION is a label in the batch (T1) or a block id (its last six characters).
--batch NAME acts in that batch instead of the current one.

Exit status: 0 done; 1 refused, nothing done (the message says why and what to
do); 2 usage; 3 tried and did not end as asked; 4 the session's login or API
failed (relaunch it on another account).
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
	self, _ := os.Executable()
	d := &drive.Driver{
		Term: rex.CLI{}, Host: session.Machine{Home: cfg.Home}, Store: batch.Store{Dir: cfg.StateDir},
		Cfg: cfg, Progress: progress.Tools{}, Watches: drive.Tmux{}, Self: self, BatchName: batchName,
	}
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
	a, code := parseOr(argv, []string{"file", "snippet", "hold", "dir", "resume", "account"}, nil)
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
	o, err := d.Open(a.pos[0], s, drive.OpenOptions{Dir: config.Expand(d.Cfg.Home, dir), Resume: a.str("resume"), Account: a.str("account"), Hold: a.str("hold")})
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
	a, code := parseOr(argv, []string{"sleep"}, nil)
	if a == nil {
		return code
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
		return bad("watch needs a SESSION, or --sleep SECONDS NOTE")
	}
	w, err := d.Watch(a.pos[0])
	if err != nil {
		return fail(err)
	}
	if w.Already {
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
		return bad("park needs a SESSION and the REASON")
	}
	if err := d.Park(a.pos[0], strings.Join(a.pos[1:], " ")); err != nil {
		return fail(err)
	}
	fmt.Printf("parked %s: no wake until a step is sent to it\n", a.pos[0])
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
