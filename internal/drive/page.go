package drive

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// What the page prints, for the owner on a phone: the words are theirs, not
// a driver's. Every fact is in the view below, worded in Go, so the
// template only lays it out.

type pageView struct {
	Title  string
	Batch  string // what the header says of the batch
	Read   string // when the page was read, and how long that took
	Live   string // why the sessions were not read live
	Asks   []askView
	Groups []groupView
	Holds  []string
	Empty  string // what to say when there is nothing to show
}

type askView struct {
	Head           string // its id, the session it holds and when it was asked
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
	State  string
	Asks   string
	Goal   string
	Facts  []fact
	Closed bool
}

type fact struct {
	Text  string
	Links []link // pull requests, after the text
	Minor bool   // secondary: delivery metadata
	Error bool   // what could not be read
}

type link struct {
	Text, URL, State string
}

// HTML is the page, as of now.
func (ov *Overview) HTML(now time.Time) []byte {
	v := pageView{Title: "drive"}
	at := func(t time.Time) string { return when(t, now, ov.Zone) }
	if ov.Batch == nil {
		v.Empty = "There is no current batch."
	} else {
		v.Title = "drive · " + ov.Batch.Name
		v.Batch = fmt.Sprintf("Batch %s, started %s", ov.Batch.Name, at(ov.Batch.Started))
	}
	v.Read = fmt.Sprintf("Read at %s, in %s.", ov.At.In(ov.Zone).Format("15:04:05"), ov.Took.Round(100*time.Millisecond))
	v.Live = ov.Live
	goals := map[string]string{}
	for _, g := range ov.Groups {
		for _, c := range g.Cards {
			goals[c.Member.Label] = c.Member.Goal
		}
	}
	for _, a := range ov.Asks {
		head := a.ID
		if a.Session != "" {
			head += " · " + a.Session
		} else {
			head += " · the whole batch"
		}
		v.Asks = append(v.Asks, askView{Head: head + " · asked " + at(a.At), Goal: goals[a.Session], Question: a.Question,
			Recommendation: a.Recommendation, IfUnanswered: a.IfUnanswered})
	}
	for _, g := range ov.Groups {
		gv := groupView{Title: g.Title}
		for _, c := range g.Cards {
			gv.Cards = append(gv.Cards, cardOf(c, ov.Wakes, now, ov.Zone))
		}
		v.Groups = append(v.Groups, gv)
	}
	v.Holds = ov.Holds
	if ov.Batch != nil && len(ov.Groups) == 0 {
		v.Empty = "No session has joined the batch yet."
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, v); err != nil {
		return []byte("the page did not render: " + template.HTMLEscapeString(err.Error()))
	}
	return out.Bytes()
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
	if len(c.Asks) > 0 {
		v.Asks = "Waits on you: " + strings.Join(c.Asks, ", ")
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
h2 { font-size: 1rem; margin: 28px 0 8px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 10px; padding: 12px 14px; margin: 0 0 10px; }
.card.closed { opacity: 0.85; }
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
{{with .Live}}<p class="banner">Not read live: {{.}}. What follows is what the batch's record holds.</p>{{end}}
</header>
{{if .Empty}}<p class="empty">{{.Empty}}</p>{{end}}
{{if or .Asks .Groups}}
<h2>Needs you</h2>
{{range .Asks}}<article class="card ask">
<p class="head">{{.Head}}</p>
{{with .Goal}}<p class="goal">{{.}}</p>{{end}}
<p class="question">{{.Question}}</p>
{{with .Recommendation}}<p>Recommended: {{.}}</p>{{end}}
<p>If unanswered: {{.IfUnanswered}}</p>
</article>
{{else}}<p class="empty">Nothing waits on you.</p>
{{end}}
{{end}}
{{range .Groups}}<h2>{{.Title}}</h2>
{{range .Cards}}<article class="card{{if .Closed}} closed{{end}}">
<div class="top"><span class="label">{{.Label}}</span>{{with .State}}<span class="state">{{.}}</span>{{end}}{{with .Asks}}<span class="badge">{{.}}</span>{{end}}</div>
{{if .Goal}}<p class="goal">{{.Goal}}</p>{{else}}<p class="goal none">No goal recorded.</p>{{end}}
{{if .Facts}}<ul class="facts">
{{range .Facts}}<li{{if .Minor}} class="minor"{{else if .Error}} class="error"{{end}}>{{.Text}}{{range $i, $l := .Links}}{{if $i}};{{end}} <span class="pr">{{if .URL}}<a href="{{.URL}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}{{with .State}} {{.}}{{end}}</span>{{end}}</li>
{{end}}</ul>{{end}}
</article>
{{end}}{{end}}
{{if .Holds}}<h2>Holds</h2>
<ul class="facts">{{range .Holds}}<li>{{.}}</li>{{end}}</ul>
{{end}}
<footer>Reload to read it again; a read is reused for 30 seconds.</footer>
</main>
</body>
</html>
`))
