package batch

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Event kinds.
const (
	KindBatch      = "batch"
	KindOpenIntent = "open-intent"  // a session is being opened under this label
	KindOpen       = "open"         // it opened: its block, account, directory
	KindAdopt      = "adopt"        // a session drive did not open joined the batch
	KindSendIntent = "send-intent"  // recorded before typing, so a crash leaves a trace
	KindSend       = "send"         // a send that reached its session, or may have: Outcome arrived, queued, recovered, garbled, other (something else came) or unknown
	KindRespond    = "respond"      // approve, deny or interrupt
	KindWatch      = "watch"        // a wake was armed
	KindWakeStop   = "wake-stopped" // a wake was stopped, as before Rex's server restarts
	KindPosting    = "posting"      // a watch began posting its wake
	KindWake       = "wake"         // a wake was posted (HTTP set) or failed (Reason set)
	KindPark       = "park"
	KindClose      = "close"
	KindRefused    = "refused" // an action drive would not take, and why
	KindFailure    = "failure" // an action that went wrong in the session
	KindGrant      = "grant"   // authority the owner handed over
	KindAsk        = "ask"     // a question put to the owner
	KindAnswer     = "answer"
	KindNote       = "note"
	KindGoal       = "goal" // what a session sets out to do, for the owner; the latest per label stands
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

	ID             string `json:"id,omitempty"`     // G1, A1, N1
	Ref            string `json:"ref,omitempty"`    // the ask an answer answers; the session an open resumed
	Source         string `json:"source,omitempty"` // the owner's words, and where they were said
	Scope          string `json:"scope,omitempty"`
	Question       string `json:"question,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	IfUnanswered   string `json:"if_unanswered,omitempty"`
	Took           string `json:"took,omitempty"` // recommended, amended, delegated, moot
	Explained      bool   `json:"explained,omitempty"`
	Note           string `json:"note,omitempty"`
	Goal           string `json:"goal,omitempty"` // on a goal, or an open-intent that sets one
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
	// Goal is what the session sets out to do, in plain terms for the owner:
	// the latest recorded for its label, which outlives a reopen and a
	// relaunch under that label.
	Goal     string
	Parked   string    // why, while parked; a later send unparks
	ParkedAt time.Time // so what the park says can be read against what happened since
	Closed   bool
	ClosedAt time.Time
	Resumed  bool // it carries on a session that read the notice in this batch
	// LastAction is the latest thing a driver did to it: a send, a respond, a park.
	LastAction time.Time
	LastSend   *Event
	// Noticed says a send that carried the notice reached the session:
	// arrived, queued or recovered. One left unknown or garbled may not have.
	Noticed    bool
	Watched    time.Time // when its wake was last armed
	Stopped    time.Time // when its wake was last stopped by watch --stop-all
	Posting    time.Time // when a watch last began posting
	Woken      time.Time // when its wake was last posted
	WokenView  string    // the view that wake carried
	PrevView   string    // the view the wake before it carried
	WakeFailed time.Time // when a watch last ended without a wake
}

// Members derives the batch's sessions, in the order they joined.
func Members(evs []Event) []*Member {
	var order []*Member
	by := map[string]*Member{}
	read := map[string]bool{} // session ids that read the notice
	goals := map[string]string{}
	opening := map[string]string{} // an open's intent, to the goal it sets once the block opens
	for i := range evs {
		e := &evs[i]
		m := by[e.Session]
		switch e.Kind {
		case KindOpenIntent:
			if e.Goal != "" {
				opening[e.Intent] = e.Goal
			}
		case KindGoal:
			goals[e.Session] = e.Goal
		case KindOpen, KindAdopt:
			// An open's goal stands once its block opened; one that failed
			// leaves the label's goal as it was.
			if g, ok := opening[e.Intent]; ok && e.Intent != "" {
				goals[e.Session] = g
				delete(opening, e.Intent)
			}
			m = &Member{Label: e.Session, Block: e.Block, SID: e.SID, Account: e.Account, Dir: e.Dir, Joined: e.At, Resumed: read[e.Ref]}
			if old, ok := by[e.Session]; ok {
				if e.Ref != "" && e.Ref == old.SID {
					// Its own conversation resumed in a new block, as after Rex
					// restarted: what its driver did with it stands, its park
					// among it. Its wake is the new block's.
					m.SID = cmp.Or(m.SID, old.SID)
					m.Parked, m.ParkedAt, m.LastAction, m.LastSend, m.Noticed = old.Parked, old.ParkedAt, old.LastAction, old.LastSend, old.Noticed
				}
				*old = *m // reopened under the same label
				m = old
			} else {
				order = append(order, m)
			}
			by[e.Session] = m
		case KindSend:
			if m != nil && e.Outcome != "" {
				m.LastAction, m.LastSend, m.Parked = e.At, e, ""
				if e.Notice && (e.Outcome == "arrived" || e.Outcome == "queued" || e.Outcome == "recovered") {
					m.Noticed, read[m.SID] = true, true
				}
			}
		case KindRespond:
			if m != nil {
				m.LastAction = e.At
			}
		case KindPark:
			if m != nil {
				m.Parked, m.ParkedAt, m.LastAction = e.Reason, e.At, e.At
			}
		case KindClose:
			if m != nil {
				m.Closed, m.ClosedAt = true, e.At
			}
		case KindWatch:
			if m != nil {
				m.Watched = e.At
			}
		case KindWakeStop:
			if m != nil {
				m.Stopped = e.At
			}
		case KindPosting:
			if m != nil {
				m.Posting = e.At
			}
		case KindWake:
			switch {
			case m == nil:
			case e.HTTP != 0:
				m.Woken, m.PrevView, m.WokenView = e.At, m.WokenView, e.View
			default:
				m.WakeFailed = e.At
			}
		case KindFailure:
			if m != nil && strings.HasPrefix(e.Reason, "watch ended without a wake") {
				m.WakeFailed = e.At
			}
		}
	}
	for _, m := range order {
		m.Goal = goals[m.Label]
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

// resolved says which intents have an outcome: a send, an open, or a
// failure. sent says which of those reached the session.
func resolved(evs []Event) (done, sent map[string]bool) {
	done, sent = map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		if e.Intent == "" {
			continue
		}
		switch e.Kind {
		case KindSend, KindOpen:
			done[e.Intent], sent[e.Intent] = true, true
		case KindFailure:
			done[e.Intent] = true
		}
	}
	return done, sent
}

// Unresolved returns a session's intents with no outcome yet, oldest first.
// Each one's driver is either still acting, holding the session's lock, or
// has gone; the next driver to own the session settles what it left.
func Unresolved(evs []Event, session string) []Event {
	done, _ := resolved(evs)
	var out []Event
	for _, e := range evs {
		if (e.Kind == KindSendIntent || e.Kind == KindOpenIntent) && e.Session == session && !done[e.Intent] {
			out = append(out, e)
		}
	}
	return out
}

// Hold is a resource a session holds from the moment its send is admitted
// until the turn that send started has ended.
type Hold struct {
	Name    string
	Session string
	Since   time.Time // when the send that took it was admitted
	Intent  string
	Pending bool   // that send has no outcome yet
	Step    string // for a session waiting for the hold: the step it was refused
	Key     string // that step's snippet, when it was one
	Path    string // that step's file, when it was one
	Open    bool   // the refused action was an open: Step is the drive open to run
}

// Holds lists the newest taking of each hold. A send that failed before it
// reached its session took nothing. Whether a taking still holds depends on
// the holder's turn, which only the session can say.
func Holds(evs []Event) []Hold {
	done, sent := resolved(evs)
	takings := map[string][]Hold{}
	var names []string
	for _, e := range evs {
		if e.Kind == KindSendIntent && e.Hold != "" {
			if _, ok := takings[e.Hold]; !ok {
				names = append(names, e.Hold)
			}
			takings[e.Hold] = append(takings[e.Hold], Hold{Name: e.Hold, Session: e.Session, Since: e.At, Intent: e.Intent, Pending: !done[e.Intent]})
		}
	}
	var out []Hold
	for _, n := range names {
		ts := takings[n]
		for _, t := range slices.Backward(ts) {
			if t.Pending || sent[t.Intent] {
				out = append(out, t)
				break
			}
		}
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
				refused[e.Session] = Hold{Name: e.Hold, Session: e.Session, Since: e.At, Step: e.Text, Key: e.Key, Path: e.Path, Open: e.Mode == "open"}
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
