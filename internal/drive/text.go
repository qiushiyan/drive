package drive

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/turn"
)

// The text each result prints on the command line, beside the wake's
// (Ending.Text): what a driver reads is drive's product, so it lives with
// the facts it reports.

// Text is the line a send prints.
func (s *Sent) Text() string {
	what := s.Typed
	if s.Path != "" {
		what = s.Path
	}
	extra := ""
	if s.Notice {
		extra += ", notice first"
	}
	if s.From != "" {
		extra += ", snippet from " + s.From
	}
	if s.Watched {
		extra += "; wake armed"
	}
	return fmt.Sprintf("%s %s: %s%s\n", s.Outcome, s.Label, what, extra)
}

// Text is what an open prints: the block it opened, then its first step.
func (o *Opened) Text() string {
	s := fmt.Sprintf("opened %s in block %s on account %s (session %s)\n", o.Label, rex.Short(o.Block), o.Account, o.Session)
	if o.Sent != nil && o.Sent.Outcome != "" {
		s += o.Sent.Text()
	}
	return s
}

// Text is the line a respond prints.
func (r *Responded) Text() string {
	s := r.Action + " " + r.Label
	if r.Call != "" {
		s += " (" + r.Call + ")"
	}
	return s + "; it now reads " + r.After + "\n"
}

// Text is the line a watch prints.
func (w *Watched) Text() string {
	switch {
	case w.Restarted:
		return w.Label + "'s watch was stuck long after its turn ended; started it again\n"
	case w.Already:
		return w.Label + " is already watched\n"
	}
	return "watching " + w.Label + ": the driver is woken when its turn ends\n"
}

// Text is the line --due prints for a session whose wake it armed.
func (r Rearmed) Text() string {
	s := fmt.Sprintf("watching %s: %s\n", r.Label, r.Why)
	if r.Failures >= 2 {
		s += fmt.Sprintf("  %s's watch has ended without a wake %d times this batch; watches.log in the batch directory says why\n", r.Label, r.Failures)
	}
	return s
}

// LaterText is the line --due prints for a wake not due yet.
func (r Rearmed) LaterText() string {
	return fmt.Sprintf("%s is %s: a run that acts on it first makes that moot\n", r.Label, r.Why)
}

// Text is what a read prints: the text, where the session stands, its view
// and next.
func (r *Reading) Text() string {
	var b strings.Builder
	b.WriteString(r.Body + "\n\n")
	fmt.Fprintf(&b, "%s is %s on account %s (session %s)", r.Label, r.State, orDash(r.Account), orDash(r.SID))
	switch r.Input {
	case "typed":
		fmt.Fprintf(&b, "; its input box holds typed text nobody sent: %q", r.Typed)
	case "suggestion":
		b.WriteString("; its input box shows only Claude Code's suggestion")
	}
	for _, j := range r.Jobs {
		b.WriteString("; a background job of its runs: " + clip(j.Command, 100))
	}
	b.WriteString("\n")
	if r.View.Session != "" { // a closed or gone session has nothing to act on
		fmt.Fprintf(&b, "view: %s\n", r.View)
	}
	if r.Next != "" {
		fmt.Fprintf(&b, "next: %s\n", r.Next)
	}
	return b.String()
}

// viewOrEmpty is a view as JSON gives it: "" for a closed or gone session, which has none.
func viewOrEmpty(v turn.View) string {
	if v.Session == "" {
		return ""
	}
	return v.String()
}

