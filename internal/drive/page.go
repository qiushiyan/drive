package drive

import (
	"bytes"
	"cmp"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
)

// What the page prints, for the owner on a phone: the words are theirs, not
// a driver's. Every fact is in the view below, worded in Go, so the
// template only lays it out and its script only moves between batches.

type pageView struct {
	Title   string
	Read    string // when the page's batches were read
	Several bool   // several batches, each a tab, with All beside them
	Tabs    []tabView
	Asks    []askView  // every open ask of every batch shown, each once
	Waiting []waitView // the sessions waiting on their driver, every batch's
	Batches []batchView
	Empty   string // what to say when there is nothing to show
}

// tabView is a batch's tab, or All's.
type tabView struct {
	Anchor string // the batch's name, or "all"
	Name   string
	Sub    string
	Asks   int  // its open asks
	Unread bool // it was not read live
	Segs   []segView
}

// segView is how many sessions stand in one place, for the bar.
type segView struct {
	Kind  string // the state it is coloured as
	N     int
	Title string
}

type healthView struct {
	Title, Value, Sub string
	Alert             bool // something the owner should look at
}

// batchView is one batch's section of the page.
type batchView struct {
	Name    string // also its anchor
	Title   string // what the page calls it: its project
	Eyebrow string
	Driver  string // who drives it, and how they hear
	Rex     string // the Rex session its sessions open in
	Read    string // when it was read, and how long that took
	Live    string // why it was not read live
	Health  []healthView
	Segs    []segView
	Groups  []groupView
	Decided []decisionView
	Grants  []string
	Notes   []entryView // the driver's notes on no one session, newest first
	Holds   []string
	Empty   string
}

type groupView struct {
	Title  string
	Kind   string
	Strips []stripView
}

// stripView is one session: a summary that always shows, the rest once
// opened.
type stripView struct {
	Anchor  string // the batch's name and the label, so a link names one session on a page of several batches
	Label   string
	Kind    string // the state it is coloured as: next, work, park, alert, done
	State   string
	For     string
	Stage   string
	StageAt string
	Why     string   // a park's reason, in the driver's words then
	Since   []string // what happened after the park
	Goal    string
	Asks    []askLink // the open asks that name it, each linking to its question
	PRs     []link    // the pull requests it opened
	Chips   []string
	Facts   []factView
	Labels  []labelView
	History []entryView // the newest of its record, newest first
	Earlier []entryView // the rest
}

type askLink struct {
	ID   string
	Href template.URL
}

type factView struct {
	Title string
	Text  string
	Links []link // pull requests, after the text
	Error bool   // what could not be read
}

type link struct {
	Text  string
	URL   string
	State string
	Kind  string // merged, open, failing or closed
}

// labelView is one stage: when the session's newest step under it was
// taken, a dated fact and no verdict.
type labelView struct {
	Name, Age  string
	On, Newest bool
}

// entryView is one thing the record holds.
type entryView struct {
	When string
	Kind string
	Head string
	Text string
	Note bool // the driver's own words, set apart
}

// decisionView is a question the owner answered, or one that stopped
// mattering, where its link still lands.
type decisionView struct {
	Anchor   string
	ID       string
	Session  string
	Card     template.URL // its session's card; "" for the whole batch
	Question string
	Took     string
	Kind     string
	Said     string
	When     string
}

type askView struct {
	ID             string
	Anchor         string       // the batch's name and the ask's, since every batch numbers its asks from A1
	Batch          string       // the batch it was asked in, which the page's tabs show it under
	BatchTitle     string       // named when the page shows several
	Session        string       // the session it holds, whose card it links to; "" for the whole batch
	Card           template.URL // the link to that card
	Asked          string
	Goal           string
	Question       string
	Recommendation string
	IfUnanswered   string
}

// waitView is a session waiting on its driver, as All lists it beside
// every other batch's.
type waitView struct {
	Href  template.URL
	Label string
	Batch string
	Kind  string
	State string
	Goal  string
}

