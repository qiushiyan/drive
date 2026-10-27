package drive

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/skill"
	"github.com/qiushiyan/drive/internal/turn"
)

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
	Other     int // lines the session received as something other than what was typed
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
// its builder, and its strategic reviewer when one merged it. A change
// merged without a pull request, as a personal tool's closeout merges it, is
// its branch and its newest commit, with Number 0 and Repo the repository's
// main checkout.
type MergedPR struct {
	Repo     string
	Number   int
	Branch   string `json:",omitempty"`
	Commit   string `json:",omitempty"`
	At       time.Time
	Sessions []string
}

// Metrics computes the measures for the batch the driver acts in.
func (d *Driver) Metrics() (*Metrics, error) {
	b, err := d.Batch()
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
	m.asks(evs)
	m.actions(evs, b.Meta.Wake == "webhook")
	d.merges(m, b, evs)
	if len(m.Merged) > 0 {
		m.PerMerge = m.OwnerWait / time.Duration(len(m.Merged))
	}
	return m, nil
}

// asks measures the owner's answers: how many, how they went, and how long
// the owner was waited on, an open ask counted to the batch's last event.
func (m *Metrics) asks(evs []batch.Event) {
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
		slices.Sort(waits)
		m.MedianAnswer = waits[len(waits)/2]
	}
}

// actions measures what drivers did from the log: sends by kind and
// delivery, refusals, failures, wakes and the ones no action followed, and,
// in a batch that wakes its driver, the turns left unwatched.
func (m *Metrics) actions(evs []batch.Event, wakes bool) {
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
			case "other":
				m.Other++
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
			if e.HTTP == 0 {
				m.WakeFails++ // the driver never heard: the turn is still unwatched
				continue
			}
			unwatched[e.Session] = 0
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
	if wakes {
		for _, n := range unwatched {
			m.Unwatched += n
		}
	}
}

// merges finds the changes the batch's sessions merged: every pull request
// a session linked that merged since the batch started, counted once
// however many sessions name it, else the session's work once it is in the
// trunk.
func (d *Driver) merges(m *Metrics, b *batch.Batch, evs []batch.Event) {
	merged := map[string]int{} // repo#number, or repo@branch, to its index in m.Merged
	add := func(key, label string, c MergedPR) {
		if i, ok := merged[key]; ok {
			m.Merged[i].Sessions = append(m.Merged[i].Sessions, label)
			return
		}
		c.Sessions = []string{label}
		m.Merged = append(m.Merged, c)
		merged[key] = len(m.Merged) - 1
	}
	members := batch.Members(evs)
	steps := make([]*progress.Steps, len(members)) // nil where a session's steps could not be read
	var refs []progress.PRRef
	for i, mem := range members {
		if d.Progress == nil || mem.SID == "" {
			continue
		}
		if s, err := d.Progress.Steps(mem.SID); err == nil {
			steps[i] = &s
			refs = append(refs, s.PRs...)
		}
	}
	states := d.prStates(refs)
	for i, mem := range members {
		m.Sessions++
		if d.Progress == nil || mem.SID == "" {
			continue
		}
		viaPR, unread := false, steps[i] == nil
		if s := steps[i]; s != nil {
			for _, ref := range s.PRs {
				key := fmt.Sprintf("%s#%d", ref.Repo, ref.Number)
				if _, ok := merged[key]; ok {
					add(key, mem.Label, MergedPR{})
					viaPR = true
					continue
				}
				st := states[keyOf(ref)]
				if st.err != nil {
					unread = true
					continue
				}
				if pr := st.pr; pr.State == "MERGED" && !pr.MergedAt.Before(b.Meta.Started) {
					add(key, mem.Label, MergedPR{Repo: ref.Repo, Number: pr.Number, At: pr.MergedAt})
					viaPR = true
				}
			}
		}
		if !viaPR {
			if l, ok := d.landed(mem.Dir, mem.SID, d.branchesOf(mem.SID)); ok && l.merged {
				key := l.repo + "@" + l.commit
				if l.commit == "" {
					key = l.repo + "@" + l.branch
				}
				add(key, mem.Label, MergedPR{Repo: l.repo, Branch: l.branch, Commit: l.commit, At: m.To})
			}
		}
		if unread {
			m.Unread++
		}
	}
}

// branchesOf are the branches a session's transcript names, which outlives
// its block.
func (d *Driver) branchesOf(sid string) []string {
	if path := d.Host.Transcript(sid); path != "" {
		if f, err := turn.ReadFile(path); err == nil {
			return f.Branches
		}
	}
	return nil
}

// workLanding is how a session's work stands against its trunk.
type workLanding struct {
	branch, commit, repo string
	merged               bool
}

// landed judges the work a session did: its newest commit, which outlives
// the branch a closeout removes, else the branch it worked on, the newest
// one other than the trunk it may have gone back to for the merge, among
// those its transcript names and those checked out where it committed (a
// session that worked in a worktree through its shell names only the
// trunk). ok is false when neither has a verdict.
func (d *Driver) landed(dir, sid string, branches []string) (workLanding, bool) {
	if d.Progress == nil || dir == "" {
		return workLanding{}, false
	}
	var commits []progress.Commit
	if s, err := d.Progress.Steps(sid); err == nil {
		commits = s.Commits
	}
	l, err := d.Progress.Landed(dir, branches, commits)
	if err != nil {
		return workLanding{}, false
	}
	w := workLanding{commit: l.Commit, repo: l.Repo}
	for _, b := range slices.Backward(l.Branches) {
		if b != l.Trunk {
			w.branch = b
			break
		}
	}
	if merged, judged := l.Merged[l.Commit]; l.Commit != "" && judged {
		w.merged = merged
		return w, true
	}
	merged, judged := l.Merged[w.branch]
	w.merged = merged
	return w, w.branch != "" && judged
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
	case strings.Contains(reason, "still runs a background shell"), strings.Contains(reason, "still runs a background job"):
		return "background job running"
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
