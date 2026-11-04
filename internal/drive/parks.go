package drive

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
)

// parkEnd is what on record brings a parked session back to its driver: an
// open ask about it, which the owner's answer settles; a hold it waits for,
// whose release watch --due wakes it on; or the time it was parked until,
// which the watchdog tells its driver of. A park with none of them is
// forgotten by a driver that keeps nothing between runs, so park refuses it.
type parkEnd struct {
	Asks  []string  // open asks about the session
	Hold  string    // the hold it waits for
	Until time.Time // when its driver should look at it again
}

func (e parkEnd) none() bool { return len(e.Asks) == 0 && e.Hold == "" && e.Until.IsZero() }

// due says its --until has come.
func (e parkEnd) due(now time.Time) bool { return !e.Until.IsZero() && !now.Before(e.Until) }

// words says what ends it, as status prints it after "parked".
func (e parkEnd) words(zone *time.Location) string {
	var out []string
	if len(e.Asks) > 0 {
		out = append(out, "on "+strings.Join(e.Asks, ", "))
	}
	if e.Hold != "" {
		out = append(out, "behind "+e.Hold)
	}
	if !e.Until.IsZero() {
		out = append(out, "until "+e.Until.In(zone).Format("01-02 15:04"))
	}
	if len(out) == 0 {
		return "with nothing to end it"
	}
	return strings.Join(out, ", ")
}

// endOf reads what ends m's park from the record; until stands in for the
// park's own time when a park is being decided.
func endOf(evs []batch.Event, m *batch.Member, until time.Time) parkEnd {
	e := parkEnd{Until: until}
	if e.Until.IsZero() {
		e.Until = m.ParkedUntil
	}
	_, asks, _ := batch.Records(evs)
	for _, a := range asks {
		if a.Answer == nil && a.Session == m.Label {
			e.Asks = append(e.Asks, a.ID)
		}
	}
	for _, w := range batch.Waiting(evs) {
		if w.Session == m.Label {
			e.Hold = w.Name
		}
	}
	return e
}

// unended is the refusal of a park nothing on record ends, naming the three
// ways a park can end, and close for a session with nothing left to do.
func (d *Driver) unended(label, view, reason string) *Refused {
	return &Refused{
		Reason: fmt.Sprintf("nothing on record ends %s's park, so no wake would bring it back: no ask about it is open, it waits for no hold, and no --until was given", label),
		Next:   d.parkWays(label, view, reason),
	}
}

// parkWays says how to park a session so that something on record ends the
// park, or to close it.
func (d *Driver) parkWays(label, view, reason string) string {
	if reason == "" {
		reason = "…"
	}
	return fmt.Sprintf("if it waits on the owner, ask (%s) and park it again; "+
		"if it waits for a date or for something outside the batch, park it until you should look again (%s; WHEN is a date, such as 2026-11-06 09:00, or a duration, such as 4h); "+
		"if nothing is left for it to do, close it (%s)",
		d.run("record", "ask", label, "--question", "…", "--recommendation", "…", "--if-unanswered", "…"),
		d.run("park", view, "--until", "WHEN", text(reason)),
		d.run("close", view))
}

// parkNeed is a parked session whose park needs its driver: its --until has
// come (Until set), or nothing on record ends it (Until zero), as after an ask
// answered with no step.
type parkNeed struct {
	Label, View string
	Until       time.Time
}

// parkNeedOf is r's park need, or nil when its park needs nothing of the driver.
func (d *Driver) parkNeedOf(r Row) *parkNeed {
	end := r.Wake.Park
	if end == nil || !end.due(d.now()) && !end.none() {
		return nil
	}
	n := &parkNeed{Label: r.Label, View: viewOrEmpty(r.View)}
	if n.View == "" {
		n.View = r.Label + "@…"
	}
	if end.due(d.now()) {
		n.Until = end.Until
	}
	return n
}

// parkLines are the "park of" lines status prints under the table, and the
// watchdog posts: one per kind of need, naming every session with it, so what
// to run is said once however many sessions share it. A line about one
// session gives its own commands; one about several gives them with LABEL and
// VIEW to fill from the list.
func (d *Driver) parkLines(needs []parkNeed, zone *time.Location) []string {
	var due, unended []parkNeed
	for _, n := range needs {
		if n.Until.IsZero() {
			unended = append(unended, n)
		} else {
			due = append(due, n)
		}
	}
	var out []string
	switch len(due) {
	case 0:
	case 1:
		n := due[0]
		out = append(out, fmt.Sprintf("park of %s until %s has come: read it (%s), then send its next step, park it again with a later --until (%s), or close it (%s)",
			n.Label, n.Until.In(zone).Format("01-02 15:04"), d.run("read", n.Label),
			d.run("park", n.View, "--until", "WHEN", text("…")), d.run("close", n.View)))
	default:
		var names []string
		for _, n := range due {
			names = append(names, fmt.Sprintf("%s (until %s, %s)", n.Label, n.Until.In(zone).Format("01-02 15:04"), n.View))
		}
		out = append(out, fmt.Sprintf("park of %s: each --until has come; read each (%s), then send its next step, park it again with a later --until (%s), or close it (%s)",
			andList(names), d.run("read", "LABEL"), d.run("park", "VIEW", "--until", "WHEN", text("…")), d.run("close", "VIEW")))
	}
	switch len(unended) {
	case 0:
	case 1:
		n := unended[0]
		out = append(out, fmt.Sprintf("park of %s has nothing on record to end it, so no wake brings it back: %s", n.Label, d.parkWays(n.Label, n.View, "")))
	default:
		var names []string
		for _, n := range unended {
			names = append(names, fmt.Sprintf("%s (%s)", n.Label, n.View))
		}
		out = append(out, fmt.Sprintf("park of %s: nothing on record ends these parks, so no wake brings them back; for each, %s",
			andList(names), d.parkWays("LABEL", "VIEW", "")))
	}
	return out
}

// andList joins items as "a, b and c".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// askAbout is the record ask that puts question, about label, to the owner.
func (d *Driver) askAbout(label, question, ifUnanswered string) string {
	return d.run("record", "ask", label, "--question", question, "--if-unanswered", ifUnanswered)
}

// LeftParked says what to do about the session an answered ask was about,
// when the answer left it parked with nothing on record to end the park; ""
// otherwise. An answer is the owner's word and always records, so this is
// the next: of the answer, not a refusal.
func (d *Driver) LeftParked(ask string) string {
	b, err := d.Batch()
	if err != nil {
		return ""
	}
	evs, err := b.Events()
	if err != nil {
		return ""
	}
	_, asks, _ := batch.Records(evs)
	for _, a := range asks {
		if a.ID != ask || a.Session == "" {
			continue
		}
		m := batch.Find(batch.Members(evs), a.Session)
		if m == nil || m.Closed || m.Parked == "" || !endOf(evs, m, time.Time{}).none() {
			return ""
		}
		// A parked session has not moved since, as a rule, so the park's own
		// view is the one to act on; one that moved refuses and says its view.
		view := m.Label + "@…"
		for _, e := range evs {
			if e.Kind == batch.KindPark && e.Session == m.Label && e.View != "" {
				view = e.View
			}
		}
		return fmt.Sprintf("%s stays parked with nothing on record to end it now that %s is answered: send it the step the answer leads to (%s); if it still waits, %s",
			m.Label, ask, d.run("send", view, text("…")), d.parkWays(m.Label, view, m.Parked))
	}
	return ""
}
