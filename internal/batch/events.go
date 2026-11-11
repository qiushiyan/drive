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
	KindFree       = "free"    // a driver handed back a hold only a job of its holder kept: Hold, Session its holder, Reason the driver's
	KindRefused    = "refused" // an action drive would not take, and why
	KindFailure    = "failure" // an action that went wrong in the session
	KindGrant      = "grant"   // authority the owner handed over
	KindAsk        = "ask"     // a question put to the owner; Ref the open ask it replaces, recorded moot in the same write
	KindAnswer     = "answer"
	// KindRestate is what an open ask (Ref) waits on now, in IfUnanswered,
	// once its session closed or its change merged and it still holds
	// something.
	KindRestate = "restate"
	KindNote    = "note"
	KindGoal    = "goal" // what a session sets out to do, for the owner; the latest per label stands
	KindSeen    = "seen" // a driver read the batch, which is how a run that changes nothing is heard
	// KindCleanup is what became of a branch a closed session left: Outcome
	// removed, unmerged, left (Reason says why), unread (it could not be
	// judged, Reason says why) or none (it left nothing).
	KindCleanup = "cleanup"
	// KindAfterWake is the batch's after-wake commands having run.
	KindAfterWake = "after-wake"
	// KindHeld is a timer that fell due while the driver is not answering, and
	// waits: Note is the timer's. A wake with that note ends it.
	KindHeld = "held"
	// KindTold is something the watchdog told the driver of a session, written
	// as its post begins: Outcome gone, watch-failing or park, with Reason when that
	// post failed.
	KindTold = "told"
	// KindLimit is an account's usage limit a session met mid-turn, found by
	// its watch or wait: Account until Due, when the limit lifts. A step that
	// meets one is a KindFailure with the same two fields.
	KindLimit = "limit"
)

// How a send ended, as its event's Outcome says.
const (
	SendArrived   = "arrived"   // the session took the step as written
	SendQueued    = "queued"    // it waits behind the turn the session runs
	SendRecovered = "recovered" // it arrived whole once retyped, its start lost the first time
	SendGarbled   = "garbled"   // it arrived without its start
	SendOther     = "other"     // the session received something else
	SendUnknown   = "unknown"   // nothing new reached the transcript in time
)

// Delivered says a send ended with its step in the session as it was
// written.
func Delivered(outcome string) bool {
	return outcome == SendArrived || outcome == SendQueued || outcome == SendRecovered
}

// Reached says a send is known to have put something in the session: its
// step, the step without its start, or something else. One left unknown is
// not known to, and no outcome is none of them.
func Reached(outcome string) bool {
	return Delivered(outcome) || outcome == SendGarbled || outcome == SendOther
}

// Unable is the accounts the batch found taking no step as of now, each to
// when it can again: a rejected login, recorded with no Due, for as long as
// the batch lasts, since nothing says when its owner logs in again; a usage
// limit until it lifts.
func Unable(evs []Event, now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, e := range evs {
		if e.Account == "" || e.Kind != KindFailure && e.Kind != KindRefused && e.Kind != KindLimit {
			continue
		}
		until, seen := out[e.Account]
		switch {
		case seen && until.IsZero():
		case e.Due.IsZero():
			out[e.Account] = time.Time{}
		case e.Due.After(until):
			out[e.Account] = e.Due
		}
	}
	for account, until := range out {
		if !until.IsZero() && !now.Before(until) {
			delete(out, account)
		}
	}
	return out
}

// ByDriver says a driver's command wrote e: a step or its intent, a prompt
// answered, an open or its intent, a park, a close, a hold freed, a wake or
// timer armed, or a refusal. A watch writes its posts, its wakes, a watch
// that ended without one and the notes of commands run after a wake, so
// none of those is a driver's.
func ByDriver(e Event) bool { return byDriver[e.Kind] }

var byDriver = map[string]bool{
	KindSendIntent: true, KindSend: true, KindRespond: true, KindPark: true,
	KindClose: true, KindWatch: true, KindRefused: true, KindOpenIntent: true, KindOpen: true,
	KindFree: true,
}

// Acted says a driver's command changed a session, or set a timer: a step
// that reached it or may have, a prompt answered, an open that started its
// Claude Code, an adoption, a park or a close. What only shows a command
// ran is ByDriver's: an intent whose action may have failed, a refusal, or a
// wake armed, which a driver's scheduled watchdog does when no run acts.
func Acted(e Event) bool {
	switch e.Kind {
	case KindSend:
		return e.Outcome != ""
	case KindOpen:
		return e.SID != ""
	case KindWatch:
		return e.Session == "" // a timer
	case KindRespond, KindAdopt, KindPark, KindClose:
		return true
	}
	return false
}