// href links to an anchor on the page. The anchor's slash stays a slash:
// a batch's name and a label hold none, and a driver writes the link.
func href(anchor string) template.URL { return template.URL("#" + anchor) }

// Anchor is where a session's card is on the page: the batch's name, a
// slash and the session's label, such as 2026-10-27/ON.
func Anchor(batchName, label string) string { return batchName + "/" + label }

// The history a strip shows before the rest folds away.
const historyShown = 12

// HTML is the page of one batch, as of now.
func (ov *Overview) HTML(now time.Time) []byte { return PageHTML([]*Overview{ov}, now) }

// PageHTML is the page of the batches ovs read, as of now: the questions
// waiting on the owner from every batch first, then each batch's sessions,
// each batch a tab when there are several. An Overview with no batch is a
// page with nothing to show.
func PageHTML(ovs []*Overview, now time.Time) []byte {
	v := pageView{Title: "drive"}
	var shown []*Overview
	for _, ov := range ovs {
		if ov.Batch != nil {
			shown = append(shown, ov)
		}
	}
	v.Several = len(shown) > 1
	names := pageNames(shown)
	switch len(shown) {
	case 0:
		v.Empty = "No batch is live."
	case 1:
		v.Title = "drive · " + names[0]
	}
	if len(ovs) > 0 {
		first := ovs[0]
		for _, ov := range ovs {
			if ov.At.Before(first.At) {
				first = ov
			}
		}
		v.Read = "Read at " + first.At.In(first.Zone).Format("15:04:05")
	}
	asks := 0
	for i, ov := range shown {
		bv := batchOf(ov, names[i], now)
		for _, a := range ov.Asks {
			av := askOf(ov, a, now)
			if v.Several {
				av.BatchTitle = names[i]
			}
			v.Asks = append(v.Asks, av)
		}
		asks += len(ov.Asks)
		for _, g := range bv.Groups {
			if g.Title != GroupPrompt && g.Title != GroupNext {
				continue
			}
			for _, s := range g.Strips {
				v.Waiting = append(v.Waiting, waitView{Href: href(s.Anchor), Label: s.Label, Batch: names[i], Kind: s.Kind,
					State: strings.TrimSpace(s.State + " " + s.For), Goal: s.Goal})
			}
		}
		v.Tabs = append(v.Tabs, tabView{Anchor: bv.Name, Name: names[i], Sub: tabSub(ov.Batch), Asks: len(ov.Asks), Unread: ov.Live != "", Segs: bv.Segs})
		v.Batches = append(v.Batches, bv)
	}
	if v.Several {
		all := tabView{Anchor: "all", Name: "All", Sub: fmt.Sprintf("%d live batches, and what waits in each", len(shown)), Asks: asks}
		v.Tabs = append([]tabView{all}, v.Tabs...)
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, v); err != nil {
		return []byte("the page did not render: " + template.HTMLEscapeString(err.Error()))
	}
	return out.Bytes()
}

// pageNames is what the page calls each batch: the Rex session its
// sessions open in, which names the project, else its driver, else its
// own name. Two batches the same name would give are told apart by their
// drivers, or their own names.
func pageNames(ovs []*Overview) []string {
	names := make([]string, len(ovs))
	count := map[string]int{}
	for i, ov := range ovs {
		names[i] = projectOf(ov.Batch)
		count[names[i]]++
	}
	for i, ov := range ovs {
		if count[names[i]] < 2 {
			continue
		}
		if d := batch.DriverOf(*ov.Batch); d != "" && d != names[i] {
			names[i] += " · " + d
		} else {
			names[i] += " · " + ov.Batch.Name
		}
	}
	return names
}

// projectOf names the project a batch works on. An older drive recorded a
// Rex session by its id, which names nothing.
func projectOf(m *batch.Meta) string {
	if l := m.RexSession; l != "" && !strings.HasPrefix(l, "session:") {
		return l
	}
	if d := batch.DriverOf(*m); d != "" {
		return d
	}
	return m.Name
}

func tabSub(m *batch.Meta) string {
	if d := batch.DriverOf(*m); d != "" {
		return d + " · " + m.Name
	}
	return m.Name
}

