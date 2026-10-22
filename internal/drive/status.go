package drive

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/turn"
)

// ReadMode picks what Read shows.
type ReadMode string

const (
	ReadLast    ReadMode = "last"    // the newest reply
	ReadPrompts ReadMode = "prompts" // what it has been told
	ReadPending ReadMode = "pending" // the tool call a prompt waits on
	ReadUser    ReadMode = "user"    // the newest prompt, as it arrived
)

// Reading is one session read for a decision.
type Reading struct {
	Label   string
	State   string
	Account string
	Input   turn.InputKind
	Typed   string
	Text    string
	View    turn.View
	At      time.Time // when the text was written
}

// Read reads a session and returns the view to act on.
func (d *Driver) Read(ref string, mode ReadMode, n int) (*Reading, error) {
	_, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	o, err := d.observe(members, ref)
	if err != nil {
		return nil, err
	}
	r := &Reading{Label: o.Label, State: o.State(), Account: o.Session.Account, Input: o.Input, Typed: o.Typed, View: o.View}
	f := o.Frontier
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
			fmt.Fprintf(&b, "--- %s%s\n%s\n\n", p.At.In(d.zone()).Format("01-02 15:04"), q, promptText(p))
		}
		r.Text = strings.TrimRight(b.String(), "\n")
	case ReadPending:
		if f.Pending == nil {
			return r, &Refused{Reason: fmt.Sprintf("%s has no tool call waiting", o.Label)}
		}
		r.Text = f.Pending.Name + ":\n" + f.Pending.Input
	case ReadUser:
		p := f.Newest()
		if p == nil {
			return r, &Refused{Reason: fmt.Sprintf("%s has received no prompt yet", o.Label)}
		}
		r.Text, r.At = promptText(*p), p.At
		if p.Queued {
			r.Text = "(queued) " + r.Text
		}
	default:
		if f.Error != nil {
			r.Text, r.At = fmt.Sprintf("[%s] %s", f.Error.Kind, f.Error.Text), f.Error.At
		} else if f.Reply != nil {
			r.Text, r.At = f.Reply.Text, f.Reply.At
		} else {
			r.Text = "(no reply yet)"
		}
	}
	return r, nil
}

// Row is one session in the status table.
type Row struct {
	Label     string
	Block     string
	Window    string
	State     string
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
	Name      string // the block's Rex label, for sessions outside the batch
	Error     string // what kept the row from being read
	Unhandled bool   // a wake was posted after the driver last acted on it
}

// Status is the batch at a glance.
type Status struct {
	Batch    *batch.Meta
	Dir      string // the batch's directory
	Zone     *time.Location
	Rows     []Row
	Others   []Row // Claude sessions in Rex outside the batch
	Holds    []string
	Asks     []*batch.Ask
	Grants   []batch.Event
	InFlight []batch.Event
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
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, m := range members {
		seen[m.Block] = true
		if m.Closed {
			continue
		}
		i := len(out.Rows)
		out.Rows = append(out.Rows, Row{Label: m.Label, Block: m.Block, Window: window[m.Block], Member: true})
		wg.Add(1)
		go func(m *batch.Member) {
			defer wg.Done()
			r := d.row(members, m.Label, withProgress)
			r.Window, r.Member = window[m.Block], true
			if m.LastSend != nil {
				r.LastSent, r.SentAt = sentLabel(m.LastSend), m.LastSend.At
			}
			// The log says what happened; the process table only whether a watch still runs.
			switch {
			case m.Parked != "":
				r.Watch = "parked: " + m.Parked
			case !m.Woken.IsZero() && !m.Woken.Before(m.Watched):
				r.Watch = "woken " + m.Woken.In(out.Zone).Format("15:04")
				r.Unhandled = m.Woken.After(m.LastAction)
			case d.Watches != nil && d.Watches.Alive(watchName(m.Block)):
				r.Watch = "armed"
				if stuck(r) {
					r.Watch = fmt.Sprintf("armed, but its turn ended %s ago: stuck (drive watch %s restarts it)", ago(r.For), m.Label)
				}
			case !m.Watched.IsZero():
				r.Watch = "ended without a wake (watches.log says why)"
			default:
				r.Watch = "none"
			}
			mu.Lock()
			out.Rows[i] = r
			mu.Unlock()
		}(m)
	}
	for _, bl := range blocks {
		if seen[bl.ID] {
			continue
		}
		if st, err := d.Term.Status(bl.ID); err == nil && st != nil {
			out.Others = append(out.Others, Row{Label: rex.Short(bl.ID), Block: bl.ID, Window: bl.Window, State: stateWord(st),
				For: d.now().Sub(st.UpdatedAt), Name: bl.Label})
		}
	}
	wg.Wait()
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
			if hh, held := d.holder(evs, members, name, ""); held {
				line = fmt.Sprintf("%s: held by %s since %s", name, hh.Session, hh.Since.In(out.Zone).Format("15:04"))
			}
			var waiting []string
			for _, w := range batch.Waiting(evs) {
				if w.Name == name {
					waiting = append(waiting, fmt.Sprintf("%s (refused %s at %s)", w.Session, clip(w.Step, 60), w.Since.In(out.Zone).Format("15:04")))
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
		for _, m := range members {
			if p := batch.Pending(evs, m.Label); p != nil {
				out.InFlight = append(out.InFlight, *p)
			}
		}
	}
	return out, nil
}

// stuck says a watch still runs long after the turn it waits on ended: a
// wait returns a minute after a turn ends, or ten after one that left its
// own background jobs running.
func stuck(r Row) bool {
	ended := r.State == "done" || r.State == "error" || strings.HasPrefix(r.State, "blocked")
	return ended && r.For > 15*time.Minute
}

func ago(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stateWord(st *rex.Status) string {
	if st.State == "blocked" && st.Kind != "" {
		return "blocked:" + st.Kind
	}
	return st.State
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

func (d *Driver) row(members []*batch.Member, label string, withProgress bool) Row {
	r := Row{Label: label}
	o, err := d.observe(members, label)
	if err != nil {
		r.State, r.Error = "gone", firstLine(err.Error())
		return r
	}
	st := o.Session.Status
	r.Block, r.State, r.For, r.Account = o.Session.Block, o.State(), d.now().Sub(st.UpdatedAt), o.Session.Account
	r.Input, r.Typed, r.View = o.Input, o.Typed, o.View
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
