package drive

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// A session batch's driver is a Claude Code session in Rex. drive decides
// when it is told exactly as it decides for a chat driver (pace.go), and
// leaves each wake in the log (leave); the driver keeps one listener running
// in the background, which takes every wake left since the last it took,
// prints where each session stands now, and exits, so Claude Code starts the
// driver's next turn. Three facts stay apart, as for a webhook: a wake was
// left (delivered), the listener took it (taken), and a later command of the
// driver's own session ran (heard). When wakes wait and no listener runs, as
// after a Rex restart ended it or a turn that forgot to start it again, the
// watchdog types one line into the driver's own block (Doorbell).

// listenName is a batch's listener among the watches: one runs at a time.
func listenName(b *batch.Batch) string { return "drive-" + safeName(b.Meta.Name) + "-listen" }

// Taken is what a listener took: each wake left since the last take, read
// as it stands now.
type Taken struct {
	Batch string
	At    time.Time
	Zone  *time.Location
	Lines []string // a line per session, timer or watchdog report
	Brief string   // the file a driver reads again once its context lost it, "" when there is none
	Ended string   // the batch stopped being live while the listener waited: why
	Next  string
}

// Listen waits for wakes left for the batch's driver and takes them. Only
// the driver's own session listens, and one listener at a time.
func (d *Driver) Listen() (*Taken, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	switch b.Meta.Wake {
	case batch.WakeSession:
	case batch.WakeWebhook:
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes its chat driver by webhook; only a batch a Claude Code session drives has a listener", b.Meta.Name)}
	default:
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "wait for a session yourself, in the background: " + d.run("wait", "LABEL")}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	reg, ok := batch.Driving(evs)
	switch {
	case !ok:
		return nil, &Refused{Reason: fmt.Sprintf("no Claude Code session is on record as batch %s's driver", b.Meta.Name),
			Next: "make the session that drives it its driver, from that session: " + d.runIn("", "batch", "use", b.Meta.Name)}
	case d.Caller.SID != reg.SID:
		return nil, &Refused{Reason: fmt.Sprintf("only batch %s's driver listens for its wakes: Claude Code session %s, in block %s", b.Meta.Name, reg.SID, rex.Short(reg.Block)),
			Next: "listen from that session; or, if this session drives the batch from now on, make it the driver: " + d.runIn("", "batch", "use", b.Meta.Name)}
	}
	if d.Watches == nil {
		return nil, fmt.Errorf("this driver runs no watches")
	}
	name := listenName(b)
	claimed, err := d.Watches.Claim(name)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, &Refused{Reason: fmt.Sprintf("a listener already runs for batch %s", b.Meta.Name),
			Next: "leave it running: it ends with the next wake, and prints it"}
	}
	defer d.Watches.Retire(name)
	if evs, err := b.Events(); err == nil && len(batch.Untaken(evs)) == 0 {
		// The driver acted on what it took, or found nothing to do: starting
		// the listener again is its trace. One started while wakes wait has
		// read none of them yet, so it is heard only by what it runs next.
		d.Seen()
	}
	for {
		if ended := d.endedWhy(b); ended != "" {
			return &Taken{Batch: b.Meta.Name, At: d.now(), Zone: b.Zone(), Ended: ended}, nil
		}
		var took []batch.Event
		err := b.Locked(func(evs []batch.Event) error {
			if took = batch.Untaken(evs); len(took) == 0 {
				return nil
			}
			return b.Append(batch.Event{At: d.now(), Kind: batch.KindTaken})
		})
		if err != nil {
			return nil, err
		}
		if len(took) > 0 {
			return d.taken(b, took), nil
		}
		d.sleep(d.poll())
	}
}

// endedWhy says why b stopped being live, "" while it is.
func (d *Driver) endedWhy(b *batch.Batch) string {
	live, err := d.Store.Live()
	if err != nil {
		return ""
	}
	for _, l := range live {
		if l.Meta.Name == b.Meta.Name {
			return ""
		}
	}
	return fmt.Sprintf("batch %s is no longer live: it ended, or a new batch of its driver's replaced it", b.Meta.Name)
}

// taken reads what each wake took is about, as it stands now: a session's
// state, view and what to run, a timer's note, the watchdog's lines. A
// session woken twice is read once.
func (d *Driver) taken(b *batch.Batch, wakes []batch.Event) *Taken {
	t := &Taken{Batch: b.Meta.Name, At: d.now(), Zone: b.Zone(), Brief: d.sessionBrief(),
		Next: "act on each, then listen again, in the background: " + d.run("listen")}
	evs, _ := b.Events()
	members := batch.Members(evs)
	seen := map[string]bool{}
	for _, w := range wakes {
		switch {
		case w.Session == "" && strings.HasPrefix(w.Note, watchdogNote):
			t.Lines = append(t.Lines, w.Note)
		case w.Session == "":
			t.Lines = append(t.Lines, "timer done: "+w.Note)
		case !seen[w.Session]:
			seen[w.Session] = true
			t.Lines = append(t.Lines, d.sessionNow(b, members, w.Session))
		}
	}
	return t
}