// Text is what status prints: the batch, a row per session, then what
// stands out across them.
func (st *Status) Text(now time.Time) string {
	var b strings.Builder
	if st.Batch == nil {
		b.WriteString("no current batch (`drive batch new NAME` starts one); Claude sessions in Rex:\n")
	} else {
		fmt.Fprintf(&b, "batch %s · wakes: %s · Rex session %s · times %s (%s)\n", st.Batch.Name, st.Batch.Wake,
			orDash(st.Batch.RexSession), st.Zone, now.In(st.Zone).Format("MST, UTC-07:00"))
	}
	if len(st.Rows) > 0 {
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SESSION\tWIN\tBLOCK\tSTATE\tFOR\tINPUT\tSTAGE\tPR\tLAST SENT\tWAKE\tVIEW")
		for _, r := range st.Rows {
			if r.Error != "" {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\t\t\t\t%s\t%s\t%s\n", r.Label, orDash(r.Window), rex.Short(r.Block), r.State, r.LastSent, r.Watch, r.Error)
				continue
			}
			sent := r.LastSent
			if !r.SentAt.IsZero() {
				sent += " " + clock(r.SentAt, st.Zone, now)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Label, orDash(r.Window), rex.Short(r.Block), r.State,
				ago(r.For), r.Input, r.Stage, prText(r), sent, r.Watch, r.View)
		}
		w.Flush()
	} else if st.Batch != nil && st.Closed > 0 {
		fmt.Fprintf(&b, "no open sessions in this batch (%d closed)\n", st.Closed)
	} else if st.Batch != nil {
		b.WriteString("no sessions in this batch yet\n")
	}
	for _, r := range st.Rows {
		if r.Input == "typed" {
			fmt.Fprintf(&b, "%s's input box holds typed text nobody sent: %q\n", r.Label, r.Typed)
		}
		for _, j := range r.Jobs {
			fmt.Fprintf(&b, "%s runs a background job since %s: %s\n", r.Label, clock(j.Started, st.Zone, now), clip(j.Command, 100))
		}
	}
	if len(st.Others) > 0 {
		b.WriteString("\nother Claude sessions in Rex:\n")
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, r := range st.Others {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", orDash(r.Window), r.Label, r.State, ago(r.For), r.Name)
		}
		w.Flush()
	}
	for _, h := range st.Holds {
		b.WriteString("hold " + h + "\n")
	}
	for _, r := range st.Rows {
		if strings.HasPrefix(r.Watch, "ended without a wake") && st.Batch != nil {
			fmt.Fprintf(&b, "what each watch printed: %s\n", st.Dir+"/watches.log")
			break
		}
	}
	for _, p := range st.InFlight {
		if p.Driver != nil {
			fmt.Fprintf(&b, "in flight: %s to %s since %s, by pid %d (%s)\n", p.Kind, p.Session, clock(p.At, st.Zone, now), p.Driver.PID, clip(p.Text, 60))
		} else {
			fmt.Fprintf(&b, "left unfinished: %s to %s since %s, by a driver that has gone (%s); the next send, respond, park or close on %s settles it\n",
				p.Kind, p.Session, clock(p.At, st.Zone, now), clip(p.Text, 60), p.Session)
		}
	}
	for _, g := range st.Grants {
		fmt.Fprintf(&b, "grant %s (%s): %s — %s\n", g.ID, clock(g.At, st.Zone, now), g.Scope, g.Source)
	}
	for _, q := range st.Asks {
		fmt.Fprintf(&b, "open ask %s for %s, %s: %s — unanswered: %s\n", q.ID, orDash(q.Session), ago(now.Sub(q.At)), q.Question, q.IfUnanswered)
	}
	return b.String()
}

