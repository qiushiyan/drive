package drive

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
)

// What the page prints, for the owner on a phone: the words are theirs, not
// a driver's. Every fact is in the view below, worded in Go, so the
// template only lays it out.

type pageView struct {
	Title   string
	Batch   string   // what the header says of the batch, or the batches
	Read    string   // when the page was read, and how long that took
	Live    []string // why sessions were not read live, a line per batch
	Asks    []askView
	Batches []batchView
	Several bool   // the page shows several drivers' batches, each under its own heading
	Empty   string // what to say when there is nothing to show
}

// batchView is one batch's sessions on the page.
type batchView struct {
	Name   string // also its anchor
	Title  string // its heading, when the page shows several
	Groups []groupView
	Holds  []string
	Empty  string
}

type askView struct {
	ID             string
	Anchor         string       // the batch's name and the ask's, since every batch numbers its asks from A1
	Batch          string       // the batch it was asked in, named when the page shows several
	Session        string       // the session it holds, whose card it links to; "" for the whole batch
	Card           template.URL // the link to that card
	Asked          string
	Goal           string
	Question       string
	Recommendation string
	IfUnanswered   string
}

type groupView struct {
	Title string
	Cards []cardView
}

type cardView struct {
	Label  string
	Anchor string // the batch's name and the label, so a link names one session's card on a page of several drivers' batches
	State  string
	Asks   []askLink // the open asks that name it, each linking to its question
	Goal   string
	Facts  []fact
	Closed bool
}

type askLink struct {
	ID   string
	Href template.URL
}

// href links to an anchor on the page. The anchor's slash stays a slash:
// a batch's name and a label hold none, and a driver writes the link.
func href(anchor string) template.URL { return template.URL("#" + anchor) }

type fact struct {
	Text  string
	Links []link // pull requests, after the text
	Minor bool   // secondary: delivery metadata
	Error bool   // what could not be read
}

type link struct {
	Text, URL, State string
}

// Anchor is where a session's card is on the page: the batch's name, a
// slash and the session's label, such as 2026-10-27/ON.
func Anchor(batchName, label string) string { return batchName + "/" + label }

// HTML is the page of one batch, as of now.
func (ov *Overview) HTML(now time.Time) []byte { return PageHTML([]*Overview{ov}, now) }

// PageHTML is the page of the batches ovs read, as of now: the questions
// waiting on the owner from every batch first, then each batch's sessions.
// An Overview with no batch is a page with nothing to show.
func PageHTML(ovs []*Overview, now time.Time) []byte {
	v := pageView{Title: "drive"}
	var shown []*Overview
	for _, ov := range ovs {
		if ov.Batch != nil {
			shown = append(shown, ov)
		}
	}
	v.Several = len(shown) > 1
	switch {
	case len(shown) == 0:
		v.Empty = "No batch is live."
	case len(shown) == 1:
		ov := shown[0]
		v.Title = "drive · " + ov.Batch.Name
		v.Batch = fmt.Sprintf("Batch %s%s, started %s", ov.Batch.Name, driverClause(ov.Batch), when(ov.Batch.Started, now, ov.Zone))
	default:
		var names []string
		for _, ov := range shown {
			names = append(names, ov.Batch.Name)
		}
		v.Batch = "Batches " + strings.Join(names, ", ")
	}
	if len(ovs) > 0 {
		first, took := ovs[0], time.Duration(0)
		for _, ov := range ovs {
			took = max(took, ov.Took)
		}
		v.Read = fmt.Sprintf("Read at %s, in %s.", first.At.In(first.Zone).Format("15:04:05"), took.Round(100*time.Millisecond))
	}
	for _, ov := range shown {
		at := func(t time.Time) string { return when(t, now, ov.Zone) }
		name := ov.Batch.Name
		if ov.Live != "" {
			if v.Several {
				v.Live = append(v.Live, fmt.Sprintf("Batch %s not read live: %s. What follows of it is what its record holds.", name, ov.Live))
			} else {
				v.Live = append(v.Live, fmt.Sprintf("Not read live: %s. What follows is what the batch's record holds.", ov.Live))
			}
		}
		goals := map[string]string{}
		for _, g := range ov.Groups {
			for _, c := range g.Cards {
				goals[c.Member.Label] = c.Member.Goal
			}
		}
		for _, a := range ov.Asks {
			av := askView{ID: a.ID, Anchor: name + "/ask-" + a.ID, Session: a.Session, Asked: "asked " + at(a.At), Goal: goals[a.Session],
				Question: a.Question, Recommendation: a.Recommendation, IfUnanswered: a.IfUnanswered}
			if a.Session != "" {
				av.Card = href(Anchor(name, a.Session))
			}
			if v.Several {
				av.Batch = name
			}
			v.Asks = append(v.Asks, av)
		}
		bv := batchView{Name: name, Holds: ov.Holds}
		if v.Several {
			bv.Title = fmt.Sprintf("Batch %s%s, started %s", name, driverClause(ov.Batch), at(ov.Batch.Started))
		}
		for _, g := range ov.Groups {
			gv := groupView{Title: g.Title}
			for _, c := range g.Cards {
				cv := cardOf(c, ov.Wakes, now, ov.Zone)
				cv.Anchor = Anchor(name, c.Member.Label)
				for _, id := range c.Asks {
					cv.Asks = append(cv.Asks, askLink{ID: id, Href: href(name + "/ask-" + id)})
				}
				gv.Cards = append(gv.Cards, cv)
			}
			bv.Groups = append(bv.Groups, gv)
		}
		if len(ov.Groups) == 0 {
			bv.Empty = "No session has joined the batch yet."
		}
		v.Batches = append(v.Batches, bv)
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, v); err != nil {
		return []byte("the page did not render: " + template.HTMLEscapeString(err.Error()))
	}
	return out.Bytes()
}

