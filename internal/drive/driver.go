// Package drive is what a driver does to the Claude Code sessions it drives:
// read where each stands, send its next step, answer a prompt, wait for its
// turn, close it. Each action is decided on a view the driver read and is
// refused once the session has moved past it; it is admitted under the
// batch's lock, recorded before it touches the session, and confirmed from
// the session's transcript before it is reported done. What to send and
// when stays the driver's judgment.
package drive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// Driver holds what every action needs.
type Driver struct {
	Term     rex.Terminal
	Host     session.Host
	Store    batch.Store
	Cfg      config.Config
	Progress progress.Source
	Watches  Watches
	Self     string // this executable, which a watch runs
	Now      func() time.Time
	Sleep    func(time.Duration)
	Poll     time.Duration // how often a wait looks; 2 s when zero

	// BatchName pins the batch an action works in; "" is the current one.
	// A watch carries it, so a wake from an old batch never acts in a new one.
	BatchName string
}

// Refused is an action drive would not take in the state the session is in.
// Nothing was done; Next says what would make it possible.
type Refused struct {
	Reason string
	Next   string

	account string // an account whose login the refusal found rejected
}

func (r *Refused) Error() string {
	if r.Next == "" {
		return r.Reason
	}
	return r.Reason + "\nnext: " + r.Next
}

// explain splits an error into what happened and the next it gives.
func explain(err error) (why, next string) {
	if r, ok := errors.AsType[*Refused](err); ok {
		return r.Reason, r.Next
	}
	if f, ok := errors.AsType[*Failed](err); ok {
		return f.What, f.Next
	}
	return err.Error(), ""
}

// Failed is an action that was tried and did not end as asked.
type Failed struct {
	What  string
	Next  string
	Login bool // the session's login or API was the failure, not the delivery
}

func (f *Failed) Error() string {
	if f.Next == "" {
		return f.What
	}
	return f.What + "\nnext: " + f.Next
}

// Time reads the driver's clock, which a test sets: every time drive prints
// or records comes from it.
func (d *Driver) Time() time.Time { return d.now() }

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Driver) sleep(t time.Duration) {
	if d.Sleep != nil {
		d.Sleep(t)
		return
	}
	time.Sleep(t)
}

func (d *Driver) poll() time.Duration {
	if d.Poll > 0 {
		return d.Poll
	}
	return 2 * time.Second
}

// zoneOf is where times are shown: the batch's zone, else this machine's.
func zoneOf(b *batch.Batch) *time.Location {
	if b != nil {
		return b.Zone()
	}
	return time.Local
}

// Batch is the batch actions work in.
func (d *Driver) Batch() (*batch.Batch, error) {
	if d.BatchName != "" {
		return d.Store.Get(d.BatchName)
	}
	return d.Store.Current()
}

// Observation is one session as it stands, read in a batch: what a message
// about it says, of its wake or of another account, is that batch's.
type Observation struct {
	in       *batch.Batch  // nil outside any batch
	Label    string        // the batch's label, or the block's short id outside the batch
	Member   *batch.Member // nil outside the batch
	Session  *session.Session
	Frontier turn.Frontier
	View     turn.View
	Input    turn.InputKind
	Typed    string // the input box's text, typed or suggested
}

// State is where the session's turn stands, in one word.
func (o *Observation) State() State { return turnState(o.Session, o.Frontier) }

func (o *Observation) zone() *time.Location { return zoneOf(o.in) }

// target resolves a label in the batch or a block id to the session's
// label and block, without reading the session.
func (d *Driver) target(members []*batch.Member, ref string) (label, block string, err error) {
	if m := batch.Find(members, ref); m != nil {
		return m.Label, m.Block, nil
	}
	id, err := d.findBlock(ref)
	if err != nil {
		return "", "", err
	}
	return rex.Short(id), id, nil
}

// own takes the lock key, a session's block or a label being opened, for
// one action described by what; release gives it back. A second driver
// acting meanwhile is refused with what the first is doing.
func (d *Driver) own(b *batch.Batch, label, key, what string) (release func(), err error) {
	release, err = d.Store.Own(key, what)
	if busy, ok := errors.AsType[*batch.Busy](err); ok {
		return nil, &Refused{Reason: fmt.Sprintf("another driver is %s (pid %d, since %s)", busy.What, busy.PID, busy.Since.In(zoneOf(b)).Format("15:04:05")),
			Next: fmt.Sprintf("leave %s to it; once it is done, %s shows where it stands", label, d.run("read", label))}
	}
	return release, err
}

