// Command drive drives Claude Code sessions in Rex for a driver standing in
// for their owner: a chat bot or a Claude Code session.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
)

type CLI struct {
	Batch  string `placeholder:"NAME" help:"Act in this batch, as drive prints its name, instead of the driver's current one. It may stand anywhere on the line."`
	Driver string `placeholder:"NAME" help:"Act in the batch this driver drives now; a chat driver's name is its webhook's. It may stand anywhere on the line."`

	Status  StatusCmd  `cmd:"" help:"Print every session in the batch, live: its state, stage, pull request, wake and view."`
	Read    ReadCmd    `cmd:"" help:"Print a session's newest reply, where it stands, its view, and next: where its state decides it."`
	Wait    WaitCmd    `cmd:"" help:"Block until a session's turn ends, then print how it ended."`
	Send    SendCmd    `cmd:"" help:"Send a session its next step, decided on a view."`
	Open    OpenCmd    `cmd:"" help:"Open a new Claude Code session in Rex, in the batch, with an optional first step."`
	Respond RespondCmd `cmd:"" help:"Answer the prompt a session waits on."`
	Watch   WatchCmd   `cmd:"" help:"Arm a session's wake in a batch that wakes a chat driver; --due arms every one that needs it."`
	Timer   TimerCmd   `cmd:"" help:"Wake the driver after a delay, with a note: a veto window, a check that is due later."`
	Park    ParkCmd    `cmd:"" help:"Stop a session's wakes until a step is sent to it, keeping why."`
	Close   CloseCmd   `cmd:"" help:"Close a session's block after its last step."`
	Record  RecordCmd  `cmd:"" help:"Record what drive cannot see: the owner's grants, asks and answers, notes, and each session's goal."`
	Log     LogCmd     `cmd:"" help:"Print the batch's timeline: what was sent, answered, opened, parked and closed, and every wake."`
	Metrics MetricsCmd `cmd:"" help:"Print the batch's measures: asks and the owner's wait, merges, sends, wakes, refusals."`
	Page    PageCmd    `cmd:"" help:"Serve the live batches as a web page their owner opens: each session's goal, where it stands, its pull requests, and what waits on them."`
	Batches BatchCmd   `cmd:"" name:"batch" help:"Print a driver's batch, or start, switch, end or list batches."`
	Snippet SnippetCmd `cmd:"" help:"Print the owner's snippet KEY as a session in DIR would receive it."`
	Snips   SnipsCmd   `cmd:"" name:"snippets" help:"List the snippet keys that resolve in DIR, each with its file."`

	Notify NotifyCmd `cmd:"" hidden:"" help:"A watch's own run: wait for the turn to end, then post the wake."`
	Ring   RingCmd   `cmd:"" hidden:"" help:"A timer's own run: sleep, then post the wake."`
}

func (CLI) Help() string {
	return `
The driver stands in for the sessions' owner, usually while they are away:
a chat bot woken by webhook, or a Claude Code session. A batch is one
drive's sessions and its record; ` + "`drive batch new NAME`" + ` starts one.

Every action on a session is decided on a VIEW, such as T1@3fa91c07, which
read, wait, status and every wake print. The action is refused once the
session has moved past that view, so a step decided on an old turn never
reaches a new one. A working session's view changes as it writes: decide on
a turn that has ended, or send with --queue. One driver acts on a session at
a time.

A SESSION is a label in the batch (T1), or a session's Rex block id, whose
last six characters are enough, before it joins the batch; its view starts
the same way (7c1e2a@3fa91c07). Copy a view as drive printed it.

A DRIVER drives a batch of its own. A chat bot is named by its webhook and
names itself on every command, since nothing in its shell tells two bots
apart:

  drive --driver itell status          # the batch driver itell drives now
  drive --batch 2026-10-27 read T1     # one batch by name, as next: prints it

A command that names neither acts in the batch of the driver with no name,
and is refused while any named driver drives one.

A refusal or failure that leaves something to do ends with a next: line: a
command that runs as written on the session as it stands, naming the batch
it acts in. Whether the step still makes sense is the driver's judgment.`
}

// wiring builds the driver an invocation acts through: this machine's in
// main, a fake world's in the tests.
type wiring func(config.Config) *drive.Driver