// driverClause names a batch's driver in its heading, where it names one.
func driverClause(m *batch.Meta) string {
	if driver := batch.DriverOf(*m); driver != "" {
		return ", driven by " + driver
	}
	return ""
}

func cardOf(c Card, wakes bool, now time.Time, zone *time.Location) cardView {
	at := func(t time.Time) string { return when(t, now, zone) }
	m, r := c.Member, c.Row
	v := cardView{Label: m.Label, Goal: m.Goal, Closed: m.Closed}
	switch {
	case m.Closed:
		v.State = "Closed " + at(m.ClosedAt)
	case r != nil && r.Error == "":
		v.State = stateWords(r.State) + " for " + ago(r.For)
	}
	if m.Parked != "" && !m.Closed {
		v.Facts = append(v.Facts, fact{Text: "Parked by the driver " + at(m.ParkedAt) + ": " + m.Parked})
	}
	var opened, named []link
	for _, p := range c.PRs {
		l := link{Text: fmt.Sprintf("#%d", p.Number), URL: p.URL, State: prWords(p)}
		if p.Opened {
			opened = append(opened, l)
		} else {
			named = append(named, l)
		}
	}
	if len(opened) > 0 {
		v.Facts = append(v.Facts, fact{Text: "Pull requests it opened:", Links: opened})
	}
	if len(named) > 0 {
		v.Facts = append(v.Facts, fact{Text: "Pull requests it named:", Links: named})
	}
	if c.Stage != "" {
		v.Facts = append(v.Facts, fact{Text: "Last recorded step: " + c.Stage + ", " + at(c.StageAt)})
	}
	if r != nil && !m.Closed {
		// Whether the driver will hear of it: a wake nobody acted on, one
		// that should have come and has not, or none at all.
		switch {
		case r.Unhandled:
			v.Facts = append(v.Facts, fact{Text: "The driver was woken " + at(m.Woken) + " and has done nothing since."})
		case !wakes || r.Due == "" || m.Parked != "":
		case r.Armed:
			v.Facts = append(v.Facts, fact{Text: "Its wake is armed but has not come since its turn ended, so the driver may not have heard."})
		default:
			v.Facts = append(v.Facts, fact{Text: "No wake is armed for it, so the driver is not told when it needs its next step."})
		}
		if len(r.Jobs) > 0 {
			v.Facts = append(v.Facts, fact{Text: fmt.Sprintf("Background jobs running: %d", len(r.Jobs)), Minor: true})
		}
	}
	if s := m.LastSend; s != nil {
		v.Facts = append(v.Facts, fact{Text: "Last step sent " + at(s.At) + ": " + sentLabel(s), Minor: true})
	}
	if c.Unread != "" {
		v.Facts = append(v.Facts, fact{Text: "Not read: " + c.Unread, Error: true})
	}
	return v
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
	t, now = t.In(zone), now.In(zone)
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "at " + t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { --bg: #f7f7f5; --card: #ffffff; --ink: #1d1d1b; --muted: #6b6b66; --line: #e4e3df; --accent: #8a4b00; --accent-bg: #fff3e0; --error: #a1261d; --link: #1a5fb4; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #161615; --card: #20201e; --ink: #ecebe6; --muted: #a09f99; --line: #33332f; --accent: #f0b35a; --accent-bg: #3a2a12; --error: #f08a80; --link: #8fb8f0; }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--ink); font: 16px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; -webkit-text-size-adjust: 100%; }
main { max-width: 760px; margin: 0 auto; padding: 16px 16px 48px; }
header.page h1 { font-size: 1.25rem; margin: 0 0 2px; }
header.page p { margin: 0; color: var(--muted); font-size: 0.9rem; }
.banner { margin: 12px 0 0; padding: 10px 12px; border-radius: 8px; background: var(--accent-bg); color: var(--accent); }
h2, h3 { font-size: 1rem; margin: 28px 0 8px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; }
h2.batch { font-size: 1.1rem; margin: 40px 0 0; padding-top: 12px; border-top: 1px solid var(--line); color: var(--ink); text-transform: none; letter-spacing: 0; }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 10px; padding: 12px 14px; margin: 0 0 10px; }
.card.closed { opacity: 0.85; }
.card:target { border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-bg); }
.badge a { color: inherit; }
.top { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 10px; }
.label { font-weight: 650; }
.state { color: var(--muted); font-size: 0.9rem; }
.badge { font-size: 0.8rem; font-weight: 600; color: var(--accent); background: var(--accent-bg); padding: 1px 8px; border-radius: 999px; }
.goal { margin: 6px 0 0; }
.goal.none { color: var(--muted); font-style: italic; }
ul.facts { list-style: none; margin: 8px 0 0; padding: 0; font-size: 0.92rem; }
ul.facts li { margin: 3px 0; overflow-wrap: anywhere; }
ul.facts li.minor { color: var(--muted); }
ul.facts li.error { color: var(--error); }
a { color: var(--link); }
.pr a { white-space: nowrap; }
.ask p { margin: 4px 0 0; }
.ask .head { color: var(--muted); font-size: 0.9rem; margin: 0; }
.ask .question { font-weight: 600; }
.empty { color: var(--muted); }
footer { margin-top: 32px; color: var(--muted); font-size: 0.85rem; }
</style>
</head>
<body>
<main>
<header class="page">
<h1>{{if .Batch}}{{.Batch}}{{else}}drive{{end}}</h1>
<p>{{.Read}}</p>
{{range .Live}}<p class="banner">{{.}}</p>
{{end}}</header>
{{if .Empty}}<p class="empty">{{.Empty}}</p>{{end}}
{{if .Batches}}
<h2>Needs you</h2>
{{range .Asks}}<article class="card ask" id="{{.Anchor}}">
<p class="head">{{.ID}}{{with .Batch}} · {{.}}{{end}} · {{if .Session}}<a href="{{.Card}}">{{.Session}}</a>{{else}}the whole batch{{end}} · {{.Asked}}</p>
{{with .Goal}}<p class="goal">{{.}}</p>{{end}}
<p class="question">{{.Question}}</p>
{{with .Recommendation}}<p>Recommended: {{.}}</p>{{end}}
<p>If unanswered: {{.IfUnanswered}}</p>
</article>
{{else}}<p class="empty">Nothing waits on you.</p>
{{end}}
{{end}}
{{range .Batches}}<section class="batch" id="{{.Name}}">
{{with .Title}}<h2 class="batch">{{.}}</h2>
{{end}}{{with .Empty}}<p class="empty">{{.}}</p>
{{end}}{{range .Groups}}<h3>{{.Title}}</h3>
{{range .Cards}}<article class="card{{if .Closed}} closed{{end}}" id="{{.Anchor}}">
<div class="top"><span class="label">{{.Label}}</span>{{with .State}}<span class="state">{{.}}</span>{{end}}{{with .Asks}}<span class="badge">Waits on you:{{range $i, $a := .}}{{if $i}},{{end}} <a href="{{$a.Href}}">{{$a.ID}}</a>{{end}}</span>{{end}}</div>
{{if .Goal}}<p class="goal">{{.Goal}}</p>{{else}}<p class="goal none">No goal recorded.</p>{{end}}
{{if .Facts}}<ul class="facts">
{{range .Facts}}<li{{if .Minor}} class="minor"{{else if .Error}} class="error"{{end}}>{{.Text}}{{range $i, $l := .Links}}{{if $i}};{{end}} <span class="pr">{{if .URL}}<a href="{{.URL}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{with .State}} {{.}}{{end}}</span>{{end}}</li>
{{end}}</ul>{{end}}
</article>
{{end}}{{end}}{{if .Holds}}<h3>Holds</h3>
<ul class="facts">{{range .Holds}}<li>{{.}}</li>{{end}}</ul>
{{end}}</section>
{{end}}
<footer>Reload to read it again; a read is reused for 30 seconds.</footer>
</main>
</body>
</html>
`))