// Heard says e shows a driver is running: it read the batch, or did or
// recorded something. A wake armed proves nothing, since the watchdog arms
// them with no driver running, nor does anything a watch writes.
func Heard(e Event) bool {
	switch e.Kind {
	case KindSeen, KindSendIntent, KindSend, KindRespond, KindPark, KindClose, KindOpenIntent, KindOpen, KindAdopt,
		KindRefused, KindAsk, KindAnswer, KindRestate, KindGrant, KindGoal, KindCleanup, KindFree:
		return true
	case KindNote:
		return e.ID != "" // drive's own notes have no id
	}
	return false
}

// Event is one line of the log. Fields not used by a kind stay empty.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"` // the batch's label for it
	Block   string    `json:"block,omitempty"`
	SID     string    `json:"sid,omitempty"`
	Account string    `json:"account,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Model   string    `json:"model,omitempty"`  // on an open: the model it was asked to run on
	Effort  string    `json:"effort,omitempty"` // on an open: the effort level it was asked to run at

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
	Ref            string `json:"ref,omitempty"`    // the ask an answer answers or a restate restates, or an ask replaces; the session an open resumed
	Source         string `json:"source,omitempty"` // the owner's words, and where they were said
	Scope          string `json:"scope,omitempty"`
	Question       string `json:"question,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	IfUnanswered   string `json:"if_unanswered,omitempty"`
	Took           string `json:"took,omitempty"` // recommended, amended, delegated, moot
	Explained      bool   `json:"explained,omitempty"`
	Note           string `json:"note,omitempty"`
	Goal           string `json:"goal,omitempty"`  // on a goal, or an open-intent that sets one
	From           string `json:"from,omitempty"`  // on an open-intent: the start note the session opens from
	Brief          string `json:"brief,omitempty"` // on an open-intent: the brief the session picks up
	HTTP           int    `json:"http,omitempty"`
	Try            int    `json:"try,omitempty"`
	// Via, on a posting or wake, is the session whose wake carried this one:
	// several endings at once make one post.
	Via    string `json:"via,omitempty"`
	Branch string `json:"branch,omitempty"` // on a cleanup, the branch it judged
	Jobs   int    `json:"jobs,omitempty"`   // on a wake, the background jobs its session still ran
	// On a posting and its wake: what made it go, such as asks or job-wait. On a
	// posting alone: when it was due, and what last held it back past that. On
	// a park: when its driver should look at it again (park --until). On a
	// failure or a limit that names an account: when its usage limit lifts.
	Why    string    `json:"why,omitempty"`
	Due    time.Time `json:"due,omitzero"`
	HeldBy string    `json:"held_by,omitempty"`
}

// Member is a session that belongs to the batch.
type Member struct {
	Label   string
	Block   string
	SID     string
	Account string
	Dir     string
	// Model and Effort are the model and the effort level its latest open
	// asked for, which drive's relaunch and reopen ask for again; "" leaves
	// it to the owner's launcher and settings.
	Model  string
	Effort string
	Joined time.Time
	// Starting says its block is on record and no open has yet said Claude
	// Code started in it: the launch is under way, or its driver ended, or it
	// failed. Opening is then the request it was opened with, as recorded,
	// and Ref the conversation it was to resume, which is not yet its own:
	// a label reopened onto its own conversation keeps that SID meanwhile.
	Starting bool
	Opening  *Event
	Ref      string
	// Goal is what the session sets out to do, in plain terms for the owner:
	// the latest recorded for its label, which outlives a reopen and a
	// relaunch under that label.
	Goal     string
	Parked   string    // why, while parked; a later send unparks
	ParkedAt time.Time // so what the park says can be read against what happened since
	// ParkedUntil is when its driver should look at it again, given with the
	// park; zero when the park named no time.
	ParkedUntil time.Time
	Closed      bool
	ClosedAt    time.Time
	Resumed     bool // it carries on a session that read the notice in this batch
	// LastAction is the latest thing a driver did to it: a send, a respond, a park.
	LastAction time.Time
	LastSend   *Event
	// Noticed says a send that carried the notice reached the session:
	// arrived, queued or recovered. One left unknown or garbled may not have.
	Noticed bool
	// Wake is what the log says of its wake: the watch armed for it, the posts
	// made about it and whether a driver answered them.
	Wake WakeRecord
	// Notices are what the watchdog told the driver of it, by what they are:
	// gone, watch-failing or park. Each counts once a driver left a trace after it.
	Notices map[string]Notice

	born int // the position of the open or adoption that put it in its block
}