func machine(cfg config.Config) *drive.Driver {
	self, _ := os.Executable()
	return &drive.Driver{
		Term: rex.CLI{}, Host: session.Machine{Home: cfg.Home}, Store: batch.Store{Dir: cfg.StateDir},
		Cfg: cfg, Progress: progress.Tools{}, Watches: drive.Detached{Dir: filepath.Join(cfg.StateDir, "watches")}, Self: self,
	}
}

var app = clikit.App{
	Name:        "drive",
	Description: "drive runs Claude Code sessions in Rex for their owner: read where each stands, send its next step, answer its prompts, and be woken when its turn ends.",
	New:         func() any { return &CLI{} },
	Options: []kong.Option{
		kong.Bind(wiring(machine)),
		// The configuration loads only once a command acts, so help and
		// usage errors work whatever state it is in.
		kong.BindToProvider(func(cli *CLI, wire wiring, ctx *kong.Context) (*drive.Driver, error) {
			cfg, err := config.Load()
			if err != nil {
				return nil, err
			}
			d := wire(cfg)
			d.BatchName, d.DriverName, d.Line = cli.Batch, cli.Driver, unselected(ctx.Args)
			return d, nil
		}),
	},
	FailureCode: 3,
	ExitCodes: map[int]string{
		1: "refused: nothing was done; next: says what to run instead",
		3: "tried and did not end as asked; the message says what did happen",
		4: "the session's login was rejected; next: relaunches it on another account, or says to tell the owner",
	},
}

func main() { os.Exit(app.Run(upgrade(os.Args[1:]))) }

// actor is a command's work. Every command embeds runs, whose Run calls the
// selected command's act and classifies what it returns, so no command can
// let a refusal exit as a failure.
type actor interface {
	act(ctx *kong.Context, d *drive.Driver) error
}

type runs struct{}

func (runs) Run(ctx *kong.Context, d *drive.Driver) error {
	return classify(ctx.Selected().Target.Addr().Interface().(actor).act(ctx, d))
}

// classify gives an error drive's exit status: 1 for a refusal, which did
// nothing, 4 for a rejected login, and 3 for anything else that failed.
func classify(err error) error {
	if r, ok := errors.AsType[*drive.Refused](err); ok {
		return clikit.Fail(1, r)
	}
	if f, ok := errors.AsType[*drive.Failed](err); ok && f.Login {
		return clikit.Fail(4, f)
	}
	return err
}

// upgrade rewrites the command lines an older drive still runs into this
// one's: a watch or timer it started before an install re-execs this
// binary by the old form, and a wake it posts may say `drive watch --sleep`.
// Each is an exact shape only an older drive wrote; anything else passes
// through untouched.
func upgrade(args []string) []string {
	batchArgs, rest := splitBatch(args)
	switch {
	case len(rest) == 3 && rest[0] == "wait" && rest[2] == "--notify":
		// drive --batch B wait LABEL --notify
		return append(batchArgs, "notify", rest[1])
	case len(rest) == 6 && rest[0] == "wait" && rest[1] == "--sleep" && rest[3] == "--note" && rest[5] == "--notify":
		// drive --batch B wait --sleep SECONDS --note NOTE --notify
		return append(batchArgs, "ring", rest[2], "--", rest[4])
	case len(rest) >= 4 && rest[0] == "watch" && rest[1] == "--sleep":
		// drive watch --sleep SECONDS NOTE…, as an older wake's next: wrote it
		return append(append(batchArgs, "timer", rest[2], "--"), rest[3:]...)
	}
	return args
}

// unselected is a command line without the flags that select its batch,
// which stand anywhere before a -- that ends the flags.
func unselected(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(out, args[i:]...)
		case a == "--batch" || a == "--driver":
			i++
		case strings.HasPrefix(a, "--batch=") || strings.HasPrefix(a, "--driver="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// splitBatch takes a leading --batch NAME off a command line.
func splitBatch(args []string) (batchArgs, rest []string) {
	if len(args) >= 2 && args[0] == "--batch" {
		return slices.Clone(args[:2]), args[2:]
	}
	return nil, args
}