// sessionNow is a woken session as it stands now, with what to run.
func (d *Driver) sessionNow(b *batch.Batch, members []*batch.Member, label string) string {
	if m := batch.Find(members, label); m != nil && m.Closed {
		return label + ": closed since its wake"
	}
	o, err := d.observe(b, members, label)
	if err != nil {
		why, next := explain(err)
		line := fmt.Sprintf("%s: cannot be read: %s", label, why)
		if next != "" {
			line += "; next: " + next
		}
		return line
	}
	t := d.turnNow(o)
	if !t.Over {
		return fmt.Sprintf("%s: working again at %s, its wake comes when that turn ends", label, o.View)
	}
	line := fmt.Sprintf("%s: %s at %s", label, t.State, o.View)
	if asks := openAsksOn(o.in, label); asks != "" {
		line += "; " + asks
	}
	line += "; next: " + d.readFirst(o, t.State)
	if w := strings.TrimSpace(d.waiterLines(b, label)); w != "" {
		line += "\n  " + strings.ReplaceAll(w, "\n", "\n  ")
	}
	return line
}

// readFirst is what to run about a woken session whose reply the message
// does not carry: read it, then what its ending decides.
func (d *Driver) readFirst(o *Observation, state State) string {
	next := d.run("read", o.Label)
	if n := d.nextFor(o, state, d.Host.Shells(o.Session.Status.PID)); n != "" {
		return "read its reply first (" + next + "); then " + n
	}
	return "read its reply first (" + next + ")"
}

// openAsksOn names the asks still open on a session, "" when none is: what
// waits for the owner stays theirs, whatever the session's reply says.
func openAsksOn(b *batch.Batch, label string) string {
	if b == nil {
		return ""
	}
	evs, err := b.Events()
	if err != nil {
		return ""
	}
	_, asks, _ := batch.Records(evs)
	var open []string
	for _, a := range asks {
		if a.Session == label && a.Answer == nil {
			open = append(open, fmt.Sprintf("%s (%s)", a.ID, clip(a.Question, 80)))
		}
	}
	if len(open) == 0 {
		return ""
	}
	return "open asks, still the owner's: " + strings.Join(open, ", ")
}

// sessionBrief is the file a session driver reads again once its context no
// longer holds it, "" while it does not exist.
func (d *Driver) sessionBrief() string {
	if d.Cfg.SessionBrief == "" {
		return ""
	}
	if _, err := os.Stat(d.Cfg.SessionBrief); err != nil {
		return ""
	}
	return d.Cfg.SessionBrief
}

// Text renders what a listener took.
func (t *Taken) Text() string {
	var out strings.Builder
	if t.Ended != "" {
		fmt.Fprintf(&out, "%s; no listener is needed for it\n", t.Ended)
		return out.String()
	}
	fmt.Fprintf(&out, "batch %s, taken %s:\n", t.Batch, t.At.In(t.Zone).Format("15:04"))
	for _, l := range t.Lines {
		fmt.Fprintf(&out, "- %s\n", l)
	}
	if t.Brief != "" {
		fmt.Fprintf(&out, "Read %s and the skill beside it again first if they are no longer in your context.\n", t.Brief)
	}
	fmt.Fprintf(&out, "next: %s\n", t.Next)
	return out.String()
}

// doorbellLine is what the watchdog types into a session driver's block when
// wakes wait and no listener runs.
func (d *Driver) doorbellLine(b *batch.Batch) string {
	return fmt.Sprintf(doorbellHead+" Wakes wait for you in batch %s and no listener runs: start one in the background (%s), then act on what it prints.", b.Meta.Name, d.run("listen"))
}

// doorbellHead opens the doorbell's line, which tells drive's own unsent
// line from one someone typed.
const doorbellHead = "[drive]"

// doorbellAfter is how long a wake waits untaken before the doorbell rings:
// a driver's turn that ends starts its listener within a minute.
const doorbellAfter = 2 * time.Minute