func askOf(ov *Overview, a *batch.Ask, now time.Time) askView {
	name := ov.Batch.Name
	av := askView{ID: a.ID, Anchor: name + "/ask-" + a.ID, Batch: name, Session: a.Session, Asked: "asked " + when(a.At, now, ov.Zone),
		Question: a.Question, Recommendation: a.Recommendation, IfUnanswered: a.IfUnanswered}
	if a.Session != "" {
		av.Card = href(Anchor(name, a.Session))
		for _, g := range ov.Groups {
			for _, c := range g.Cards {
				if c.Member.Label == a.Session {
					av.Goal = c.Member.Goal
				}
			}
		}
	}
	return av
}

func batchOf(ov *Overview, title string, now time.Time) batchView {
	at := func(t time.Time) string { return when(t, now, ov.Zone) }
	m := ov.Batch
	bv := batchView{Name: m.Name, Title: title, Eyebrow: "Batch " + m.Name + " · started " + pageClock(m.Started, now, ov.Zone), Holds: ov.Holds}
	driver := batch.DriverOf(*m)
	switch {
	case driver != "" && ov.Wakes:
		bv.Driver = "Driven by " + driver + ", woken by webhook"
	case driver != "":
		bv.Driver = "Driven by " + driver + ", which waits itself"
	case ov.Wakes:
		bv.Driver = "Its driver is woken by webhook"
	}
	if l := m.RexSession; l != "" && !strings.HasPrefix(l, "session:") {
		bv.Rex = l
	}
	if ov.Live != "" {
		bv.Live = "Not read live: " + ov.Live + ". What follows is what the batch's record holds."
	} else {
		bv.Read = fmt.Sprintf("Read at %s, in %s.", ov.At.In(ov.Zone).Format("15:04:05"), ov.Took.Round(100*time.Millisecond))
	}
	bv.Health = healthOf(ov, now)
	for _, g := range ov.Groups {
		bv.Segs = append(bv.Segs, segView{Kind: groupKind(g.Title), N: len(g.Cards), Title: strings.ToLower(g.Title[:1]) + g.Title[1:]})
		gv := groupView{Title: g.Title, Kind: groupKind(g.Title)}
		for _, c := range g.Cards {
			gv.Strips = append(gv.Strips, stripOf(ov, c, g.Title, now))
		}
		bv.Groups = append(bv.Groups, gv)
	}
	if len(ov.Groups) == 0 {
		bv.Empty = "No session has joined the batch yet."
	}
	answered := slices.Clone(ov.Answered)
	slices.SortStableFunc(answered, func(a, b *batch.Ask) int { return b.Answer.At.Compare(a.Answer.At) })
	for _, a := range answered {
		d := decisionView{Anchor: m.Name + "/ask-" + a.ID, ID: a.ID, Session: a.Session, Question: a.Question, Said: a.Answer.Source,
			When: fmt.Sprintf("asked %s, answered %s, %s later", pageClock(a.At, now, ov.Zone), pageClock(a.Answer.At, now, ov.Zone), ago(a.Answer.At.Sub(a.At)))}
		d.Took, d.Kind = answerTook(a.Answer)
		if a.Session != "" {
			d.Card = href(Anchor(m.Name, a.Session))
		} else {
			d.Session = "the whole batch"
		}
		bv.Decided = append(bv.Decided, d)
	}
	for _, g := range ov.Grants {
		bv.Grants = append(bv.Grants, fmt.Sprintf("%s, %s: %s, in your words %s", g.ID, at(g.At), g.Scope, g.Source))
	}
	for _, n := range slices.Backward(ov.Notes) {
		bv.Notes = append(bv.Notes, entryOf(n, nil, now, ov.Zone))
	}
	return bv
}

// groupKind is the state a group's sessions are coloured as.
func groupKind(title string) string {
	switch title {
	case GroupParked:
		return "park"
	case GroupWorking:
		return "work"
	case GroupUnread:
		return "alert"
	case GroupClosed:
		return "done"
	}
	return "next"
}

