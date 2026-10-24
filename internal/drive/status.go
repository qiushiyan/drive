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
	r := &Reading{Label: m.Label, SID: m.SID, State: state, Account: m.Account}
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
	Label     string
	SID       string
	Block     string
	Window    string
	State     State
	For       time.Duration // since its state last changed
	Account   string
	Input     turn.InputKind
	Typed     string
	Stage     string
	PR        *progress.PR
	PRRef     *progress.PRRef
	PRErr     string
	LastSent  string // the last step a driver sent, and when
	SentAt    time.Time
	Watch     string // armed, woken at …, parked: why, none
	View      turn.View
	Member    bool
	Name      string          // the block's Rex label, for sessions outside the batch
	Error     string          // what kept the row from being read
	Unhandled bool            // a wake was posted after the driver last acted on it
	Due       string          // why the watchdog should arm its wake; "" when nothing is due
	Again     time.Time       // when the watchdog posts a wake nobody acted on once more, while that is still to come
	Jobs      []session.Shell // background jobs it runs
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
	out := &Status{Zone: time.Local}
	if b != nil {
		out.Batch, out.Zone, out.Dir = &b.Meta, b.Zone(), b.Dir
	}
	session := ""
	if b != nil {
		session = b.Meta.RexSession
	}
	blocks, err := d.Term.Blocks(session)
	if err != nil {
		return nil, err
	}
	window := map[string]string{}
	for _, bl := range blocks {
		window[bl.ID] = bl.Window
	}
	var wg sync.WaitGroup
	seen := map[string]bool{}
	var open []*batch.Member
	for _, m := range members {
		seen[m.Block] = true
		if m.Closed {
			out.Closed++
			continue
		}
		open = append(open, m)
	}
	// Each reader fills its own row of a table sized up front.
	out.Rows = make([]Row, len(open))
	for i, m := range open {
		wg.Add(1)
		go func(i int, m *batch.Member) {
			defer wg.Done()
			r := d.row(b, members, m.Label, withProgress)
			r.Window, r.Member = window[m.Block], true
			if m.LastSend != nil {
				r.LastSent, r.SentAt = sentLabel(m.LastSend), m.LastSend.At
			}
			r.Watch, r.Unhandled, r.Due, r.Again = d.wakeOf(b, m, r, out.Zone)
			out.Rows[i] = r
		}(i, m)
	}
	// Other Claude sessions are read the same way, each into its own slot.
	others := make([]*Row, len(blocks))
	for i, bl := range blocks {
		if seen[bl.ID] {
			continue
		}
		wg.Add(1)
		go func(i int, bl rex.Block) {
			defer wg.Done()
			if st, err := d.Term.Status(bl.ID); err != nil || st == nil {
				return
			}
			r := d.row(nil, nil, bl.ID, false)
			if r.Error != "" {
				return
			}
			r.Label, r.Window, r.Name = rex.Short(bl.ID), bl.Window, bl.Label
			others[i] = &r
		}(i, bl)
	}
	wg.Wait()
	for _, r := range others {
		if r != nil {
			out.Others = append(out.Others, *r)
		}
	}
	if b != nil {
		names := map[string]bool{}
		for _, h := range batch.Holds(evs) {
			names[h.Name] = true
		}
		for _, w := range batch.Waiting(evs) {
			names[w.Name] = true
		}
		for _, name := range sortedKeys(names) {
			line := name + ": free"
			hh, held := d.holder(evs, members, name, "")
			if held {
				line = fmt.Sprintf("%s: held by %s since %s, %s", name, hh.Session, hh.Since.In(out.Zone).Format("15:04"), hh.why)
			}
			var waiting []string
			for _, w := range batch.Waiting(evs) {
				if w.Name != name {
					continue
				}
				waiting = append(waiting, fmt.Sprintf("%s (refused %s at %s)", w.Session, clip(join(stepWords(refusedStep(w))...), 60), w.Since.In(out.Zone).Format("15:04")))
				// A parked waiter whose hold is free wakes no one unless its watch is armed again.
				for i := range out.Rows {
					if r := &out.Rows[i]; !held && r.Label == w.Session && strings.HasPrefix(r.Watch, "parked") {
						r.Due = "waits for " + name + ", which is free"
						r.Watch += "; " + name + " is free now, and " + d.run("watch", "--due") + " wakes it"
					}
				}
			}
			if len(waiting) > 0 {
				line += "; waiting for it: " + strings.Join(waiting, ", ")
			} else if strings.HasSuffix(line, ": free") {
				continue
			}
			out.Holds = append(out.Holds, line)
		}
		grants, asks, _ := batch.Records(evs)
		out.Grants = grants
		for _, a := range asks {
			if a.Answer == nil {
				out.Asks = append(out.Asks, a)
			}
		}
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
				out.InFlight = append(out.InFlight, InFlight{Event: p, Driver: d.Store.Busy(key)})
			}
		}
	}
	return out, nil
}

