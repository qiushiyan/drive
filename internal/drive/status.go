package drive

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// ReadMode picks what Read shows.
type ReadMode string

const (
	ReadLast    ReadMode = "last"    // the newest reply
	ReadPrompts ReadMode = "prompts" // what it has been told
	ReadPending ReadMode = "pending" // the tool call a prompt waits on
	ReadUser    ReadMode = "user"    // the newest prompt, as it arrived
	ReadScreen  ReadMode = "screen"  // the end of what its block shows: a prompt's options, whatever covers the input box
)

// Reading is one session read for a decision.
type Reading struct {
	Label   string
	SID     string // the Claude Code session id, which claude-steps takes
	Goal    string // what it sets out to do, as recorded for the owner
	State   State
	Account string
	Input   turn.InputKind
	Typed   string
	Body    string // what was read: the reply, the prompts, the pending call or the screen
	View    turn.View
	At      time.Time       // when the text was written
	Next    string          // what to run, where the state decides it
	Jobs    []session.Shell // background jobs it runs
}

// Read reads a session and returns the view to act on.
func (d *Driver) Read(ref string, mode ReadMode, n int) (*Reading, error) {
	b, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	m := batch.Find(members, ref)
	if m != nil && m.Closed {
		return d.readEnded(b, m, "closed", mode, n)
	}
	o, err := d.observe(b, members, ref)
	if err != nil {
		if m != nil {
			if _, lerr := session.Locate(d.Term, d.Host, m.Block); errors.Is(lerr, session.ErrNoClaude) {
				return d.readEnded(b, m, "gone", mode, n)
			}
		}
		return nil, err
	}
	r := &Reading{Label: o.Label, SID: o.Session.ID, State: o.State(), Account: o.Session.Account, Input: o.Input, Typed: o.Typed, View: o.View}
	if o.Member != nil {
		r.Goal = o.Member.Goal
	}
	r.Jobs = d.Host.Shells(o.Session.Status.PID)
	r.Next = d.nextFor(o, r.State, r.Jobs)
	if mode == ReadScreen {
		screen, err := d.Term.Capture(o.Session.Block, false)
		if err != nil {
			return r, &Refused{Reason: fmt.Sprintf("%s's screen cannot be read: %v", o.Label, err), Next: "try again: " + d.run("read", o.Label, "--screen")}
		}
		r.Body = lastLines(strings.TrimRight(screen, "\r\n "), 40)
		return r, nil
	}
	return r, d.readText(r, o.Frontier, mode, n, o.zone())
}

// readEnded reads a member whose session has ended, its block closed or its
// process gone, from its transcript, which outlives both: a report run after
// a reset still has its recap. It has no screen, prompt or view to act on.
func (d *Driver) readEnded(b *batch.Batch, m *batch.Member, state State, mode ReadMode, n int) (*Reading, error) {
	path := ""
	if m.SID != "" {
		path = d.Host.Transcript(m.SID)
	}
	if path == "" {
		return nil, &Refused{Reason: fmt.Sprintf("%s is %s, and no transcript of its session (%s) is found", m.Label, state, m.SID),
			Next: d.run("log") + " has what was sent to it and how each step went"}
	}
	if mode == ReadScreen || mode == ReadPending {
		return nil, &Refused{Reason: fmt.Sprintf("%s is %s: it has no screen and no prompt waiting", m.Label, state),
			Next: "read its transcript as it ended: " + d.run("read", m.Label)}
	}
	f, err := turn.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s's transcript: %w", m.Label, err)
	}
	r := &Reading{Label: m.Label, SID: m.SID, Goal: m.Goal, State: state, Account: m.Account}
	return r, d.readText(r, f, mode, n, zoneOf(b))
}