// observe reads one session in batch b, whose sessions are members. ref is
// a label in the batch or a block id.
func (d *Driver) observe(b *batch.Batch, members []*batch.Member, ref string) (*Observation, error) {
	o := &Observation{in: b}
	label, block, err := d.target(members, ref)
	if err != nil {
		return nil, err
	}
	o.Label, o.Member = label, batch.Find(members, label)
	s, err := session.Locate(d.Term, d.Host, block)
	if errors.Is(err, session.ErrNoClaude) {
		return nil, &Refused{Reason: fmt.Sprintf("%s (block %s) has no Claude Code session running", o.Label, rex.Short(block)),
			Next: "it may have exited; " + d.run("status") + " lists the sessions that are running"}
	}
	if err != nil {
		return nil, &Refused{Reason: fmt.Sprintf("%s (block %s) cannot be read: %v", o.Label, rex.Short(block), err),
			Next: "the block may be gone; " + d.run("status") + " lists what is running"}
	}
	o.Session = s
	if o.Frontier, err = s.Frontier(); err != nil {
		return nil, fmt.Errorf("reading %s's transcript: %w", o.Label, err)
	}
	o.Frontier.Error = liveError(s, o.Frontier)
	o.View = turn.ViewOf(o.Label, s.ID, o.Frontier)
	if screen, err := d.Term.Capture(block, true); err == nil {
		o.Input, o.Typed = turn.InputBox(screen)
	}
	if o.State().onPrompt() && o.Input == turn.InputTyped {
		o.Input, o.Typed = turn.InputPrompt, "" // a prompt draws its options behind the box's ❯
	}
	return o, nil
}

func (d *Driver) findBlock(ref string) (string, error) {
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return "", err
	}
	var hits []string
	for _, b := range blocks {
		if b.ID == ref || (len(ref) >= 6 && strings.HasSuffix(b.ID, ref)) {
			hits = append(hits, b.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", &Refused{Reason: fmt.Sprintf("no session or block named %q", ref),
			Next: d.run("status") + " lists the batch's sessions by label and the other Claude sessions by block"}
	default:
		return "", &Refused{Reason: fmt.Sprintf("%q names %d blocks", ref, len(hits)), Next: "give more of the block id"}
	}
}

// inBatch is the batch an action works in, nil when there is none: a
// session outside any batch is still read and answered.
func (d *Driver) inBatch() (*batch.Batch, error) {
	b, err := d.Batch()
	if errors.Is(err, batch.ErrNoBatch) {
		return nil, nil
	}
	return b, err
}

// members reads the batch's sessions; with no batch there are none.
func (d *Driver) members() (*batch.Batch, []batch.Event, []*batch.Member, error) {
	b, err := d.inBatch()
	if b == nil || err != nil {
		return nil, nil, nil, err
	}
	evs, err := b.Events()
	if err != nil {
		return nil, nil, nil, err
	}
	return b, evs, batch.Members(evs), nil
}

// checkView refuses an action decided on a view the session has moved past,
// naming the prompt it moved on and, from the batch's record evs, the step
// drive sent that the prompt is: several runs act on one batch, and a run
// refused over another's step should know the session did not move alone.
// Queued sends only need the session to have received nothing since.
func (d *Driver) checkView(o *Observation, v turn.View, promptOnly bool, evs []batch.Event) error {
	zone := o.zone()
	cur := o.View
	if v.Prompt == cur.Prompt && (promptOnly || v.Reply == cur.Reply) {
		return nil
	}
	what, on := "it has written more since", ", answering "
	if v.Prompt != cur.Prompt {
		what, on = "it has received another prompt since", ": "
	}
	if p := o.Frontier.Newest(); p != nil {
		step := sentStep(evs, o.Label, *p, zone)
		if step == "" {
			step = fmt.Sprintf("its newest prompt (%s: %s)", p.At.In(zone).Format("15:04"), clip(promptText(*p), 80))
		}
		what += on + step
	}
	return &Refused{Reason: fmt.Sprintf("%s has moved past view %s: %s", o.Label, v, what),
		Next: "read it again and decide on what it shows now: " + d.run("read", o.Label)}
}