// healthOf is the batch at a glance: whether its driver acts, whether it
// hears, what has merged, and what the owner has decided. What was not
// read says so, rather than reading as none.
func healthOf(ov *Overview, now time.Time) []healthView {
	var out []healthView
	acted := healthView{Title: "Last driver action", Value: "None yet", Sub: "no step sent, open or park"}
	if e := ov.Acted; e != nil {
		acted.Value = pageClock(e.At, now, ov.Zone)
		acted.Sub = ago(now.Sub(e.At)) + " ago, " + actedWords(*e)
	}
	out = append(out, acted)

	var cards []Card
	for _, g := range ov.Groups {
		cards = append(cards, g.Cards...)
	}
	if ov.Wakes {
		wakes := healthView{Title: "Wakes", Value: "Not read"}
		if ov.Live == "" {
			var open, armed, parked, woken, unread int
			for _, c := range cards {
				m, r := c.Member, c.Row
				switch {
				case m.Closed:
				case m.Parked != "":
					parked++
				case r == nil || r.Error != "":
					unread++
				default:
					open++
					if r.Armed {
						armed++
					}
					if r.Unhandled {
						woken++
					}
				}
			}
			switch {
			case open > 0:
				wakes.Value = fmt.Sprintf("%d of %d armed", armed, open)
			case unread > 0:
				wakes.Value = "Not read"
			default:
				wakes.Value = "None needed"
			}
			var sub []string
			if woken > 0 {
				sub = append(sub, fmt.Sprintf("%d woken with nothing done since", woken))
				wakes.Alert = true
			}
			if armed < open {
				wakes.Alert = true
			}
			if unread > 0 {
				sub = append(sub, fmt.Sprintf("%d not read", unread))
				wakes.Alert = true
			}
			if parked > 0 {
				sub = append(sub, fmt.Sprintf("%d parked, which arm none", parked))
			}
			if len(sub) == 0 && open > 0 {
				sub = append(sub, "every open session")
			}
			wakes.Sub = strings.Join(sub, ", ")
		}
		out = append(out, wakes)
	}

	prs := healthView{Title: "Pull requests", Value: "Not read", Sub: "the sessions' steps were not read"}
	if ov.Live == "" {
		seen := map[prKey]bool{}
		var merged, open, closed, unread, failing int
		stepsUnread := 0
		for _, c := range cards {
			if c.Unread != "" {
				stepsUnread++
			}
			for _, p := range c.PRs {
				if seen[keyOf(p.PRRef)] {
					continue
				}
				seen[keyOf(p.PRRef)] = true
				switch {
				case p.PR == nil:
					unread++
				case p.PR.State == "MERGED":
					merged++
				case p.PR.State == "CLOSED":
					closed++
				default:
					open++
					if p.PR.Failed > 0 {
						failing++
					}
				}
			}
		}
		var parts []string
		for _, n := range []struct {
			n    int
			word string
		}{{merged, "merged"}, {open, "open"}, {closed, "closed"}, {unread, "not read"}} {
			if n.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n.n, n.word))
			}
		}
		var sub []string
		switch {
		case len(parts) > 0:
			prs.Value = strings.Join(parts, ", ")
		case stepsUnread > 0:
			prs.Value = "None found"
		default:
			prs.Value, sub = "None yet", []string{"no session has named one"}
		}
		if failing > 0 {
			sub, prs.Alert = append(sub, fmt.Sprintf("%d open with failing checks", failing)), true
		}
		if stepsUnread > 0 {
			sub = append(sub, plural(stepsUnread, "session")+" whose steps were not read")
		}
		prs.Sub = strings.Join(sub, ", ")
	}
	out = append(out, prs)

	decided := healthView{Title: "Your decisions", Value: "None asked yet"}
	took := map[string]int{}
	for _, a := range ov.Answered {
		took[a.Answer.Took]++
	}
	owner := len(ov.Answered) - took["moot"]
	if len(ov.Answered)+len(ov.Asks) > 0 {
		decided.Value = fmt.Sprintf("%d answered", owner)
		var sub []string
		for _, t := range []struct{ key, word string }{{"recommended", "as recommended"}, {"amended", "amended"}, {"delegated", "handed on"}, {"moot", "recorded moot"}} {
			if took[t.key] > 0 {
				sub = append(sub, fmt.Sprintf("%d %s", took[t.key], t.word))
			}
		}
		if len(ov.Asks) > 0 {
			sub = append(sub, fmt.Sprintf("%d open", len(ov.Asks)))
		}
		decided.Sub = strings.Join(sub, ", ")
	}
	out = append(out, decided)
	return out
}