// readText fills a reading's text from the transcript, as mode asks, its
// times in zone.
func (d *Driver) readText(r *Reading, f turn.Frontier, mode ReadMode, n int, zone *time.Location) error {
	switch mode {
	case ReadPrompts:
		ps := f.Prompts
		if f.Queued != nil {
			ps = append(append([]turn.Prompt{}, ps...), *f.Queued)
		}
		if len(ps) > n {
			ps = ps[len(ps)-n:]
		}
		var b strings.Builder
		for _, p := range ps {
			q := ""
			if p.Queued {
				q = " (queued)"
			}
			fmt.Fprintf(&b, "--- %s%s\n%s\n\n", p.At.In(zone).Format("01-02 15:04"), q, promptText(p))
		}
		r.Body = strings.TrimRight(b.String(), "\n")
	case ReadPending:
		if f.Pending == nil {
			return &Refused{Reason: fmt.Sprintf("%s has no tool call waiting", r.Label)}
		}
		r.Body = f.Pending.Name + ":\n" + f.Pending.Input
	case ReadUser:
		p := f.Newest()
		if p == nil {
			return &Refused{Reason: fmt.Sprintf("%s has received no prompt yet", r.Label)}
		}
		r.Body, r.At = promptText(*p), p.At
		if p.Queued {
			r.Body = "(queued) " + r.Body
		}
	default:
		if f.Error != nil {
			r.Body, r.At = fmt.Sprintf("[%s] %s", f.Error.Kind, f.Error.Text), f.Error.At
		} else if a, local := f.Answer(); a != nil {
			r.Body, r.At = answerText(f, a, local), a.At
		} else {
			r.Body = "(no reply yet)"
		}
	}
	return nil
}

// Row is one session in the status table.
type Row struct {
	Label    string
	SID      string
	Block    string
	Window   string
	State    State
	For      time.Duration // since its state last changed
	Account  string
	Input    turn.InputKind
	Typed    string
	Stage    string
	StageAt  time.Time    // when its stage's newest step was taken
	PR       *progress.PR // the newest it named, as read
	PRRef    *progress.PRRef
	PRErr    string
	PRs      []progress.PRRef // every one it opened or named
	StepsErr string           // why its stage and pull requests could not be read
	LastSent string           // the last step a driver sent, and when
	SentAt   time.Time
	View     turn.View
	Member   bool
	Name     string          // the block's Rex label, for sessions outside the batch
	Goal     string          // what a member sets out to do, as recorded
	Error    string          // what kept the row from being read
	Jobs     []session.Shell // background jobs it runs
	Wake
}

// Wake is how a member's wake stands.
type Wake struct {
	Watch     string    // armed, woken at …, parked: why, none
	Armed     bool      // its watch runs, stuck or not
	Unhandled bool      // a wake was posted after the driver last acted on it
	Due       string    // why the watchdog should arm its wake; "" when nothing is due
	Again     time.Time // when the watchdog posts a wake nobody acted on once more, while that is still to come
}

// Status is the batch at a glance.
type Status struct {
	Batch    *batch.Meta
	Dir      string // the batch's directory
	Zone     *time.Location
	Rows     []Row
	Others   []Row // Claude sessions in Rex outside the batch
	Closed   int   // sessions of the batch closed since
	Holds    []string
	Asks     []*batch.Ask
	Grants   []batch.Event
	InFlight []InFlight
	NoGoal   string // which open sessions have no goal recorded, and how to record one
}

// InFlight is an action admitted with no outcome yet.
type InFlight struct {
	batch.Event
	Driver *batch.Owner // the driver acting on it; nil when that driver has gone
}