// sentStep names the step drive sent to label that prompt p is, as the
// batch's record evs has it: "" for a prompt drive did not send.
func sentStep(evs []batch.Event, label string, p turn.Prompt, zone *time.Location) string {
	if !p.Known {
		return ""
	}
	for _, e := range slices.Backward(evs) {

		if e.Kind != batch.KindSend || e.Session != label || e.Text == "" || strings.TrimSpace(e.Text) != strings.TrimSpace(p.Text) {
			continue
		}
		what := fmt.Sprintf("the step %q", clip(e.Text, 60))
		switch {
		case e.Key != "":
			what = "the " + e.Key + " snippet"
		case e.Mode == "notice":
			what = "the notice"
		case e.Mode == "file":
			what = "the file step"
		}
		return what + " drive sent at " + e.At.In(zone).Format("15:04")
	}
	return ""
}

func promptText(p turn.Prompt) string {
	if !p.Known {
		return "(a slash command, queued)"
	}
	return p.Text
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}

// otherAccounts names the accounts a session could be relaunched on: not
// the one it runs on, and none whose login this batch saw rejected. The
// primary (~/.claude, which a session reports as "primary") is one when the
// name its launcher takes is known.
func (d *Driver) otherAccounts(b *batch.Batch, current string) []string {
	rejected := map[string]bool{current: true}
	if b != nil {
		evs, _ := b.Events()
		for _, e := range evs {
			if e.Account != "" && (e.Kind == batch.KindFailure || e.Kind == batch.KindRefused) {
				rejected[e.Account] = true
			}
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(d.Cfg.Home, ".claude-accounts", "*"))
	var out []string
	for _, dir := range dirs {
		if st, err := os.Stat(filepath.Join(dir, "sessions")); err == nil && st.IsDir() && !rejected[filepath.Base(dir)] {
			out = append(out, filepath.Base(dir))
		}
	}
	sort.Strings(out)
	if d.Cfg.PrimaryName != "" && !rejected["primary"] {
		if st, err := os.Stat(filepath.Join(d.Cfg.Home, ".claude", "sessions")); err == nil && st.IsDir() {
			out = append(out, "primary")
		}
	}
	return out
}

// relaunchHint says how to carry a session whose login was rejected over to
// another account, for a driver that has its step to hand.
func (d *Driver) relaunchHint(o *Observation) string { return d.relaunch(o, lastStep(o.in, o.Label)) }

// lastStep is the step a session was last sent in this batch, as drive open
// takes it: what a session whose login was rejected was to take. A file or
// a snippet goes again as the copy drive saved of it.
func lastStep(b *batch.Batch, label string) Step {
	if b != nil {
		evs, _ := b.Events()
		for _, e := range slices.Backward(evs) {

			if e.Kind != batch.KindSendIntent || e.Session != label || e.Mode == "notice" {
				continue
			}
			if e.Path != "" {
				return Step{File: e.Path}
			}
			return Step{Line: e.Text}
		}
	}
	return Step{}
}

// relaunch says how to carry a session whose login was rejected over to
// another account: close it on its view as it stands now, and resume it
// there with step, as drive open takes it, on the open.
func (d *Driver) relaunch(o *Observation, step Step) string {
	others := d.otherAccounts(o.in, o.Session.Account)
	if len(others) == 0 {
		return "every other account's login was rejected in this batch too: tell the owner, and park the session until they log in again"
	}
	// Claude Code finds a session to resume under its working directory's project.
	cmd := d.run("close", viewNow(o)) + " && " + d.openCommand(o.Label, OpenOptions{Dir: o.Session.Dir, Resume: o.Session.ID, Account: others[0]}, step)
	what := "relaunch it on another account with the step it was to take"
	if step.empty() {
		what = "relaunch it on another account, then send it the step it was to take on the view the open prints"
	}
	if jobs := d.Host.Shells(o.Session.Status.PID); len(jobs) > 0 {
		return fmt.Sprintf("%s once its background job (%s) ends, as close refuses until then: %s", what, clip(jobs[0].Command, 120), cmd)
	}
	return what + ": " + cmd
}