// actedWords is what a driver did, in a few words.
func actedWords(e batch.Event) string {
	who := cmp.Or(e.Session, "the batch")
	switch e.Kind {
	case batch.KindSendIntent, batch.KindSend:
		return "a step to " + who
	case batch.KindRespond:
		return who + "'s prompt answered"
	case batch.KindOpenIntent, batch.KindOpen:
		return who + " opened"
	case batch.KindPark:
		return who + " parked"
	case batch.KindClose:
		return who + " closed"
	case batch.KindWatch:
		if e.Session == "" {
			return "a timer set"
		}
		return who + "'s wake armed"
	case batch.KindRefused:
		return "an action on " + who + " refused"
	}
	return e.Kind
}

// answerTook is how an answer was taken, and the state it is coloured as.
// A question that stopped mattering is no decision of the owner's.
func answerTook(e *batch.Event) (string, string) {
	words, kind := "Answered", "work"
	switch e.Took {
	case "recommended":
		words = "As recommended"
	case "amended":
		words, kind = "Amended", "you"
	case "delegated":
		words, kind = "Handed to a fresh model", "next"
	case "moot":
		words, kind = "Recorded moot", "done"
	}
	if e.Explained {
		words += ", once explained"
	}
	return words, kind
}

func stripOf(ov *Overview, c Card, group string, now time.Time) stripView {
	at := func(t time.Time) string { return when(t, now, ov.Zone) }
	m, r := c.Member, c.Row
	read := r != nil && r.Error == ""
	s := stripView{Anchor: Anchor(ov.Batch.Name, m.Label), Label: m.Label, Kind: groupKind(group), Goal: m.Goal}
	if read && (r.State == "error" || r.State == "blocked:auth") {
		s.Kind = "alert"
	}
	switch {
	case m.Closed:
		s.State, s.For = "Closed", at(m.ClosedAt)
	case read && r.State.onPrompt():
		s.State, s.For = stateWords(r.State), "for "+ago(r.For)
	case m.Parked != "":
		s.State, s.For = "Parked", at(m.ParkedAt)
	case read:
		s.State, s.For = stateWords(r.State), "for "+ago(r.For)
	default:
		s.State = "Not read live"
	}
	if m.Parked != "" && !m.Closed {
		s.Why = m.Parked
		s.Since = sinceThePark(ov, c, now)
		if read {
			s.Chips = append(s.Chips, strings.ToLower(stateWords(r.State))+" for "+ago(r.For))
		}
	}
	if c.Stage != "" {
		s.Stage, s.StageAt = c.Stage, pageClock(c.StageAt, now, ov.Zone)
	}
	for _, id := range c.Asks {
		s.Asks = append(s.Asks, askLink{ID: id, Href: href(ov.Batch.Name + "/ask-" + id)})
	}

	var opened, named []link
	for _, p := range c.PRs {
		l := link{Text: prName(p.PRRef), URL: p.URL, State: prWords(p), Kind: prKind(p)}
		if p.Opened {
			opened = append(opened, l)
		} else {
			named = append(named, l)
		}
	}
	s.PRs = opened
	if read && len(r.Jobs) > 0 && !m.Closed {
		s.Chips = append(s.Chips, plural(len(r.Jobs), "job")+" running")
	}
	if e := m.LastSend; e != nil {
		s.Chips = append(s.Chips, "sent "+pageClock(e.At, now, ov.Zone)+": "+clip(sentWords(*e), 48))
	}

	if len(opened) > 0 {
		s.Facts = append(s.Facts, factView{Title: "Opened", Text: "Pull requests it opened:", Links: opened})
	}
	if len(named) > 0 {
		s.Facts = append(s.Facts, factView{Title: "Named", Text: "Pull requests it named:", Links: named})
	}
	if e := m.LastSend; e != nil {
		s.Facts = append(s.Facts, factView{Title: "Last step", Text: "Sent " + at(e.At) + ": " + sentWords(*e) + delivered(e.Outcome)})
	}
	if c.Branch != "" {
		s.Facts = append(s.Facts, factView{Title: "Branch", Text: c.Branch})
	}
	if m.Dir != "" {
		s.Facts = append(s.Facts, factView{Title: "Directory", Text: tilde(m.Dir)})
	}
	if m.Parked != "" && !m.Closed {
		s.Facts = append(s.Facts, factView{Title: "Parked", Text: "Parked by the driver " + at(m.ParkedAt) + ". A parked session arms no wake until its next step."})
	}
	if read && !m.Closed {
		if len(r.Jobs) > 0 {
			s.Facts = append(s.Facts, factView{Title: "Jobs", Text: plural(len(r.Jobs), "background job") + " running"})
		}
		// Whether the driver will hear of it: a wake nobody acted on, one
		// that should have come and has not, or none at all.
		switch {
		case r.Unhandled:
			s.Facts = append(s.Facts, factView{Title: "Wake", Text: "The driver was woken " + at(m.Woken) + " and has done nothing since.", Error: true})
		case !ov.Wakes || m.Parked != "":
		case r.Due == "" && r.Armed:
			s.Facts = append(s.Facts, factView{Title: "Wake", Text: "Armed: its driver hears when its turn ends."})
		case r.Due == "":
		case r.Armed:
			s.Facts = append(s.Facts, factView{Title: "Wake", Text: "Its wake is armed but has not come since its turn ended, so the driver may not have heard.", Error: true})
		default:
			s.Facts = append(s.Facts, factView{Title: "Wake", Text: "No wake is armed for it, so the driver is not told when it needs its next step.", Error: true})
		}
	}
	if c.Unread != "" {
		s.Facts = append(s.Facts, factView{Title: "Not read", Text: "Not read: " + c.Unread, Error: true})
	}

	for _, l := range c.Labels {
		lv := labelView{Name: l.Name, Age: "·", On: !l.At.IsZero(), Newest: l.Name == c.Stage}
		if lv.On {
			lv.Age = ago(now.Sub(l.At))
		}
		s.Labels = append(s.Labels, lv)
	}

	asks := map[string]*batch.Event{} // the asks about it, for the answers to name what they answer
	for i := range c.History {
		if e := &c.History[i]; e.Kind == batch.KindAsk {
			asks[e.ID] = e
		}
	}
	for _, h := range slices.Backward(c.History) {
		e := entryOf(h, asks, now, ov.Zone)
		if len(s.History) < historyShown {
			s.History = append(s.History, e)
		} else {
			s.Earlier = append(s.Earlier, e)
		}
	}
	return s
}

