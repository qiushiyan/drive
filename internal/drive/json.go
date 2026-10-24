package drive

import (
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
)

// The documents --json prints, one per command: keys in snake_case, times
// in RFC 3339, durations in whole seconds, a list as [] when it is empty,
// and a field that does not apply to a row left out. They are the command
// line's own shapes, apart from the types drive keeps on disk, so either
// can change without the other; each help page with --json names every key.

type statusDoc struct {
	Batch    *batchDoc     `json:"batch"` // null when there is no current batch
	Sessions []rowDoc      `json:"sessions"`
	Closed   int           `json:"closed"`
	Others   []rowDoc      `json:"others"`
	Holds    []string      `json:"holds"`
	OpenAsks []askDoc      `json:"open_asks"`
	Grants   []grantDoc    `json:"grants"`
	InFlight []inFlightDoc `json:"in_flight"`
}

type batchDoc struct {
	Name       string    `json:"name"`
	Started    time.Time `json:"started"`
	Wake       string    `json:"wake"`
	Webhook    string    `json:"webhook,omitempty"`
	RexSession string    `json:"rex_session,omitempty"`
	TZ         string    `json:"tz,omitempty"`
}

type rowDoc struct {
	Label         string          `json:"label"`
	SID           string          `json:"sid"`
	Block         string          `json:"block"`
	Window        string          `json:"window"`
	Name          string          `json:"name,omitempty"`
	State         string          `json:"state"`
	ForSeconds    int             `json:"for_seconds"`
	Account       string          `json:"account"`
	Input         string          `json:"input"`
	InputText     string          `json:"input_text,omitempty"`
	Stage         string          `json:"stage"`
	PR            *prDoc          `json:"pr,omitempty"`
	LastSent      string          `json:"last_sent"`
	Watch         string          `json:"watch"`
	UnhandledWake bool            `json:"unhandled_wake"`
	WakeDue       string          `json:"wake_due,omitempty"`
	View          string          `json:"view,omitempty"`
	Error         string          `json:"error,omitempty"`
	Jobs          []session.Shell `json:"jobs"`
}