// NoticePark is the watchdog telling a driver of a park nothing on record
// ends, or one whose --until has come.
const NoticePark = "park"

// Notice is something the watchdog told a driver of a session.
type Notice struct {
	At    time.Time
	Heard bool
}

// heard notes a driver's trace: whatever the watchdog told it of the member
// has been heard.
func (m *Member) heard() {
	for what, n := range m.Notices {
		n.Heard = true
		m.Notices[what] = n
	}
}

// Members derives the batch's sessions, in the order they joined.
func Members(evs []Event) []*Member {
	var order []*Member
	by := map[string]*Member{}
	read := map[string]bool{} // session ids that read the notice
	goals := map[string]string{}
	opening := map[string]string{} // an open's intent, to the goal it sets once the block opens
	asked := map[string]*Event{}   // an open's intent, to the request it recorded
	for i := range evs {
		e := &evs[i]
		m := by[e.Session]
		if Heard(*e) {
			for _, x := range order {
				x.heard()
			}
		}
		switch e.Kind {
		case KindOpenIntent:
			if e.Goal != "" {
				opening[e.Intent] = e.Goal
			}
			if e.Intent != "" {
				asked[e.Intent] = e
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
			started := e.Kind == KindAdopt || e.SID != ""
			if old, ok := by[e.Session]; ok && !old.Closed && old.Block == e.Block && started && (old.Starting || old.SID == e.SID) {
				// Claude Code started in the block its open recorded: the same
				// member, now open. Said twice, as by the driver that opened it
				// and one that found it running, it is said once.
				old.SID, old.Account, old.Dir = cmp.Or(e.SID, old.SID), cmp.Or(e.Account, old.Account), cmp.Or(e.Dir, old.Dir)
				old.Model, old.Effort = cmp.Or(e.Model, old.Model), cmp.Or(e.Effort, old.Effort)
				old.Starting, old.Opening = false, nil
				continue
			}
			m = &Member{Label: e.Session, Block: e.Block, SID: e.SID, Account: e.Account, Dir: e.Dir, Model: e.Model, Effort: e.Effort, Joined: e.At, Resumed: read[e.Ref], born: i + 1,
				Starting: !started, Ref: e.Ref}
			if m.Starting && e.Intent != "" {
				m.Opening = asked[e.Intent]
			}
			if old, ok := by[e.Session]; ok {
				if e.Ref != "" && e.Ref == old.SID {
					// Its own conversation resumed in a new block, as after Rex
					// restarted: what its driver did with it stands, its park
					// among it. Its wake is the new block's.
					m.SID = cmp.Or(m.SID, old.SID)
					m.Parked, m.ParkedAt, m.ParkedUntil, m.LastAction, m.LastSend, m.Noticed = old.Parked, old.ParkedAt, old.ParkedUntil, old.LastAction, old.LastSend, old.Noticed
				}
				*old = *m // reopened under the same label
				m = old
			} else {
				order = append(order, m)
			}
			by[e.Session] = m
		case KindSend:
			if m != nil && e.Outcome != "" {
				m.LastAction, m.LastSend, m.Parked, m.ParkedUntil = e.At, e, "", time.Time{}
				delete(m.Notices, NoticePark)
				if e.Notice && Delivered(e.Outcome) {
					m.Noticed, read[m.SID] = true, true
				}
			}
		case KindRespond:
			if m != nil {
				m.LastAction = e.At
			}
		case KindPark:
			if m != nil {
				m.Parked, m.ParkedAt, m.ParkedUntil, m.LastAction = e.Reason, e.At, e.Due, e.At
				// A new park is told of afresh, its own time with it.
				delete(m.Notices, NoticePark)
			}
		case KindClose:
			if m != nil {
				m.Closed, m.ClosedAt = true, e.At
			}
		case KindWatch:
			if m != nil {
				m.Wake.armed, m.Wake.armedAt = e.At, i+1
			}
		case KindWakeStop:
			if m != nil {
				m.Wake.stopped = e.At
			}
		case KindTold:
			if m != nil && e.Outcome != NoticeHoldFree { // that one is its waiter's (Waiting)
				if m.Notices == nil {
					m.Notices = map[string]Notice{}
				}
				if e.Reason != "" {
					delete(m.Notices, e.Outcome) // the post failed, so it told no one
				} else {
					m.Notices[e.Outcome] = Notice{At: e.At}
				}
			}
		case KindFailure:
			if m != nil && IsWatchEnded(*e) {
				m.Wake.ended = e.At
			}
		}
	}
	// A post is its member's while the member runs in the block the post was
	// about: one begun before a label was reopened is the old block's.
	posts := Posts(evs)
	for i := range posts {
		p := &posts[i]
		if m := by[p.Session]; m != nil && p.began > m.born && (p.Block == "" || m.Block == "" || p.Block == m.Block) {
			m.Wake.add(p)
		}
	}
	for _, m := range order {
		m.Goal = goals[m.Label]
		m.Wake.acted = m.LastAction
	}
	return order
}

// Hearing is whether a batch's driver is answering its wakes, read from the
// log: a run a webhook started leaves a trace within minutes, so wakes posted
// and followed by none were not heard, as when the bot's usage is out and the
// webhook still answers 2xx. Posts that failed count alike. A wake with no Why
// was posted by an older drive, whose runs left no trace of a read, so only its
// failure says anything.
type Hearing struct {
	Strikes int       // wakes in a row that failed or went unheard
	Since   time.Time // when the first of them went out
	Last    time.Time // when the newest wake went out, one too recent to count yet included
	Heard   time.Time // when a driver last left a trace
}

// HeardWithin is how long a posted wake has to show its driver running
// before it counts as unheard.
const HeardWithin = 10 * time.Minute

// Hearings reads how a batch's wakes are being heard as of now.
func Hearings(evs []Event, now time.Time) Hearing {
	var h Hearing
	posts := Posts(evs)
	result := map[int]*Post{} // a wake event's position, to the post it ended
	for i := range posts {
		if p := &posts[i]; !p.Pending() {
			result[p.ended] = p
		}
	}
	for i, e := range evs {
		switch {
		case Heard(e):
			h.Heard, h.Strikes, h.Since, h.Last = e.At, 0, time.Time{}, time.Time{}
		case e.Kind == KindWake && e.Via == "":
			p := result[i+1]
			if p != nil && p.recorded && !h.Heard.IsZero() && !h.Heard.Before(p.Began) && e.HTTP != 0 {
				continue // a run answered before its post returned
			}
			h.Last = e.At
			if e.HTTP != 0 && e.Why == "" {
				continue // an older drive posted it, and its runs left no trace of a read
			}
			if e.HTTP == 0 || now.Sub(e.At) >= HeardWithin {
				if h.Strikes == 0 {
					h.Since = e.At
				}
				h.Strikes++
			}
		}
	}
	return h
}

// Cleanup is what the log last says of a branch a closed session left.
type Cleanup struct {
	Session, Branch, Outcome, Reason string
	At                               time.Time
}

// Cleanups lists, for each session and branch, the newest cleanup recorded,
// in the order first judged. A session with an outcome of none or removed has
// nothing left of that branch.
func Cleanups(evs []Event) []Cleanup {
	var out []Cleanup
	at := map[[2]string]int{}
	for _, e := range evs {
		if e.Kind != KindCleanup {
			continue
		}
		c := Cleanup{Session: e.Session, Branch: e.Branch, Outcome: e.Outcome, Reason: e.Reason, At: e.At}
		key := [2]string{e.Session, e.Branch}
		if i, ok := at[key]; ok {
			out[i] = c
			continue
		}
		at[key] = len(out)
		out = append(out, c)
	}
	return out
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
		if refers(ref, m.Block) {
			return m
		}
	}
	return nil
}