// Doorbell types one line into a session batch's driver when wakes wait for
// it and no listener runs, once, and again only once a driver's trace shows
// it answered or repostAfter passes with none. It types only into the
// driver's own session, idle, with nothing typed in its box. It says what it
// rang, "" when it rang nothing.
func (d *Driver) Doorbell(b *batch.Batch) (string, error) {
	if b.Meta.Wake != batch.WakeSession || d.Watches == nil || d.Watches.Alive(listenName(b)) {
		return "", nil
	}
	evs, err := b.Events()
	if err != nil {
		return "", err
	}
	untaken := batch.Untaken(evs)
	if len(untaken) == 0 || d.now().Sub(untaken[0].At) < doorbellAfter {
		return "", nil
	}
	reg, ok := batch.Driving(evs)
	if !ok || reg.Block == "" {
		return "", nil
	}
	for _, e := range slices.Backward(evs) {

		if batch.Heard(e) {
			break // the driver ran since the last ring
		}
		if e.Kind == batch.KindTold && e.Outcome == batch.NoticeDoorbell && e.Reason == "" && d.now().Sub(e.At) < repostAfter {
			return "", nil
		}
		if e.Kind == batch.KindTold && e.Outcome == batch.NoticeDoorbell && e.Reason != "" {
			break // the last ring failed: try again
		}
	}
	release, err := d.Store.Own(reg.Block, b.Meta.Name, "ringing the driver's doorbell")
	if err != nil {
		return "", nil // someone acts on the block; the next watchdog run looks again
	}
	a := &action{d: d, block: reg.Block, release: release, label: "driver " + rex.Short(reg.Block)}
	defer a.release()
	o, err := d.observeBlock(nil, nil, a.label, reg.Block)
	switch {
	case err != nil:
		return "", nil // the driver is not there to ring
	case o.Session.ID != reg.SID:
		return "", nil // another conversation runs in its block now
	case o.Input == turn.InputTyped && strings.HasPrefix(strings.TrimSpace(o.Typed), doorbellHead):
		// A ring whose Enter was lost left its own line in the box.
		a.o = o
		if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindTold, Outcome: batch.NoticeDoorbell, Block: reg.Block}); err != nil {
			return "", err
		}
		if err := a.press("enter"); err != nil {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindTold, Outcome: batch.NoticeDoorbell, Block: reg.Block, Reason: clip(firstLine(err.Error()), 200)})
			return "", err
		}
		return "pressed Enter on the line a ring left unsent", nil
	case !ringable(o):
		return "", nil // it works, waits on someone, or shows something other than an empty box
	}
	a.o = o
	line := d.doorbellLine(b)
	// On record before the typing, as the driver can answer before it returns.
	if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindTold, Outcome: batch.NoticeDoorbell, Block: reg.Block}); err != nil {
		return "", err
	}
	if _, err := a.deliver(line); err != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindTold, Outcome: batch.NoticeDoorbell, Block: reg.Block, Reason: clip(firstLine(err.Error()), 200)})
		return "", err
	}
	return line, nil
}

// ringable says the driver's session can take a typed line: its turn is
// over, on no prompt, with an empty box. A turn an API error or an interrupt
// ended is the ordinary way a wake turn dies without starting its listener,
// a Rex restart's interrupt among them; a rejected login or a usage limit
// moves only once someone acts on it.
func ringable(o *Observation) bool {
	state := o.State()
	switch {
	case state == Working || state.blocked():
		return false
	case o.Frontier.Error != nil && o.Frontier.Error.Limit:
		return false
	}
	return o.Input == turn.InputEmpty || o.Input == turn.InputSuggestion // typing lands in whatever the screen shows
}

// listenerLine says a session batch's driver has no listener running, and
// how long wakes have waited for one; "" for any other batch, or while one
// runs.
// stalled keeps only wakes that waited past the doorbell's delay, as the
// owner's page says nothing of a driver between its wake and its listener.
func (d *Driver) listenerLine(b *batch.Batch, evs []batch.Event, zone *time.Location, stalled bool) string {
	if b.Meta.Wake != batch.WakeSession || d.Watches == nil || d.Watches.Alive(listenName(b)) {
		return ""
	}
	untaken := batch.Untaken(evs)
	if stalled && (len(untaken) == 0 || d.now().Sub(untaken[0].At) < doorbellAfter || d.driverWorking(evs)) {
		return ""
	}
	if len(untaken) > 0 {
		return fmt.Sprintf("no listener runs, and %d wakes have waited for the driver since %s: start one, in the background: %s",
			len(untaken), untaken[0].At.In(zone).Format("15:04"), d.run("listen"))
	}
	return "no listener runs for the driver: start one, in the background, before its turn ends: " + d.run("listen")
}

// joinLines joins the lines that say something, one to a line.
func joinLines(lines ...string) string {
	var out []string
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// driverWorking says the session batch's driver is mid-turn, which starts
// its listener when the turn ends: wakes waiting meanwhile are no stall.
func (d *Driver) driverWorking(evs []batch.Event) bool {
	reg, ok := batch.Driving(evs)
	if !ok || reg.Block == "" {
		return false
	}
	s, err := session.Locate(d.Term, d.Host, reg.Block)
	if err != nil || s.ID != reg.SID {
		return false
	}
	f, err := s.Frontier()
	return err == nil && turnState(s, f) == Working
}