// clock is a time as drive prints it: the hour alone today, with the date
// otherwise.
func clock(t time.Time, zone *time.Location, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = t.In(zone), now.In(zone)
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// ago is how long ago something was, as status's FOR column and every
// message give it.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func prText(r Row) string {
	switch {
	case r.PR != nil:
		pr := r.PR
		s := fmt.Sprintf("#%d %s", pr.Number, strings.ToLower(pr.State))
		if pr.Draft {
			s += " draft"
		}
		if pr.State == "OPEN" {
			s += fmt.Sprintf(": %d pass", pr.Passed)
			if pr.Failed > 0 {
				s += fmt.Sprintf(", %d fail", pr.Failed)
			}
			if pr.Pending > 0 {
				s += fmt.Sprintf(", %d pending", pr.Pending)
			}
			if pr.Merge != "" {
				s += ", " + strings.ToLower(pr.Merge)
			}
		}
		return s
	case r.PRRef != nil:
		return fmt.Sprintf("#%d (%s)", r.PRRef.Number, r.PRErr)
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// LogText is what log prints: a line per event, in the batch's zone.
func LogText(b *batch.Batch, evs []batch.Event, now time.Time) string {
	var out strings.Builder
	zone := b.Zone()
	for _, e := range evs {
		fmt.Fprintf(&out, "%s  %-12s %-8s %s\n", clock(e.At, zone, now), e.Kind, e.Session, Describe(e))
	}
	return out.String()
}

// Describe is an event as the log tells it.
func Describe(e batch.Event) string {
	switch e.Kind {
	case batch.KindSend:
		s := e.Outcome + ": " + clip(e.Text, 100)
		if e.Key != "" {
			s = e.Outcome + ": snippet " + e.Key
		}
		if e.Notice {
			s += " (the notice first)"
		}
		if e.Hold != "" {
			s += " (holds " + e.Hold + ")"
		}
		return s
	case batch.KindSendIntent, batch.KindOpenIntent:
		return clip(e.Text, 100)
	case batch.KindOpen, batch.KindAdopt:
		return fmt.Sprintf("block %s, account %s, %s", rex.Short(e.Block), e.Account, e.Dir)
	case batch.KindRespond:
		s := e.Action + " " + clip(e.Call, 100)
		if e.Outcome != "" {
			s += " → " + e.Outcome
		}
		return s
	case batch.KindWake:
		what := orDash(e.Outcome)
		if e.Session == "" {
			what = "timer " + e.Note
		}
		if e.HTTP == 0 && e.Reason != "" {
			return what + ", post failed: " + e.Reason
		}
		return fmt.Sprintf("%s, posted (http %d, try %d)", what, e.HTTP, e.Try)
	case batch.KindGrant:
		return e.ID + " " + e.Scope + " — " + e.Source
	case batch.KindAsk:
		return e.ID + " " + e.Question + " (recommends: " + e.Recommendation + "; unanswered: " + e.IfUnanswered + ")"
	case batch.KindAnswer:
		ex := ""
		if e.Explained {
			ex = ", needed explaining"
		}
		return e.Ref + " " + e.Took + ex + " — " + e.Source
	case batch.KindNote:
		return e.Note
	case batch.KindRefused, batch.KindFailure, batch.KindPark:
		return clip(e.Reason, 140)
	}
	return clip(e.Note+" "+e.Reason, 140)
}

// Text is what metrics prints.
func (m *Metrics) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "batch %s, %s → %s, %d sessions\n", m.Batch, m.From.In(m.Zone).Format("01-02 15:04"), m.To.In(m.Zone).Format("01-02 15:04 MST"), m.Sessions)
	fmt.Fprintf(&b, "asks: %d (%d answered, %d open); took the recommendation %d, amended %d, delegated %d, moot %d; needed explaining %d\n",
		m.Asks, m.Answered, m.Unanswered, m.Took["recommended"], m.Took["amended"], m.Took["delegated"], m.Took["moot"], m.Explained)
	fmt.Fprintf(&b, "owner wait: %s in all, median answer %s", m.OwnerWait.Round(time.Minute), m.MedianAnswer.Round(time.Minute))
	if len(m.Merged) > 0 {
		fmt.Fprintf(&b, ", %s per merged change", m.PerMerge.Round(time.Minute))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "merged: %d", len(m.Merged))
	for _, p := range m.Merged {
		if p.Number == 0 {
			what := p.Branch
			if what == "" {
				what = p.Commit[:min(7, len(p.Commit))]
			}
			fmt.Fprintf(&b, " %s %s, no pull request (%s)", filepath.Base(p.Repo), what, strings.Join(p.Sessions, ", "))
			continue
		}
		fmt.Fprintf(&b, " %s#%d (%s)", p.Repo, p.Number, strings.Join(p.Sessions, ", "))
	}
	if m.Unread > 0 {
		fmt.Fprintf(&b, "; %d session(s) whose progress could not be read, so this may be short", m.Unread)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "sends: %d — %d the owner's snippets, %d slash commands typed, %d the driver's own words; %d retyped after a lost start, %d left garbled, %d received as something else; %d with delivery unknown\n",
		m.Sends, m.Snippets, m.Commands, m.OwnWords, m.Recovered, m.Garbled, m.Other, m.Unknown)
	if len(m.ByKey) > 0 {
		var keys []string
		for k, n := range m.ByKey {
			keys = append(keys, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "snippets: %s\n", strings.Join(keys, ", "))
	}
	fmt.Fprintf(&b, "wakes: %d posted for sessions and %d for timers, %d failed; %d followed by no action on the session before its next wake or the batch's end\n",
		m.Wakes, m.Timers, m.WakeFails, m.Unhandled)
	fmt.Fprintf(&b, "turns left unwatched: %d (a send or respond that no wake, park or close of its session followed)\n", m.Unwatched)
	if len(m.Refusals) > 0 {
		var rs []string
		for k, n := range m.Refusals {
			rs = append(rs, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(rs)
		fmt.Fprintf(&b, "refused: %s\n", strings.Join(rs, ", "))
	}
	for _, f := range m.Failures {
		fmt.Fprintf(&b, "failure: %s\n", f)
	}
	return b.String()
}

// Text is what batch new prints.
func (st *Started) Text() string {
	s := fmt.Sprintf("batch %s started and current (%s)\n", st.Batch.Meta.Name, st.Batch.Dir)
	if st.RexStarted {
		s += fmt.Sprintf("started Rex session %s for its sessions\n", st.Batch.Meta.RexSession)
	}
	if st.Stopped > 0 {
		s += fmt.Sprintf("stopped %d watches of batch %s\n", st.Stopped, st.Prev.Meta.Name)
	}
	return s
}

// BatchText is what batch prints of the batch it acts in.
func BatchText(b *batch.Batch) string {
	evs, _ := b.Events()
	return fmt.Sprintf("batch %s, started %s · wakes: %s · Rex session %s · zone %s · %d sessions · %s\n", b.Meta.Name,
		b.Meta.Started.In(b.Zone()).Format("2006-01-02 15:04 MST"), b.Meta.Wake, orDash(b.Meta.RexSession),
		orDash(b.Meta.TZ), len(batch.Members(evs)), b.Dir)
}

// BatchesText is what batch list prints, the current one starred.
func BatchesText(metas []batch.Meta, current string) string {
	var b strings.Builder
	for _, m := range metas {
		mark := " "
		if m.Name == current {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s %s  started %s  wakes: %s\n", mark, m.Name, m.Started.Local().Format("2006-01-02 15:04"), m.Wake)
	}
	return b.String()
}
