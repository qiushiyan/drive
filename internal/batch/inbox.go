package batch

// A session batch's driver is a Claude Code session, which no webhook
// reaches: each wake is left in the log, and the driver's listener takes
// every wake left since the last it took. A wake left is delivered, as a
// webhook's 200 is; neither says the driver heard it, which only a later
// command of the driver's own says.

import "slices"

// Driving is the Claude Code session that drives a session batch: its newest
// registration, which a batch new or a batch use from that session wrote.
func Driving(evs []Event) (Event, bool) {
	for _, ev := range slices.Backward(evs) {
		if ev.Kind == KindDriver {
			return ev, true
		}
	}
	return Event{}, false
}

// Untaken is every wake left for the batch's listener since it last took
// them, oldest first: the endings a post carried too, each under its own
// session.
func Untaken(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		switch {
		case e.Kind == KindTaken:
			out = out[:0]
		case e.Kind == KindWake && e.Inbox:
			out = append(out, e)
		}
	}
	return out
}