// sinceThePark is what the record and the read show happened to a parked
// session after the park: answers to the questions about it, and where its
// turn has stood since. The park stays in the words it was written in.
func sinceThePark(ov *Overview, c Card, now time.Time) []string {
	m, r := c.Member, c.Row
	var out []string
	for _, e := range c.History {
		if e.Kind == batch.KindAnswer && e.At.After(m.ParkedAt) {
			out = append(out, "Since the park: "+answerHead(e)+" "+when(e.At, now, ov.Zone)+".")
		}
	}
	if r != nil && r.Error == "" && r.Since.After(m.ParkedAt) {
		out = append(out, "Since the park: "+sinceWords(r.State, pageClock(r.Since, now, ov.Zone))+".")
	}
	return out
}

// sinceWords is a turn's state, from when it began.
func sinceWords(s State, t string) string {
	switch s {
	case "working":
		return "it has been working since " + t
	case "done":
		return "its turn ended at " + t
	case "idle":
		return "it has been idle since " + t
	case "interrupted":
		return "it was interrupted at " + t
	case "error":
		return "it stopped on an API error at " + t
	case "blocked:permission":
		return "it has waited on a permission prompt since " + t
	case "blocked:question":
		return "it has waited on a question it asked since " + t
	case "blocked:auth":
		return "its login was rejected at " + t
	}
	return "it has read as " + string(s) + " since " + t
}