// refers says ref names block: its id, or its last six characters or more.
func refers(ref, block string) bool {
	return block == ref || len(ref) >= 6 && strings.HasSuffix(block, ref)
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

// Hold is a taking of a resource: a session holds it from the moment its send
// is admitted until the turn that send started has ended.
type Hold struct {
	Name    string
	Session string
	Since   time.Time // when the send that took it was admitted
	Intent  string
	Pending bool // that send has no outcome yet
}

// Waiter is a session a send or an open was refused for because another held
// what it needed: it waits for that hold from then until a step reaches it or
// it closes.
type Waiter struct {
	Hold    string    // the hold it waits for
	Session string    // its label; its block's short id until it joins the batch; the label an open was refused under
	Since   time.Time // when it began to wait
	Step    string    // the step it was refused
	Key     string    // that step's snippet, when it was one
	Path    string    // that step's file, when it was one
	Open    bool      // the refused action was an open: Step is the drive open to run
	// Told is when the watchdog last told the driver the hold is free for it,
	// and Heard whether a driver left a trace since. The telling stands while
	// the session waits on as it did: a refusal on the hold again, or anyone
	// taking the hold, has it wait anew, and parking it again does not.
	Told  time.Time
	Heard bool
}

// NoticeHoldFree is the watchdog telling a driver that the hold a session
// waits for is free: a told event with the waiter as Session and the hold.
const NoticeHoldFree = "hold-free"

// Holds lists the newest taking of each hold. A send that failed before it
// reached its session took nothing, and a hold a driver freed is no one's
// until a later send takes it. Whether a taking still holds depends on the
// holder's turn, which only the session can say.
func Holds(evs []Event) []Hold {
	done, sent := resolved(evs)
	takings := map[string][]Hold{}
	var names []string
	for _, e := range evs {
		switch {
		case e.Kind == KindSendIntent && e.Hold != "":
			if _, ok := takings[e.Hold]; !ok && !slices.Contains(names, e.Hold) {
				names = append(names, e.Hold)
			}
			takings[e.Hold] = append(takings[e.Hold], Hold{Name: e.Hold, Session: e.Session, Since: e.At, Intent: e.Intent, Pending: !done[e.Intent]})
		case e.Kind == KindFree:
			delete(takings, e.Hold)
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

// Waiting lists the sessions a send or an open was refused for because
// another held what it needed, in the order each began to wait. A session
// waits until a step reaches it, which also ends a park behind the hold, or
// until it closes. One refused by its block id, before it joined the batch,
// is the same waiter under the label it joins as: the refusal names the
// block, and every later event the label.
func Waiting(evs []Event) []Waiter {
	var out []Waiter
	labels := map[string]bool{} // the batch's labels so far
	blocks := map[string]bool{} // the waiters a refusal named by a block id
	at := func(session string) int {
		return slices.IndexFunc(out, func(w Waiter) bool { return w.Session == session })
	}
	for _, e := range evs {
		if Heard(e) {
			for i := range out {
				out[i].Heard = !out[i].Told.IsZero()
			}
		}
		switch e.Kind {
		case KindTold:
			if i := at(e.Session); e.Outcome == NoticeHoldFree && i >= 0 && out[i].Hold == e.Hold {
				out[i].Told, out[i].Heard = e.At, false
				if e.Reason != "" {
					out[i].Told = time.Time{} // the post failed, so it told no one
				}
			}
		case KindSendIntent:
			// The hold is taken, so whoever still waits for it waits anew.
			for i := range out {
				if e.Hold != "" && out[i].Hold == e.Hold {
					out[i].Told, out[i].Heard = time.Time{}, false
				}
			}
		case KindOpen, KindAdopt:
			labels[e.Session] = true
			for i, w := range out {
				if !blocks[w.Session] || !refers(w.Session, e.Block) {
					continue
				}
				delete(blocks, w.Session)
				if at(e.Session) < 0 {
					out[i].Session = e.Session
				} else {
					out = slices.Delete(out, i, i+1) // the label waits already, as for an open refused
				}
				break
			}
		case KindRefused:
			if e.Hold == "" {
				continue
			}
			w := Waiter{Hold: e.Hold, Session: e.Session, Since: e.At, Step: e.Text, Key: e.Key, Path: e.Path, Open: e.Mode == "open"}
			switch i := at(e.Session); {
			case i < 0:
				out = append(out, w)
				blocks[e.Session] = !w.Open && !labels[e.Session]
			case out[i].Hold != e.Hold:
				out[i] = w
			default:
				out[i].Told, out[i].Heard = time.Time{}, false // refused on it again: it was not free after all
			}
		case KindSend, KindClose:
			if i := at(e.Session); i >= 0 && (e.Kind == KindClose || e.Outcome != "") {
				out = slices.Delete(out, i, i+1)
			}
		}
	}
	return out
}

// Ask is a question put to the owner, with its answer once given. Its
// IfUnanswered is what it waits on as it stands: the latest restatement's.
type Ask struct {
	Event
	Answer   *Event
	Restated *Event // the latest restatement, nil while it stands as asked
}

// Stood is when the ask last stood as written: asked, or restated. What
// happened to its session after that is what may have overtaken it.
func (a *Ask) Stood() time.Time {
	if a.Restated != nil {
		return a.Restated.At
	}
	return a.At
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
		case KindRestate:
			if a := byID[e.Ref]; a != nil {
				r := e
				a.Restated, a.IfUnanswered = &r, e.IfUnanswered
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