type prDoc struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Merge   string `json:"merge,omitempty"`
	Review  string `json:"review,omitempty"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Pending int    `json:"pending"`
}

type askDoc struct {
	ID             string    `json:"id"`
	Session        string    `json:"session,omitempty"`
	At             time.Time `json:"at"`
	Question       string    `json:"question"`
	Recommendation string    `json:"recommendation,omitempty"`
	IfUnanswered   string    `json:"if_unanswered"`
}

type grantDoc struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Scope  string    `json:"scope"`
	Source string    `json:"source"`
}

type inFlightDoc struct {
	Kind      string    `json:"kind"`
	Session   string    `json:"session"`
	At        time.Time `json:"at"`
	Text      string    `json:"text"`
	DriverPID int       `json:"driver_pid,omitempty"` // left out when that driver has gone
}

// JSON is what status --json prints.
func (st *Status) JSON() any {
	rows := func(rs []Row) []rowDoc {
		out := []rowDoc{}
		for _, r := range rs {
			d := rowDoc{Label: r.Label, SID: r.SID, Block: rex.Short(r.Block), Window: r.Window, Name: r.Name, State: r.State,
				ForSeconds: int(r.For.Seconds()), Account: r.Account, Input: string(r.Input), Stage: r.Stage, LastSent: r.LastSent,
				Watch: r.Watch, UnhandledWake: r.Unhandled, WakeDue: r.Due, View: viewOrEmpty(r.View), Error: r.Error, Jobs: jobs(r.Jobs)}
			if r.Input == "typed" || r.Input == "suggestion" {
				d.InputText = r.Typed
			}
			if p := r.PR; p != nil {
				d.PR = &prDoc{Number: p.Number, State: p.State, Draft: p.Draft, Merge: p.Merge, Review: p.Review, Passed: p.Passed, Failed: p.Failed, Pending: p.Pending}
			}
			out = append(out, d)
		}
		return out
	}
	doc := statusDoc{Sessions: rows(st.Rows), Closed: st.Closed, Others: rows(st.Others), Holds: list(st.Holds),
		OpenAsks: []askDoc{}, Grants: []grantDoc{}, InFlight: []inFlightDoc{}}
	if m := st.Batch; m != nil {
		doc.Batch = &batchDoc{Name: m.Name, Started: m.Started, Wake: m.Wake, Webhook: m.Webhook, RexSession: m.RexSession, TZ: m.TZ}
	}
	for _, q := range st.Asks {
		doc.OpenAsks = append(doc.OpenAsks, askDoc{ID: q.ID, Session: q.Session, At: q.At, Question: q.Question, Recommendation: q.Recommendation, IfUnanswered: q.IfUnanswered})
	}
	for _, g := range st.Grants {
		doc.Grants = append(doc.Grants, grantDoc{ID: g.ID, At: g.At, Scope: g.Scope, Source: g.Source})
	}
	for _, p := range st.InFlight {
		f := inFlightDoc{Kind: p.Kind, Session: p.Session, At: p.At, Text: p.Text}
		if p.Driver != nil {
			f.DriverPID = p.Driver.PID
		}
		doc.InFlight = append(doc.InFlight, f)
	}
	return doc
}

type readDoc struct {
	Session   string          `json:"session"`
	SID       string          `json:"sid"`
	State     string          `json:"state"`
	Account   string          `json:"account"`
	Input     string          `json:"input"`
	InputText string          `json:"input_text,omitempty"`
	Text      string          `json:"text"`
	At        *time.Time      `json:"at,omitempty"`
	View      string          `json:"view,omitempty"`
	Next      string          `json:"next,omitempty"`
	Jobs      []session.Shell `json:"jobs"`
}

// JSON is what read --json prints.
func (r *Reading) JSON() any {
	d := readDoc{Session: r.Label, SID: r.SID, State: r.State, Account: r.Account, Input: string(r.Input), Text: r.Body,
		View: viewOrEmpty(r.View), Next: r.Next, Jobs: jobs(r.Jobs)}
	if r.Input == "typed" || r.Input == "suggestion" {
		d.InputText = r.Typed
	}
	if !r.At.IsZero() {
		d.At = &r.At
	}
	return d
}

type endingDoc struct {
	Session string          `json:"session"`
	Block   string          `json:"block"`
	State   string          `json:"state"`
	Error   *apiErrorDoc    `json:"error,omitempty"`
	Shells  int             `json:"shells"`
	Orphans int             `json:"orphans"`
	Jobs    []session.Shell `json:"jobs"`
	Reply   string          `json:"reply,omitempty"`
	Screen  string          `json:"screen,omitempty"`
	View    string          `json:"view,omitempty"`
	Next    string          `json:"next,omitempty"`
}

type apiErrorDoc struct {
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// JSON is what wait --json prints.
func (e *Ending) JSON() any {
	d := endingDoc{Session: e.Label, Block: rex.Short(e.Block), State: e.State, Shells: e.Shells, Orphans: e.Orphans,
		Jobs: jobs(e.Jobs), Reply: e.Reply, Screen: e.Screen, View: viewOrEmpty(e.View), Next: e.Next}
	if e.Error != nil {
		d.Error = &apiErrorDoc{Kind: e.Error.Kind, Text: e.Error.Text, At: e.Error.At}
	}
	return d
}

type metricsDoc struct {
	Batch               string         `json:"batch"`
	From                time.Time      `json:"from"`
	To                  time.Time      `json:"to"`
	Sessions            int            `json:"sessions"`
	Asks                int            `json:"asks"`
	Answered            int            `json:"answered"`
	Unanswered          int            `json:"unanswered"`
	Took                map[string]int `json:"took"`
	Explained           int            `json:"explained"`
	OwnerWaitSeconds    int            `json:"owner_wait_seconds"`
	MedianAnswerSeconds int            `json:"median_answer_seconds"`
	Merged              []mergedDoc    `json:"merged"`
	PerMergeSeconds     int            `json:"per_merge_seconds"`
	Unread              int            `json:"unread"`
	Sends               int            `json:"sends"`
	Snippets            int            `json:"snippets"`
	ByKey               map[string]int `json:"by_key"`
	Commands            int            `json:"commands"`
	OwnWords            int            `json:"own_words"`
	Recovered           int            `json:"recovered"`
	Garbled             int            `json:"garbled"`
	Other               int            `json:"other"`
	Unknown             int            `json:"unknown"`
	Refusals            map[string]int `json:"refusals"`
	Failures            []string       `json:"failures"`
	Wakes               int            `json:"wakes"`
	Timers              int            `json:"timers"`
	WakeFails           int            `json:"wake_fails"`
	Unhandled           int            `json:"unhandled"`
	Unwatched           int            `json:"unwatched"`
}

type mergedDoc struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number,omitempty"` // left out for a change merged without a pull request
	Branch   string    `json:"branch,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	At       time.Time `json:"at"`
	Sessions []string  `json:"sessions"`
}

// JSON is what metrics --json prints.
func (m *Metrics) JSON() any {
	seconds := func(d time.Duration) int { return int(d.Seconds()) }
	counts := func(c map[string]int) map[string]int {
		if c == nil {
			return map[string]int{}
		}
		return c
	}
	d := metricsDoc{Batch: m.Batch, From: m.From, To: m.To, Sessions: m.Sessions, Asks: m.Asks, Answered: m.Answered,
		Unanswered: m.Unanswered, Took: counts(m.Took), Explained: m.Explained, OwnerWaitSeconds: seconds(m.OwnerWait),
		MedianAnswerSeconds: seconds(m.MedianAnswer), Merged: []mergedDoc{}, PerMergeSeconds: seconds(m.PerMerge), Unread: m.Unread,
		Sends: m.Sends, Snippets: m.Snippets, ByKey: counts(m.ByKey), Commands: m.Commands, OwnWords: m.OwnWords,
		Recovered: m.Recovered, Garbled: m.Garbled, Other: m.Other, Unknown: m.Unknown, Refusals: counts(m.Refusals),
		Failures: list(m.Failures), Wakes: m.Wakes, Timers: m.Timers, WakeFails: m.WakeFails, Unhandled: m.Unhandled, Unwatched: m.Unwatched}
	for _, p := range m.Merged {
		d.Merged = append(d.Merged, mergedDoc{Repo: p.Repo, Number: p.Number, Branch: p.Branch, Commit: p.Commit, At: p.At, Sessions: list(p.Sessions)})
	}
	return d
}

// LogJSON is what log --json prints: the events as the batch recorded them.
func LogJSON(evs []batch.Event) any { return list(evs) }

func jobs(js []session.Shell) []session.Shell { return list(js) }

// list is a slice as JSON gives it: [] when empty, never null.
func list[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