// answerHead says who settled a question and how. An answer the owner
// gave is theirs; one recorded moot is not.
func answerHead(e batch.Event) string {
	switch e.Took {
	case "recommended":
		return "you took the recommendation on " + e.Ref
	case "amended":
		return "you amended " + e.Ref
	case "delegated":
		return "you handed " + e.Ref + " to a fresh model"
	case "moot":
		return e.Ref + " was recorded moot"
	}
	return e.Ref + " was answered"
}

// entryOf is one thing the record holds, as its owner reads it. asks are
// the questions the answers among the history answer.
func entryOf(e batch.Event, asks map[string]*batch.Event, now time.Time, zone *time.Location) entryView {
	v := entryView{When: pageClock(e.At, now, zone), Kind: "next"}
	switch e.Kind {
	case batch.KindOpen:
		v.Kind, v.Head, v.Text = "done", "Opened", "in a new block of Rex."
		if e.Ref != "" {
			v.Head, v.Text = "Reopened", "in a new block, resuming its conversation."
		}
	case batch.KindAdopt:
		v.Kind, v.Head, v.Text = "done", "Joined the batch", "when the driver first sent to it."
	case batch.KindSend:
		v.Head, v.Text = "Sent", sentWords(e)+delivered(e.Outcome)
		if e.Outcome != "arrived" && e.Outcome != "queued" {
			v.Kind = "alert"
		}
	case batch.KindRespond:
		v.Head, v.Text = "Answered its prompt:", respondWords(e.Action)
	case batch.KindPark:
		v.Kind, v.Head, v.Text = "park", "Parked:", e.Reason
	case batch.KindClose:
		v.Kind, v.Head = "done", "Closed"
	case batch.KindAsk:
		v.Kind, v.Head, v.Text = "you", "Asked "+e.ID+":", e.Question
	case batch.KindAnswer:
		head := answerHead(e)
		v.Kind, v.Head, v.Text = "you", strings.ToUpper(head[:1])+head[1:], e.Source
		if e.Took == "moot" {
			v.Kind = "done"
		}
		if a := asks[e.Ref]; a != nil && e.Source == "" {
			v.Text = a.Question
		}
	case batch.KindNote:
		// A driver's note has an id; drive's own, such as an after-wake
		// command that failed, has none.
		v.Kind, v.Head, v.Text = "done", "drive noted:", e.Note
		if e.ID != "" {
			v.Head, v.Note = "Note "+e.ID, true
		}
	case batch.KindWake:
		v.Head = "Woke the driver"
		if e.Outcome != "" {
			v.Head += ":"
		}
		if e.Reason != "" {
			v.Kind, v.Head, v.Text = "alert", "A wake failed:", e.Reason
		} else if e.Outcome == "gone" {
			v.Kind, v.Text = "alert", "the session was gone."
		} else if e.Outcome != "" {
			v.Text = strings.ToLower(stateWords(State(e.Outcome))) + "."
		}
	case batch.KindFailure:
		v.Kind, v.Head, v.Text = "alert", "Failed:", e.Reason
	case batch.KindGoal:
		v.Kind, v.Head, v.Text = "done", "Goal recorded:", e.Goal
	default:
		v.Head = e.Kind
	}
	return v
}

// sentWords is the step a send carried: the snippet it came from, a file
// the driver wrote, or the line typed.
func sentWords(e batch.Event) string {
	switch {
	case e.Key != "":
		return "the snippet " + e.Key
	case e.Mode == "file":
		return "a file the driver wrote"
	}
	return "“" + clip(strings.TrimSpace(stepOf(e)), 160) + "”"
}