// wakeOf says how a member's wake stands, first whether a watch runs and
// then what the log says came of the last one, and why the watchdog should
// arm it again ("" when nothing is due).
// repostAfter is how long a wake nobody acted on waits before the watchdog
// posts it once more: a run acting on it takes minutes.
const repostAfter = 10 * time.Minute

func (d *Driver) wakeOf(b *batch.Batch, m *batch.Member, r Row, zone *time.Location) (word string, unhandled bool, due string, again time.Time) {
	alive := d.Watches != nil && b != nil && d.Watches.Alive(watchName(b, m.Block))
	woken := !m.Woken.IsZero() && !m.Woken.Before(m.Watched)
	switch {
	case m.Parked != "":
		return "parked: " + m.Parked, false, "", again
	case alive && stuck(r.State, d.now().Add(-r.For), m.Watched, d.now()):
		return fmt.Sprintf("armed, but its turn ended %s ago: stuck (%s restarts it)", ago(r.For), d.run("watch", m.Label)), false, "stuck", again
	case alive:
		return "armed", false, "", again
	case m.Posting.After(m.Watched) && m.Posting.After(m.Woken) && m.Posting.After(m.WakeFailed):
		return "posting since " + m.Posting.In(zone).Format("15:04"), false, "", again
	case woken && m.Woken.After(m.LastAction):
		// The driver owes it a decision. A session that moved on by itself
		// since, such as one a usage limit's reset continued, is woken again
		// when that turn ends; otherwise the wake is posted once more, so one
		// lost to a run that died is not lost, and never again for one view.
		word = "woken " + m.Woken.In(zone).Format("15:04") + " (nothing done since"
		switch {
		case r.View.Session != "" && m.WokenView != "" && r.View.String() != m.WokenView:
			due = "it moved on by itself since its last wake"
		case m.WokenView == m.PrevView:
			word += ", and posted again already"
		case d.now().Sub(m.Woken) > repostAfter:
			due = "woken " + ago(d.now().Sub(m.Woken)) + " ago with nothing done since"
		default:
			again = m.Woken.Add(repostAfter)
			word += "; " + d.run("watch", "--due") + " posts it again from " + again.In(zone).Format("15:04")
		}
		return word + ")", true, due, again
	case woken:
		return "none (woken " + m.Woken.In(zone).Format("15:04") + ", acted on since)", false, "acted on with no wake armed", again
	case !m.Watched.IsZero():
		return "ended without a wake (watches.log says why)", false, "its watch ended without a wake", again
	}
	return "none", false, "never watched", again
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
		if s, err := d.Progress.Steps(o.Session.ID); err == nil {
			r.Stage, r.PRRef = s.Stage, s.PR
			if s.PR != nil {
				if pr, err := d.Progress.PR(*s.PR); err == nil {
					r.PR = &pr
				} else {
					r.PRErr = firstLine(err.Error())
				}
			}
		}
	}
	return r
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
