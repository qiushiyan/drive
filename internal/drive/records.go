package drive

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/skill"
)

// Grant records authority the owner handed over, in their words and with
// what it covers. It lives as long as the batch: a new batch starts with none.
func (d *Driver) Grant(source, scope string) (string, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(scope) == "" {
		return "", &Refused{Reason: "a grant needs --source (the owner's words and where they said them) and --scope (what it covers)"}
	}
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindGrant, ID: batch.NextID(evs, batch.KindGrant, "G"), Source: source, Scope: scope}
	})
}

// Ask records a question put to the owner.
func (d *Driver) Ask(session, question, recommendation, ifUnanswered string) (string, error) {
	if question == "" || ifUnanswered == "" {
		return "", &Refused{Reason: "an ask needs --question and --if-unanswered (what happens while nobody answers)"}
	}
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindAsk, ID: batch.NextID(evs, batch.KindAsk, "A"), Session: session,
			Question: question, Recommendation: recommendation, IfUnanswered: ifUnanswered}
	})
}

var tookWords = map[string]bool{"recommended": true, "amended": true, "delegated": true, "moot": true}

// Answer records the owner's answer to an ask: whether they took the
// recommendation, amended it, handed it to someone (a fresh model), or it
// stopped mattering; and separately whether they needed it explained first.
func (d *Driver) Answer(ask, source, took string, explained bool) (string, error) {
	if !tookWords[took] {
		return "", &Refused{Reason: fmt.Sprintf("--took %q: one of recommended, amended, delegated, moot", took)}
	}
	b, err := d.Batch()
	if err != nil {
		return "", &Refused{Reason: err.Error()}
	}
	err = b.Locked(func(evs []batch.Event) error {
		for _, e := range evs {
			if e.Kind == batch.KindAsk && e.ID == ask {
				return b.Append(batch.Event{At: d.now(), Kind: batch.KindAnswer, Ref: ask, Source: source, Took: took, Explained: explained})
			}
		}
		return &Refused{Reason: fmt.Sprintf("no ask %s in batch %s", ask, b.Meta.Name), Next: "drive status lists the open asks"}
	})
	return ask, err
}

// Note records something the driver wants the batch to remember.
func (d *Driver) Note(session, text string) (string, error) {
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindNote, ID: batch.NextID(evs, batch.KindNote, "N"), Session: session, Note: text}
	})
}

func (d *Driver) record(make func([]batch.Event) batch.Event) (string, error) {
	b, err := d.Batch()
	if err != nil {
		return "", &Refused{Reason: err.Error()}
	}
	var id string
	err = b.Locked(func(evs []batch.Event) error {
		e := make(evs)
		e.At = d.now()
		id = e.ID
		return b.Append(e)
	})
	return id, err
}

// Log is the batch's events since a time.
func (d *Driver) Log(since time.Time) (*batch.Batch, []batch.Event, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, nil, &Refused{Reason: err.Error()}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, nil, err
	}
	var out []batch.Event
	for _, e := range evs {
		if !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return b, out, nil
}

// Metrics is how a batch went, in the measures the steward comparison uses.
type Metrics struct {
	Batch string
	Zone  *time.Location `json:"-"`
	From  time.Time
	To    time.Time // the batch's last event: what is still open is measured to it

	Asks         int
	Answered     int
	Took         map[string]int // recommended, amended, delegated, moot
	Explained    int            // answers the owner needed explained first
	Unanswered   int
	OwnerWait    time.Duration // summed from each ask to its answer, or to the batch's last event while open
	MedianAnswer time.Duration

	Sessions int
	Merged   []MergedPR    // every pull request a session named that merged since the batch started
	Unread   int           // sessions whose progress could not be read: Merged may be short
	PerMerge time.Duration // OwnerWait over merged changes

	Sends     int
	Snippets  int            // sends that were one of the owner's snippets
	ByKey     map[string]int // which snippets
	Commands  int            // slash commands the driver typed, such as /compact
	OwnWords  int            // steps the driver wrote: a typed line that is no command, or a file
	Refusals  map[string]int // by kind of reason
	Failures  []string
	Unknown   int // sends whose delivery is unknown
	Recovered int // lines retyped after their start was lost
	Garbled   int // lines that arrived without their start and stayed so
	Wakes     int // a session's wakes posted
	Timers    int // a timer's wakes posted
	WakeFails int // wakes, a session's or a timer's, whose post failed
	// Unhandled counts wakes posted that no driver action on their session
	// followed before its next wake or the batch's end: a send, respond,
	// park, close, re-armed watch, or a refusal of one.
	Unhandled int
	// Unwatched counts, in a batch that wakes its driver, the sends and
	// responds that reached a session and that no wake, park or close of it
	// followed before the batch's end: turns whose end woke no one, the lost
	// wake itself.
	Unwatched int
}

// driverActions are what a driver does to a session; any of them after a
// wake shows the wake reached a driver.
var driverActions = map[string]bool{
	batch.KindSendIntent: true, batch.KindSend: true, batch.KindRespond: true, batch.KindPark: true,
	batch.KindClose: true, batch.KindWatch: true, batch.KindRefused: true, batch.KindOpenIntent: true, batch.KindOpen: true,
}

// MergedPR is a change the batch merged, with the sessions that name it:
// its builder, and its strategic reviewer when one merged it.
type MergedPR struct {
	Repo     string
	Number   int
	At       time.Time
	Sessions []string
}