// Status reads every session in the batch, and lists the other Claude
// sessions in Rex, live.
func (d *Driver) Status(withProgress bool) (*Status, error) {
	b, evs, members, err := d.members()
	if err != nil {
		return nil, err
	}
	out := &Status{Zone: zoneOf(b)}
	session := ""
	if b != nil {
		out.Batch, out.Dir, session = &b.Meta, b.Dir, b.Meta.RexSession
	}
	blocks, err := d.Term.Blocks(session)
	if err != nil {
		return nil, err
	}
	// A parked waiter whose hold is free is due a wake, so the holds are
	// read before the rows.
	holds := d.holdsNow(evs, members)
	out.Rows, out.Others, out.Closed = d.rows(b, members, blocks, holds, out.Zone, withProgress)
	if withProgress {
		d.readNewestPRs(out.Rows)
	}
	var none []string
	for _, r := range out.Rows {
		if r.Goal == "" {
			none = append(none, r.Label)
		}
	}
	if len(none) > 0 {
		out.NoGoal = fmt.Sprintf("no goal recorded for %s; the owner's page shows each session's goal: %s", strings.Join(none, ", "),
			d.run("record", "goal", none[0], text("…")))
	}
	out.Holds = holdLines(holds, out.Zone)
	grants, asks, _ := batch.Records(evs)
	out.Grants = grants
	for _, a := range asks {
		if a.Answer == nil {
			out.Asks = append(out.Asks, a)
		}
	}
	out.InFlight = d.inFlight(evs, members)
	return out, nil
}

// rows reads every open session of the batch, and the other Claude
// sessions among blocks, at once, each reader filling a slot of its own;
// closed counts the batch's sessions closed since.
func (d *Driver) rows(b *batch.Batch, members []*batch.Member, blocks []rex.Block, holds []holdNow, zone *time.Location, withProgress bool) (rows, others []Row, closed int) {
	window := map[string]string{}
	seen := map[string]bool{}
	for _, bl := range blocks {
		window[bl.ID] = bl.Window
	}
	for _, m := range members {
		seen[m.Block] = true
		if m.Closed {
			closed++
		}
	}
	var wg sync.WaitGroup
	outside := make([]*Row, len(blocks))
	for i, bl := range blocks {
		if seen[bl.ID] {
			continue
		}
		wg.Go(func() {
			if st, err := d.Term.Status(bl.ID); err != nil || st == nil {
				return
			}
			r := d.row(nil, nil, bl.ID, false)
			if r.Error != "" {
				return
			}
			r.Label, r.Window, r.Name = rex.Short(bl.ID), bl.Window, bl.Label
			outside[i] = &r
		})
	}
	rows = d.memberRows(b, members, window, holds, zone, withProgress)
	wg.Wait()
	for _, r := range outside {
		if r != nil {
			others = append(others, *r)
		}
	}
	return rows, others, closed
}

// memberRows reads every open member of the batch at once, each reader
// filling a slot of its own: its turn, its wake, and with withProgress its
// stage and the pull requests it named. window places each block in Rex.
func (d *Driver) memberRows(b *batch.Batch, members []*batch.Member, window map[string]string, holds []holdNow, zone *time.Location, withProgress bool) []Row {
	free := map[string]string{} // a session waiting for a hold that is free, to that hold
	for _, h := range holds {
		for _, w := range h.waiting {
			if !h.held {
				free[w.Session] = h.Name
			}
		}
	}
	var open []*batch.Member
	for _, m := range members {
		if !m.Closed {
			open = append(open, m)
		}
	}
	rows := make([]Row, len(open))
	var wg sync.WaitGroup
	for i, m := range open {
		wg.Go(func() {
			r := d.row(b, members, m.Label, withProgress)
			r.Window, r.Member, r.Goal = window[m.Block], true, m.Goal
			if m.LastSend != nil {
				r.LastSent, r.SentAt = sentLabel(m.LastSend), m.LastSend.At
			}
			r.Wake = d.wakeOf(b, m, r, zone, free[m.Label])
			rows[i] = r
		})
	}
	wg.Wait()
	return rows
}

// holdLines is a line per hold that is held or waited for: who holds it,
// and who waits for it with the step it was refused.
func holdLines(holds []holdNow, zone *time.Location) []string {
	var out []string
	for _, h := range holds {
		line := h.Name + ": free"
		if h.held {
			line = fmt.Sprintf("%s: held by %s since %s, %s", h.Name, h.by.Session, h.by.Since.In(zone).Format("15:04"), h.by.why)
		}
		var waiting []string
		for _, w := range h.waiting {
			waiting = append(waiting, fmt.Sprintf("%s (refused %s at %s)", w.Session, clip(join(stepWords(refusedStep(w))...), 60), w.Since.In(zone).Format("15:04")))
		}
		switch {
		case len(waiting) > 0:
			line += "; waiting for it: " + strings.Join(waiting, ", ")
		case !h.held:
			continue
		}
		out = append(out, line)
	}
	return out
}