// delivered is how a step reached its session, when that is worth saying.
func delivered(outcome string) string {
	switch outcome {
	case "arrived", "":
		return ""
	case "queued":
		return ", queued behind its turn"
	case "recovered":
		return ", retyped after its start was lost"
	case "garbled":
		return ", which arrived without its start"
	case "other":
		return ", though the session received something else"
	case "unknown":
		return ", which may not have arrived"
	}
	return ", " + outcome
}

func respondWords(action string) string {
	switch action {
	case "approve":
		return "approved."
	case "deny":
		return "denied."
	case "interrupt":
		return "interrupted the turn."
	case "clear":
		return "cleared its input box."
	}
	return action + "."
}

func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// tilde is a directory with the home directory as ~.
func tilde(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(dir, home+"/") {
		return "~" + dir[len(home):]
	}
	return dir
}

// prName is a pull request with its repository's name, since a batch's
// sessions may work in several.
func prName(ref progress.PRRef) string {
	repo := ref.Repo[strings.LastIndex(ref.Repo, "/")+1:]
	return fmt.Sprintf("%s#%d", repo, ref.Number)
}

// prKind is the state a pull request is coloured as.
func prKind(p PRView) string {
	switch {
	case p.PR == nil:
		return "unread"
	case p.PR.State == "MERGED":
		return "merged"
	case p.PR.State == "CLOSED":
		return "closed"
	case p.PR.Failed > 0:
		return "failing"
	}
	return "open"
}

// stateWords is a turn's state as the owner reads it.
func stateWords(s State) string {
	switch s {
	case "working":
		return "Working"
	case "done":
		return "Turn ended"
	case "idle":
		return "Idle, with no prompt yet"
	case "interrupted":
		return "Interrupted"
	case "error":
		return "Stopped on an API error"
	case "blocked:permission":
		return "Waiting on a permission prompt"
	case "blocked:question":
		return "Waiting on a question it asked"
	case "blocked:auth":
		return "Its login was rejected"
	}
	return string(s)
}

// prWords is a pull request's state as read: merged, closed, or open with
// how its checks and review stand.
func prWords(p PRView) string {
	pr := p.PR
	switch {
	case pr == nil && p.Err != "":
		return "not read: " + p.Err
	case pr == nil:
		return ""
	case pr.State == "MERGED":
		return "merged"
	case pr.State == "CLOSED":
		return "closed without merging"
	}
	words := []string{"open"}
	if pr.Draft {
		words[0] = "draft"
	}
	checks := func(n int, how string) string {
		if n == 1 {
			return "1 check " + how
		}
		return fmt.Sprintf("%d checks %s", n, how)
	}
	switch {
	case pr.Failed > 0:
		words = append(words, checks(pr.Failed, "failing"))
	case pr.Pending > 0:
		words = append(words, checks(pr.Pending, "pending"))
	case pr.Passed > 0:
		words = append(words, "checks passing")
	}
	switch pr.Review {
	case "APPROVED":
		words = append(words, "approved")
	case "CHANGES_REQUESTED":
		words = append(words, "changes requested")
	}
	if pr.Merge == "DIRTY" {
		words = append(words, "conflicts with its base")
	}
	return strings.Join(words, ", ")
}

// when is a time as the page says it: the clock alone today, the weekday
// within the week, the date before that.
func when(t, now time.Time, zone *time.Location) string {
	if t.IsZero() {
		return "at an unrecorded time"
	}
	c := pageClock(t, now, zone)
	if !strings.Contains(c, " ") {
		return "at " + c
	}
	return c
}

// pageClock is a time as a column of times shows it: 15:04 today, Mon 15:04
// within the week, 2 Jan 15:04 before that.
func pageClock(t, now time.Time, zone *time.Location) string {
	if t.IsZero() {
		return "–"
	}
	t, now = t.In(zone), now.In(zone)
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}

//go:embed page.html
var pageSource string

var pageTemplate = template.Must(template.New("page").Parse(pageSource))