// Metrics computes the measures for a batch ("" is the current one).
func (d *Driver) Metrics(name string) (*Metrics, error) {
	b, err := d.Batch()
	if name != "" {
		b, err = d.Store.Get(name)
	}
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	m := &Metrics{Batch: b.Meta.Name, Zone: b.Zone(), From: b.Meta.Started, To: b.Meta.Started, Took: map[string]int{}, ByKey: map[string]int{}, Refusals: map[string]int{}}
	for _, e := range evs {
		if e.At.After(m.To) {
			m.To = e.At
		}
	}
	_, asks, _ := batch.Records(evs)
	var waits []time.Duration
	for _, a := range asks {
		m.Asks++
		if a.Answer == nil {
			m.Unanswered++
			m.OwnerWait += m.To.Sub(a.At)
			continue
		}
		m.Answered++
		m.Took[a.Answer.Took]++
		if a.Answer.Explained {
			m.Explained++
		}
		w := a.Answer.At.Sub(a.At)
		m.OwnerWait += w
		waits = append(waits, w)
	}
	if len(waits) > 0 {
		sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
		m.MedianAnswer = waits[len(waits)/2]
	}
	woken := map[string]bool{}    // a session's posted wake that no driver action has followed yet
	unwatched := map[string]int{} // a session's steps that no wake, park or close has followed yet
	for _, e := range evs {
		if e.Session != "" && driverActions[e.Kind] {
			woken[e.Session] = false
		}
		switch e.Kind {
		case batch.KindSend:
			if e.Mode == "notice" {
				continue // the notice ahead of a typed first step
			}
			m.Sends++
			switch {
			case e.Key != "":
				m.Snippets++
				m.ByKey[e.Key]++
			case e.Mode == "typed" && skill.Command(e.Text) != "":
				m.Commands++
			default:
				m.OwnWords++
			}
			switch e.Outcome {
			case "recovered":
				m.Recovered++
			case "garbled":
				m.Garbled++
			case "unknown":
				m.Unknown++
			}
			if e.Outcome != "unknown" {
				unwatched[e.Session]++
			}
		case batch.KindRespond:
			if e.Action != "clear" { // clearing the input box starts no turn
				unwatched[e.Session]++
			}
		case batch.KindPark, batch.KindClose:
			unwatched[e.Session] = 0
		case batch.KindRefused:
			m.Refusals[refusalKind(e.Reason)]++
		case batch.KindFailure:
			m.Failures = append(m.Failures, e.Session+": "+clip(e.Reason, 120))
		case batch.KindWake:
			if e.Session == "" { // a timer
				if e.HTTP == 0 {
					m.WakeFails++
				} else {
					m.Timers++
				}
				continue
			}
			unwatched[e.Session] = 0 // its watch saw the turn end, whether or not the post went
			if e.HTTP == 0 {
				m.WakeFails++
				continue
			}
			m.Wakes++
			if woken[e.Session] {
				m.Unhandled++
			}
			woken[e.Session] = true
		}
	}
	for _, w := range woken {
		if w {
			m.Unhandled++ // its last wake, with nothing after it
		}
	}
	if b.Meta.Wake == "webhook" {
		for _, n := range unwatched {
			m.Unwatched += n
		}
	}
	merged := map[string]int{}   // repo#number to its index in m.Merged
	looked := map[string]error{} // repo#number to how its lookup went: each is looked up once
	for _, mem := range batch.Members(evs) {
		m.Sessions++
		if d.Progress == nil || mem.SID == "" {
			continue
		}
		s, err := d.Progress.Steps(mem.SID)
		if err != nil {
			m.Unread++
			continue
		}
		unread := false
		for _, ref := range s.PRs {
			key := fmt.Sprintf("%s#%d", ref.Repo, ref.Number)
			if i, ok := merged[key]; ok {
				m.Merged[i].Sessions = append(m.Merged[i].Sessions, mem.Label)
				continue
			}
			if err, ok := looked[key]; ok {
				unread = unread || err != nil
				continue
			}
			pr, err := d.Progress.PR(ref)
			if looked[key] = err; err != nil {
				unread = true
				continue
			}
			if pr.State == "MERGED" && !pr.MergedAt.Before(b.Meta.Started) {
				m.Merged = append(m.Merged, MergedPR{Repo: ref.Repo, Number: pr.Number, At: pr.MergedAt, Sessions: []string{mem.Label}})
				merged[key] = len(m.Merged) - 1
			}
		}
		if unread {
			m.Unread++
		}
	}
	if len(m.Merged) > 0 {
		m.PerMerge = m.OwnerWait / time.Duration(len(m.Merged))
	}
	return m, nil
}

// refusalKind groups refusal reasons by what stopped the action.
func refusalKind(reason string) string {
	switch {
	case strings.Contains(reason, "moved past view"):
		return "stale view"
	case strings.Contains(reason, "another driver"):
		return "concurrent driver"
	case strings.Contains(reason, "is held by"):
		return "held resource"
	case strings.Contains(reason, "login was rejected"):
		return "login rejected"
	case strings.Contains(reason, " waits on a ") && strings.Contains(reason, " prompt"):
		return "waiting on a prompt"
	case strings.Contains(reason, "has a prompt it has not answered"):
		return "step not yet answered"
	case strings.Contains(reason, "still runs a background shell"):
		return "background shell running"
	case strings.Contains(reason, "is working"):
		return "still working"
	case strings.Contains(reason, "already open"):
		return "already open"
	case strings.Contains(reason, "starts only when typed"):
		return "typed-only skill in a file"
	case strings.Contains(reason, "typed text"), strings.Contains(reason, "unsent line"):
		return "typed input present"
	default:
		return "other"
	}
}