// inFlight lists the actions admitted with no outcome yet, each with the
// driver acting on it, or none when that driver has gone.
func (d *Driver) inFlight(evs []batch.Event, members []*batch.Member) []InFlight {
	var out []InFlight
	named := map[string]bool{}
	for _, e := range evs {
		if (e.Kind != batch.KindSendIntent && e.Kind != batch.KindOpenIntent) || named[e.Session] {
			continue
		}
		named[e.Session] = true
		for _, p := range batch.Unresolved(evs, e.Session) {
			key := "open:" + p.Session
			if m := batch.Find(members, p.Session); p.Kind == batch.KindSendIntent && m != nil {
				key = m.Block
			}
			out = append(out, InFlight{Event: p, Driver: d.Store.Busy(key)})
		}
	}
	return out
}

// repostAfter is how long a wake nobody acted on waits before the watchdog
// posts it once more: a run acting on it takes minutes.
const repostAfter = 10 * time.Minute

// wakeOf says how a member's wake stands, first whether a watch runs and
// then what the log says came of the last one, and why the watchdog should
// arm it again. free is the hold a parked member waits for, when that hold
// is free.
func (d *Driver) wakeOf(b *batch.Batch, m *batch.Member, r Row, zone *time.Location, free string) Wake {
	alive := d.Watches != nil && b != nil && d.Watches.Alive(watchName(b, m.Block))
	woken := !m.Woken.IsZero() && !m.Woken.Before(m.Watched)
	switch {
	case m.Parked != "" && free != "":
		// A parked waiter whose hold is free wakes no one unless its watch is armed again.
		return Wake{Watch: "parked: " + m.Parked + "; " + free + " is free now, and " + d.run("watch", "--due") + " wakes it",
			Due: "waits for " + free + ", which is free"}
	case m.Parked != "":
		return Wake{Watch: "parked: " + m.Parked}
	case alive && stuck(r.State, d.now().Add(-r.For), m.Watched, d.now()):
		return Wake{Watch: fmt.Sprintf("armed, but its turn ended %s ago: stuck (%s restarts it)", ago(r.For), d.run("watch", m.Label)), Due: "stuck", Armed: true}
	case alive:
		return Wake{Watch: "armed", Armed: true}
	case m.Posting.After(m.Watched) && m.Posting.After(m.Woken) && m.Posting.After(m.WakeFailed):
		return Wake{Watch: "posting since " + m.Posting.In(zone).Format("15:04")}
	case woken && m.Woken.After(m.LastAction):
		// The driver owes it a decision. A session that moved on by itself
		// since, such as one a usage limit's reset continued, is woken again
		// when that turn ends; otherwise the wake is posted once more, so one
		// lost to a run that died is not lost, and never again for one view.
		w := Wake{Watch: "woken " + m.Woken.In(zone).Format("15:04") + " (nothing done since", Unhandled: true}
		switch {
		case r.View.Session != "" && m.WokenView != "" && r.View.String() != m.WokenView:
			w.Due = "it moved on by itself since its last wake"
		case m.WokenView == m.PrevView:
			w.Watch += ", and posted again already"
		case d.now().Sub(m.Woken) > repostAfter:
			w.Due = "woken " + ago(d.now().Sub(m.Woken)) + " ago with nothing done since"
		default:
			w.Again = m.Woken.Add(repostAfter)
			w.Watch += "; " + d.run("watch", "--due") + " posts it again from " + w.Again.In(zone).Format("15:04")
		}
		w.Watch += ")"
		return w
	case woken:
		return Wake{Watch: "none (woken " + m.Woken.In(zone).Format("15:04") + ", acted on since)", Due: "acted on with no wake armed"}
	case !m.Watched.IsZero():
		return Wake{Watch: "ended without a wake (watches.log says why)", Due: "its watch ended without a wake"}
	}
	return Wake{Watch: "none", Due: "never watched"}
}

