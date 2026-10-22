package batch

import (
	"fmt"
	"strings"
	"time"
)

// Event kinds.
const (
	KindBatch      = "batch"
	KindOpenIntent = "open-intent" // a session is being opened under this label
	KindOpen       = "open"        // it opened: its block, account, directory
	KindAdopt      = "adopt"       // a session drive did not open joined the batch
	KindSendIntent = "send-intent" // recorded before typing, so a crash leaves a trace
	KindSend       = "send"        // how a send ended: Outcome arrived, queued or unknown
	KindRespond    = "respond"     // approve, deny or interrupt
	KindWatch      = "watch"       // a wake was armed
	KindWake       = "wake"        // a wake was posted (HTTP set) or failed (Reason set)
	KindPark       = "park"
	KindClose      = "close"
	KindRefused    = "refused" // an action drive would not take, and why
	KindFailure    = "failure" // an action that went wrong in the session
	KindGrant      = "grant"   // authority the owner handed over
	KindAsk        = "ask"     // a question put to the owner
	KindAnswer     = "answer"
	KindNote       = "note"
)

// Event is one line of the log. Fields not used by a kind stay empty.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"` // the batch's label for it
	Block   string    `json:"block,omitempty"`
	SID     string    `json:"sid,omitempty"`
	Account string    `json:"account,omitempty"`
	Dir     string    `json:"dir,omitempty"`

	Intent  string `json:"intent,omitempty"`  // joins a send-intent or open-intent to its outcome
	Mode    string `json:"mode,omitempty"`    // typed or file
	Key     string `json:"key,omitempty"`     // the snippet a step came from
	Path    string `json:"path,omitempty"`    // the file sent
	Text    string `json:"text,omitempty"`    // what was typed into the session
	Notice  bool   `json:"notice,omitempty"`  // the step carried the notice
	Outcome string `json:"outcome,omitempty"` // arrived, queued, unknown, recovered, …
	Reason  string `json:"reason,omitempty"`
	Hold    string `json:"hold,omitempty"`
	View    string `json:"view,omitempty"` // the view an action was decided on
	Action  string `json:"action,omitempty"`
	Call    string `json:"call,omitempty"` // the tool call a respond answered

	ID             string `json:"id,omitempty"` // G1, A1, N1
	Ref            string `json:"ref,omitempty"`
	Source         string `json:"source,omitempty"` // the owner's words, and where they were said
	Scope          string `json:"scope,omitempty"`
	Question       string `json:"question,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	IfUnanswered   string `json:"if_unanswered,omitempty"`
	Took           string `json:"took,omitempty"` // recommended, amended, delegated, moot
	Explained      bool   `json:"explained,omitempty"`
	Note           string `json:"note,omitempty"`
	HTTP           int    `json:"http,omitempty"`
	Try            int    `json:"try,omitempty"`
}

// Member is a session that belongs to the batch.
type Member struct {
	Label   string
	Block   string
	SID     string
	Account string
	Dir     string
	Joined  time.Time
	Parked  string // why, while parked; a later send unparks
	Closed  bool
	// LastAction is the latest thing a driver did to it: a send, a respond, a park.
	LastAction time.Time
	LastSend   *Event
	Sends      int
	Watched    time.Time // when its wake was last armed
	Woken      time.Time // when its wake was last posted
}

// Members derives the batch's sessions, in the order they joined.
func Members(evs []Event) []*Member {
	var order []*Member
	by := map[string]*Member{}
	for i := range evs {
		e := &evs[i]
		m := by[e.Session]
		switch e.Kind {
		case KindOpen, KindAdopt:
			m = &Member{Label: e.Session, Block: e.Block, SID: e.SID, Account: e.Account, Dir: e.Dir, Joined: e.At}
			if old, ok := by[e.Session]; ok {
				*old = *m // reopened under the same label
				m = old
			} else {
				order = append(order, m)
			}
			by[e.Session] = m
		case KindSend:
			if m != nil && e.Outcome != "" {
				m.LastAction, m.LastSend, m.Parked = e.At, e, ""
				m.Sends++
			}
		case KindRespond:
			if m != nil {
				m.LastAction = e.At
			}
		case KindPark:
			if m != nil {
				m.Parked, m.LastAction = e.Reason, e.At
			}
		case KindClose:
			if m != nil {
				m.Closed = true
			}
		case KindWatch:
			if m != nil {
				m.Watched = e.At
			}
		case KindWake:
			if m != nil && e.HTTP != 0 {
				m.Woken = e.At
			}
		}
	}
	return order
}

// Find resolves a label or a block id (its last six characters are enough)
// to a member.
func Find(members []*Member, ref string) *Member {
	for _, m := range members {
		if m.Label == ref {
			return m
		}
	}
	for _, m := range members {
		if m.Block == ref || strings.HasSuffix(m.Block, ref) && len(ref) >= 6 {
			return m
		}
	}
	return nil
}

// InFlight is how long an intent with no outcome counts as still running.
const InFlight = 3 * time.Minute

// Pending returns the newest intent for a session with no outcome yet.
func Pending(evs []Event, session string) *Event {
	done := map[string]bool{}
	for _, e := range evs {
		if (e.Kind == KindSend || e.Kind == KindFailure || e.Kind == KindOpen) && e.Intent != "" {
			done[e.Intent] = true
		}
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if (e.Kind == KindSendIntent || e.Kind == KindOpenIntent) && e.Session == session && !done[e.Intent] {
			return &evs[i]
		}
	}
	return nil
}

// Hold is a resource a session holds while the turn a send started runs.
type Hold struct {
	Name    string
	Session string
	Since   time.Time
	Step    string // for a session waiting for the hold: the step it was refused
}

// Holds lists the newest taking of each hold. Whether it is still held
// depends on the holder's turn, which only the session can say.
func Holds(evs []Event) []Hold {
	latest := map[string]Hold{}
	var names []string
	for _, e := range evs {
		if e.Kind == KindSend && e.Hold != "" && (e.Outcome == "arrived" || e.Outcome == "queued") {
			if _, ok := latest[e.Hold]; !ok {
				names = append(names, e.Hold)
			}
			latest[e.Hold] = Hold{Name: e.Hold, Session: e.Session, Since: e.At}
		}
	}
	out := make([]Hold, 0, len(names))
	for _, n := range names {
		out = append(out, latest[n])
	}
	return out
}

// Waiting lists the sessions a send was refused for because another held
// what it needed, and that have been sent nothing since.
func Waiting(evs []Event) []Hold {
	refused := map[string]Hold{}
	var order []string
	for _, e := range evs {
		switch {
		case e.Kind == KindRefused && e.Hold != "":
			if _, ok := refused[e.Session]; !ok {
				order = append(order, e.Session)
			}
			if _, ok := refused[e.Session]; !ok || refused[e.Session].Name != e.Hold {
				refused[e.Session] = Hold{Name: e.Hold, Session: e.Session, Since: e.At, Step: e.Text}
			}
		case e.Kind == KindSend && e.Outcome != "":
			delete(refused, e.Session)
		}
	}
	var out []Hold
	for _, s := range order {
		if h, ok := refused[s]; ok {
			out = append(out, h)
			delete(refused, s)
		}
	}
	return out
}

// Ask is a question put to the owner, with its answer once given.
type Ask struct {
	Event
	Answer *Event
}

// Records derives the owner's grants, the asks with their answers, and notes.
func Records(evs []Event) (grants []Event, asks []*Ask, notes []Event) {
	byID := map[string]*Ask{}
	for _, e := range evs {
		switch e.Kind {
		case KindGrant:
			grants = append(grants, e)
		case KindAsk:
			a := &Ask{Event: e}
			asks = append(asks, a)
			byID[e.ID] = a
		case KindAnswer:
			if a := byID[e.Ref]; a != nil {
				ans := e
				a.Answer = &ans
			}
		case KindNote:
			notes = append(notes, e)
		}
	}
	return
}

// NextID is the next record id with a prefix: G1, A3.
func NextID(evs []Event, kind, prefix string) string {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return fmt.Sprintf("%s%d", prefix, n+1)
}
