package drive

import (
	"errors"
	"fmt"
	"slices"
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
	// DrivenBy is, for a session outside the batch, the live batch of
	// another driver's that drives it, which alone acts on it.
	DrivenBy string
}

// Read reads a session and returns the view to act on.
func (d *Driver) Read(ref string, mode ReadMode, n int) (*Reading, error) {
	b, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	m := batch.Find(members, ref)
	if m != nil && m.Closed {
		return d.readEnded(b, m, nil, mode, n)
	}
	o, err := d.observe(b, members, ref)
	if err != nil {
		if m != nil && gone(err) {
			return d.readEnded(b, m, err, mode, n)
		}
		return nil, err
	}
	d.noteLimit(o.in, o) // a driver that reads a limit and relaunches leaves no watch to find it
	r := &Reading{Label: o.Label, SID: o.Session.ID, State: o.State(), Account: o.Session.Account, Input: o.Input, Typed: o.Typed, View: o.View}
	if o.Member != nil {
		r.Goal = o.Member.Goal
	}
	r.Jobs = d.Host.Shells(o.Session.Status.PID)
	if o.Member == nil {
		if e, err := d.drivenElsewhere(b, o.Session.Block); err == nil && e != nil {
			r.DrivenBy = e.String()
		}
	}
	if r.DrivenBy == "" {
		r.Next = d.nextFor(o, r.State, r.Jobs)
	}
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
// goneBy is what the read of a gone one met, nil for one its driver closed.
func (d *Driver) readEnded(b *batch.Batch, m *batch.Member, goneBy error, mode ReadMode, n int) (*Reading, error) {
	state := Closed
	if goneBy != nil {
		state = Gone
	}
	path := ""
	if m.SID != "" {
		path = d.Host.Transcript(m.SID)
	}
	if path == "" && state == Gone && m.Starting {
		if busy := d.opening(m); busy != nil {
			return nil, &Refused{Reason: fmt.Sprintf("%s has nothing to read: %s", m.Label, openingWords(busy)),
				Next: fmt.Sprintf("let it finish; once it is done, %s shows where it stands", d.run("read", m.Label))}
		}
		why, next := d.openAgain(b, m)
		return nil, &Refused{Reason: fmt.Sprintf("%s has nothing to read: %s", m.Label, why), Next: next}
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
	if state == Gone {
		r.Next = d.goneNext(b, m, &f, goneBy)
	}
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
	Since    time.Time     // when its state last changed
	Account  string
	Input    turn.InputKind
	Typed    string
	Stage    string
	StageAt  time.Time        // when its stage's newest step was taken
	Labels   []progress.Label // every stage, with its newest step's time
	Branch   string
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
	DrivenBy string          // for a session outside the batch, the live batch that drives it: its label there, the batch and its driver
	Goal     string          // what a member sets out to do, as recorded
	Error    string          // what kept the row from being read
	Next     string          // for a member that is gone, how to bring it back
	Jobs     []session.Shell // background jobs it runs
	Wake
	facts wakeFacts // what decides its wake, for a session read live
}

// wakeKind is how a member's wake stands, which a reader decides on; the
// words are Wake's.
type wakeKind int

const (
	wakeNever     wakeKind = iota // no watch was ever armed for it
	wakeParked                    // parked: it wakes no one until its next step
	wakeArmed                     // its watch runs, and posts when its turn ends
	wakeJobHeld                   // its watch runs, and a job holds its post
	wakeStalled                   // its watch runs, and waits for a driver that is not answering
	wakeStuck                     // its watch runs long past when it should have posted
	wakeStopped                   // stopped by watch --stop-all
	wakePosting                   // its post is under way
	wakeUnhandled                 // woken, and nothing done since
	wakeActed                     // woken and acted on, with no wake armed since
	wakeEnded                     // its watch ended without a wake
)

// Wake is how a member's wake stands.
type Wake struct {
	kind      wakeKind
	Watch     string    // armed, woken at …, parked: why, none
	Armed     bool      // its watch runs, stuck or not
	Unhandled bool      // a wake was posted after the driver last acted on it
	Due       string    // why the watchdog should arm its wake; "" when nothing is due
	Again     time.Time // when the watchdog posts a wake nobody acted on once more, while that is still to come
	// HoldsUntil is when its wake comes at the latest while a job holds it,
	// and HoldWhy why; zero while nothing does.
	HoldsUntil time.Time
	HoldWhy    string
	// Park is what ends its park, while it is parked.
	Park *parkEnd
}

// Status is the batch at a glance.
type Status struct {
	Batch  *batch.Meta
	Dir    string // the batch's directory
	Zone   *time.Location
	Rows   []Row
	Others []Row // Claude sessions in Rex outside the batch
	Closed int   // sessions of the batch closed since
	Holds  []string
	Asks   []*batch.Ask
	// Overtaken is, by ask, what happened to an open ask's session after
	// the ask last stood: its close, and its newest pull request's merge
	// when progress was read.
	Overtaken map[string]Overtaken
	Grants    []batch.Event
	InFlight  []InFlight
	NoGoal    string // which open sessions have no goal recorded, and how to record one
	Unheard   string // that the batch's driver is not answering its wakes, and what is held for it
	// TimersHeld are the timers due while the driver is not answering, each
	// still on the books.
	TimersHeld []string
	// Leftovers are what closed sessions of this driver's batches left: a line
	// each, with what becomes of it. Read only with progress, like the rows' stage.
	Leftovers []string
	// Parks are the parked sessions whose park needs the driver: its --until
	// has come, or nothing on record ends it. A line each, with what to run.
	Parks []string

	free []freeHold // the holds that are free with sessions waiting for them
}

// freeHold is a hold that is free while sessions wait for it, with the send
// or open to run for each: what its waiters' driver has to hear once.
type freeHold struct {
	Name string
	Next string
}

// freeHolds lists them. A waiter needs no member to be one: an open refused on
// a hold waits under the label it was to open as.
func (d *Driver) freeHolds(b *batch.Batch, evs []batch.Event, members []*batch.Member, holds []holdNow) []freeHold {
	var out []freeHold
	for _, h := range holds {
		if b == nil || h.by.held() || len(h.waiting) == 0 {
			continue
		}
		out = append(out, freeHold{Name: h.Name, Next: d.waitersNext(b, evs, members, h.Name)})
	}
	return out
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
	switch {
	case errors.Is(err, rex.ErrNoServer):
		return nil, serverDown(d.again())
	case errors.Is(err, rex.ErrNoSession):
		// Its Rex session ended, as on a server restart: its sessions read
		// as gone below, each with the open that brings it back, which
		// starts the Rex session again.
	case err != nil:
		return nil, err
	}
	// A parked waiter's row says when its hold is free, so the holds are read
	// before the rows.
	holds := d.holdsNow(evs, members)
	hearing := batch.Hearings(evs, d.now())
	out.Rows, out.Others, out.Closed = d.rows(b, evs, members, blocks, holds, hearing, out.Zone, withProgress)
	if b != nil && b.Meta.Wakes() {
		out.Unheard = joinLines(unheardLine(hearing, out.Zone), d.listenerLine(b, evs, out.Zone))
		out.TimersHeld = d.timersHeld(b, evs)
	}
	if withProgress && b != nil {
		out.Leftovers = d.leftoverLines(b)
	}
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
	var needs []parkNeed
	for _, r := range out.Rows {
		if n := d.parkNeedOf(r); n != nil {
			needs = append(needs, *n)
		}
	}
	out.Parks = d.parkLines(needs, out.Zone)
	out.Holds = holdLines(holds, out.Zone)
	// A free hold someone waits for says what to run for each waiter, in place
	// of who waits: that send or open is the news.
	out.free = d.freeHolds(b, evs, members, holds)
	for _, f := range out.free {
		for i, line := range out.Holds {
			if strings.HasPrefix(line, f.Name+": free") {
				out.Holds[i] = f.Name + ": free; " + f.Next
			}
		}
	}
	grants, asks, _ := batch.Records(evs)
	out.Grants = grants
	out.Overtaken = map[string]Overtaken{}
	for _, a := range asks {
		if a.Answer != nil {
			continue
		}
		out.Asks = append(out.Asks, a)
		if a.Session == "" {
			continue
		}
		var ref *progress.PRRef
		var pr *progress.PR
		if i := slices.IndexFunc(out.Rows, func(r Row) bool { return r.Label == a.Session }); i >= 0 {
			ref, pr = out.Rows[i].PRRef, out.Rows[i].PR
		}
		if o := overtakenOf(a, batch.Find(members, a.Session), ref, pr); !o.None() {
			o.Next = d.rejudge(a.ID)
			out.Overtaken[a.ID] = o
		}
	}
	out.InFlight = d.inFlight(evs, members)
	return out, nil
}

// rows reads every open session of the batch, and the other Claude
// sessions among blocks, at once, each reader filling a slot of its own;
// closed counts the batch's sessions closed since.
func (d *Driver) rows(b *batch.Batch, evs []batch.Event, members []*batch.Member, blocks []rex.Block, holds []holdNow, hearing batch.Hearing, zone *time.Location, withProgress bool) (rows, others []Row, closed int) {
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
			// The block is in hand, so its row reads it without listing
			// Rex's blocks again; one with no Claude Code reads as an error.
			r := d.rowAt(nil, nil, rex.Short(bl.ID), bl.ID, false)
			if r.Error != "" {
				return
			}
			r.Label, r.Window, r.Name = rex.Short(bl.ID), bl.Window, bl.Label
			if e, err := d.drivenElsewhere(b, bl.ID); err == nil && e != nil {
				r.DrivenBy = e.String()
			}
			outside[i] = &r
		})
	}
	rows = d.memberRows(b, evs, members, window, holds, hearing, zone, withProgress)
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
func (d *Driver) memberRows(b *batch.Batch, evs []batch.Event, members []*batch.Member, window map[string]string, holds []holdNow, hearing batch.Hearing, zone *time.Location, withProgress bool) []Row {
	free := map[string]string{} // a session waiting for a hold that is free, to that hold
	for _, h := range holds {
		for _, w := range h.waiting {
			if !h.by.held() {
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
			var end parkEnd
			if m.Parked != "" {
				end = endOf(evs, m, time.Time{})
			}
			r.Wake = d.wakeOf(b, m, r, hearing, zone, free[m.Label], end)
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
		if h.by.held() {
			line = fmt.Sprintf("%s: held by %s since %s, %s", h.Name, h.by.Session, h.by.Since.In(zone).Format("15:04"), h.by.why())
		}
		var waiting []string
		for _, w := range h.waiting {
			step := clip(join(stepWords(refusedStep(w))...), 60)
			if w.Open {
				step = "its open" // the whole command, which the free hold's line gives
			}
			waiting = append(waiting, fmt.Sprintf("%s (refused %s at %s)", w.Session, step, w.Since.In(zone).Format("15:04")))
		}
		switch {
		case len(waiting) > 0:
			line += "; waiting for it: " + strings.Join(waiting, ", ")
		case !h.by.held():
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

// wakeOf says how a member's wake stands, first whether a watch runs and
// then what the log says came of the last one, and why the watchdog should
// arm it again. free is the hold a parked member waits for, when that hold
// is free.
func (d *Driver) wakeOf(b *batch.Batch, m *batch.Member, r Row, hearing batch.Hearing, zone *time.Location, free string, end parkEnd) Wake {
	alive := d.Watches != nil && b != nil && d.Watches.Alive(watchName(b, m.Block))
	standing, since := m.Wake.Standing(d.now())
	told, _ := m.Wake.Told()
	parked := "parked " + end.words(zone)
	if end.due(d.now()) {
		parked += ", which has come"
	}
	switch {
	case m.Parked != "" && free != "":
		// It stays parked and wakes no one of itself: the hold's own line says
		// what to send it, and the watchdog tells its driver once (freeHolds).
		return Wake{kind: wakeParked, Watch: parked + ": " + m.Parked + "; " + free + " is free now", Park: &end}
	case m.Parked != "":
		return Wake{kind: wakeParked, Watch: parked + ": " + m.Parked, Park: &end}
	case alive:
		plan := r.facts.plan()
		switch {
		case plan.held != "" && plan.at.After(d.now()):
			return Wake{kind: wakeJobHeld, Watch: "armed: " + plan.held + ", so the wake comes when that turn ends, by " + plan.at.In(zone).Format("15:04") + " at the latest",
				Armed: true, HoldsUntil: plan.at, HoldWhy: plan.held}
		case hearing.Strikes >= stallAfter:
			return Wake{kind: wakeStalled, Watch: "armed: what it has to say waits until the driver answers", Armed: true}
		case plan.stuck(d.now()):
			return Wake{kind: wakeStuck, Watch: fmt.Sprintf("armed, but its turn ended %s ago: stuck (%s restarts it)", ago(r.For), d.run("watch", m.Label)), Due: "stuck", Armed: true}
		}
		return Wake{kind: wakeArmed, Watch: "armed", Armed: true}
	}
	// No watch runs for it, so the record says what came of the last one.
	switch standing {
	case batch.WakeStopped:
		return Wake{kind: wakeStopped, Watch: "stopped " + since.In(zone).Format("15:04") + " (" + d.run("watch", "--due") + " arms it again)", Due: "its wake was stopped"}
	case batch.WakePosting:
		return Wake{kind: wakePosting, Watch: "posting since " + since.In(zone).Format("15:04")}
	case batch.WakeUnhandled:
		// The driver owes it a decision.
		w := Wake{kind: wakeUnhandled, Watch: "woken " + told.At.In(zone).Format("15:04") + " (nothing done since", Unhandled: true}
		f := r.facts
		f.member = m
		switch rp := f.repost(d.now()); {
		case rp.due != "":
			w.Due = rp.due
		case rp.done:
			w.Watch += ", and posted again already"
		default:
			w.Again = rp.again
			w.Watch += "; " + d.run("watch", "--due") + " posts it again from " + w.Again.In(zone).Format("15:04")
		}
		w.Watch += ")"
		return w
	case batch.WakeActed:
		return Wake{kind: wakeActed, Watch: "none (woken " + told.At.In(zone).Format("15:04") + ", acted on since)", Due: "acted on with no wake armed"}
	case batch.WakeNoResult:
		return Wake{kind: wakeEnded, Watch: "ended without a wake (watches.log says why)", Due: "its watch ended without a wake"}
	}
	return Wake{kind: wakeNever, Watch: "none", Due: "never watched"}
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
		return clip(strings.TrimSpace(stepOf(*e)), 40)
	}
}

func (d *Driver) row(b *batch.Batch, members []*batch.Member, label string, withProgress bool) Row {
	return d.rowAt(b, members, label, "", withProgress)
}

// rowAt is row for a session whose block is known, "" when label is to be
// resolved.
func (d *Driver) rowAt(b *batch.Batch, members []*batch.Member, label, block string, withProgress bool) Row {
	r := Row{Label: label}
	var o *Observation
	var err error
	if block != "" {
		o, err = d.observeBlock(b, members, label, block)
	} else {
		o, err = d.observe(b, members, label)
	}
	if err != nil {
		// Gone is a block or a Claude Code that no longer runs; a read that
		// merely failed says neither, and reads again.
		r.State, r.Error = Unread, firstLine(err.Error())
		if gone(err) {
			r.State = Gone
		}
		if m := batch.Find(members, label); m != nil && gone(err) {
			if busy := d.opening(m); busy != nil {
				r.State, r.Error = Starting, openingWords(busy)
			} else {
				r.Next = d.goneNext(b, m, nil, err)
			}
		}
		return r
	}
	r.Block, r.SID, r.State, r.Since, r.Account = o.Session.Block, o.Session.ID, o.State(), o.Session.Since(), o.Session.Account
	r.For = d.now().Sub(r.Since)
	r.Input, r.Typed, r.View = o.Input, o.Typed, o.View
	r.Jobs = d.Host.Shells(o.Session.Status.PID)
	stable := r.Since
	if m := o.Member; m != nil && m.Wake.Armed().After(stable) {
		stable = m.Wake.Armed()
	}
	r.facts = factsOf(o, d.turnNow(o), o.Member, stable, WakeQuiet)
	if withProgress && d.Progress != nil && o.Session.ID != "" {
		if s, err := d.Progress.Steps(o.Session.ID); err != nil {
			r.StepsErr = firstLine(err.Error())
		} else {
			r.Stage, r.StageAt, r.Labels, r.Branch, r.PRRef, r.PRs = s.Stage, s.StageAt, s.Labels, s.Branch, s.PR, s.PRs
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