// stuck says a watch still runs long after the turn it waits on ended: a
// wait returns a minute after a turn ends, or ten after one that left its
// own background jobs running, counted from when the watch was armed if
// that was later.
func stuck(state State, since, watched, now time.Time) bool {
	ended := state == "done" || state == "idle" || state == "error" || state == "interrupted" || state.blocked()
	if watched.After(since) {
		since = watched
	}
	return ended && now.Sub(since) > 15*time.Minute
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sentLabel(e *batch.Event) string {
	switch {
	case e.Key != "":
		return "snippet " + e.Key
	case e.Mode == "file":
		return "file"
	default:
		return clip(strings.TrimSpace(e.Text), 40)
	}
}

func (d *Driver) row(b *batch.Batch, members []*batch.Member, label string, withProgress bool) Row {
	r := Row{Label: label}
	o, err := d.observe(b, members, label)
	if err != nil {
		r.State, r.Error = "gone", firstLine(err.Error())
		return r
	}
	r.Block, r.SID, r.State, r.For, r.Account = o.Session.Block, o.Session.ID, o.State(), d.now().Sub(o.Session.Since()), o.Session.Account
	r.Input, r.Typed, r.View = o.Input, o.Typed, o.View
	r.Jobs = d.Host.Shells(o.Session.Status.PID)
	if withProgress && d.Progress != nil && o.Session.ID != "" {
		if s, err := d.Progress.Steps(o.Session.ID); err != nil {
			r.StepsErr = firstLine(err.Error())
		} else {
			r.Stage, r.StageAt, r.PRRef, r.PRs = s.Stage, s.StageAt, s.PR, s.PRs
		}
	}
	return r
}

// readNewestPRs gives each row the state of the newest pull request it
// named.
func (d *Driver) readNewestPRs(rows []Row) {
	var refs []progress.PRRef
	for _, r := range rows {
		if r.PRRef != nil {
			refs = append(refs, *r.PRRef)
		}
	}
	states := d.prStates(refs)
	for i := range rows {
		if ref := rows[i].PRRef; ref != nil {
			if s, ok := states[keyOf(*ref)]; ok {
				rows[i].PR, rows[i].PRErr = s.read()
			}
		}
	}
}

// prKey is a pull request, whichever session names it.
type prKey struct {
	repo   string
	number int
}

func keyOf(r progress.PRRef) prKey { return prKey{r.Repo, r.Number} }

// prRead is a pull request's state as read, or why it could not be.
type prRead struct {
	pr  progress.PR
	err error
}

// read is the state, or why it could not be read, as a row or a card holds it.
func (s prRead) read() (*progress.PR, string) {
	if s.err != nil {
		return nil, firstLine(s.err.Error())
	}
	pr := s.pr
	return &pr, ""
}

// prStates reads the state of every pull request refs name, all at once
// and each once however many sessions name it: status, the page and
// metrics each ask for theirs in one call.
func (d *Driver) prStates(refs []progress.PRRef) map[prKey]prRead {
	out := map[prKey]prRead{}
	if d.Progress == nil {
		return out
	}
	var unique []progress.PRRef
	for _, ref := range refs {
		if _, ok := out[keyOf(ref)]; !ok {
			out[keyOf(ref)] = prRead{}
			unique = append(unique, ref)
		}
	}
	reads := make([]prRead, len(unique))
	var wg sync.WaitGroup
	for i, ref := range unique {
		wg.Go(func() { reads[i].pr, reads[i].err = d.Progress.PR(ref) })
	}
	wg.Wait()
	for i, ref := range unique {
		out[keyOf(ref)] = reads[i]
	}
	return out
}

// answerText is an answer as a driver reads it; a local command's output
// says which command it answers, since it is no reply.
func answerText(f turn.Frontier, a *turn.Entry, local bool) string {
	if !local {
		return a.Text
	}
	cmd := "the command"
	if p := f.Newest(); p != nil && p.Known {
		cmd = p.Text
	}
	return cmd + " finished: " + a.Text
}
