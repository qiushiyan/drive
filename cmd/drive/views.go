package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/snippet"
	"github.com/qiushiyan/drive/internal/turn"
)

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

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

func prText(r drive.Row) string {
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

func cmdStatus(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, []string{"json", "quick"})
	if a == nil {
		return code
	}
	st, err := d.Status(!a.on("quick"))
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	if a.on("json") {
		rows := func(rs []drive.Row) []map[string]any {
			var out []map[string]any
			for _, r := range rs {
				m := map[string]any{"label": r.Label, "sid": r.SID, "block": rex.Short(r.Block), "window": r.Window, "state": r.State,
					"for_seconds": int(r.For.Seconds()), "account": r.Account, "input": r.Input, "stage": r.Stage,
					"watch": r.Watch, "unhandled_wake": r.Unhandled, "wake_due": r.Due, "last_sent": r.LastSent, "error": r.Error,
					"jobs": r.Jobs}
				if r.View.Session != "" {
					m["view"] = r.View.String()
				}
				if r.Input == "typed" || r.Input == "suggestion" {
					m["input_text"] = r.Typed
				}
				if r.PR != nil {
					m["pr"] = r.PR
				}
				if r.Name != "" {
					m["name"] = r.Name
				}
				out = append(out, m)
			}
			return out
		}
		printJSON(map[string]any{"batch": st.Batch, "sessions": rows(st.Rows), "closed": st.Closed, "others": rows(st.Others),
			"holds": st.Holds, "open_asks": st.Asks, "grants": st.Grants, "in_flight": st.InFlight})
		return 0
	}
	if st.Batch == nil {
		fmt.Println("no current batch (`drive batch new NAME` starts one); Claude sessions in Rex:")
	} else {
		fmt.Printf("batch %s · wakes: %s · Rex session %s · times %s (%s)\n", st.Batch.Name, st.Batch.Wake,
			orDash(st.Batch.RexSession), st.Zone, now.In(st.Zone).Format("MST, UTC-07:00"))
	}
	if len(st.Rows) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SESSION\tWIN\tBLOCK\tSTATE\tFOR\tINPUT\tSTAGE\tPR\tLAST SENT\tWAKE\tVIEW")
		for _, r := range st.Rows {
			if r.Error != "" {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\t\t\t\t%s\t%s\t%s\n", r.Label, orDash(r.Window), rex.Short(r.Block), r.State, r.LastSent, r.Watch, r.Error)
				continue
			}
			wake := r.Watch
			sent := r.LastSent
			if !r.SentAt.IsZero() {
				sent += " " + clock(r.SentAt, st.Zone, now)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Label, orDash(r.Window), rex.Short(r.Block), r.State,
				ago(r.For), r.Input, r.Stage, prText(r), sent, wake, r.View)
		}
		w.Flush()
	} else if st.Batch != nil && st.Closed > 0 {
		fmt.Printf("no open sessions in this batch (%d closed)\n", st.Closed)
	} else if st.Batch != nil {
		fmt.Println("no sessions in this batch yet")
	}
	for _, r := range st.Rows {
		if r.Input == "typed" {
			fmt.Printf("%s's input box holds typed text nobody sent: %q\n", r.Label, r.Typed)
		}
		for _, j := range r.Jobs {
			fmt.Printf("%s runs a background job since %s: %s\n", r.Label, clock(j.Started, st.Zone, now), clip(j.Command, 100))
		}
	}
	if len(st.Others) > 0 {
		fmt.Println("\nother Claude sessions in Rex:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, r := range st.Others {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", orDash(r.Window), r.Label, r.State, ago(r.For), r.Name)
		}
		w.Flush()
	}
	for _, h := range st.Holds {
		fmt.Println("hold " + h)
	}
	for _, r := range st.Rows {
		if strings.HasPrefix(r.Watch, "ended without a wake") && st.Batch != nil {
			fmt.Printf("what each watch printed: %s\n", st.Dir+"/watches.log")
			break
		}
	}
	for _, p := range st.InFlight {
		if p.Driver != nil {
			fmt.Printf("in flight: %s to %s since %s, by pid %d (%s)\n", p.Kind, p.Session, clock(p.At, st.Zone, now), p.Driver.PID, clip(p.Text, 60))
		} else {
			fmt.Printf("left unfinished: %s to %s since %s, by a driver that has gone (%s); the next send, respond, park or close on %s settles it\n",
				p.Kind, p.Session, clock(p.At, st.Zone, now), clip(p.Text, 60), p.Session)
		}
	}
	for _, g := range st.Grants {
		fmt.Printf("grant %s (%s): %s — %s\n", g.ID, clock(g.At, st.Zone, now), g.Scope, g.Source)
	}
	for _, q := range st.Asks {
		fmt.Printf("open ask %s for %s, %s: %s — unanswered: %s\n", q.ID, orDash(q.Session), ago(now.Sub(q.At)), q.Question, q.IfUnanswered)
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmdRead(d *drive.Driver, argv []string) int {
	// --prompts takes an optional count: "--prompts 5" or "--prompts".
	n := 10
	for i, x := range argv {
		if x == "--prompts" && i+1 < len(argv) {
			if v, err := strconv.Atoi(argv[i+1]); err == nil {
				n = v
				argv = append(append([]string{}, argv[:i+1]...), argv[i+2:]...)
				break
			}
		}
	}
	a, code := parseOr(argv, nil, []string{"prompts", "pending", "user", "screen", "json"})
	if a == nil {
		return code
	}
	if len(a.pos) != 1 {
		return bad("read needs one SESSION")
	}
	mode := drive.ReadLast
	switch {
	case a.on("prompts"):
		mode = drive.ReadPrompts
	case a.on("pending"):
		mode = drive.ReadPending
	case a.on("user"):
		mode = drive.ReadUser
	case a.on("screen"):
		mode = drive.ReadScreen
	}
	r, err := d.Read(a.pos[0], mode, n)
	if err != nil {
		return fail(err)
	}
	if a.on("json") {
		printJSON(map[string]any{"session": r.Label, "sid": r.SID, "state": r.State, "account": r.Account, "input": r.Input,
			"input_text": r.Typed, "text": r.Text, "at": r.At, "view": viewOrEmpty(r.View), "next": r.Next, "jobs": r.Jobs})
		return 0
	}
	fmt.Println(r.Text)
	fmt.Println()
	line := fmt.Sprintf("%s is %s on account %s (session %s)", r.Label, r.State, orDash(r.Account), orDash(r.SID))
	switch r.Input {
	case "typed":
		line += fmt.Sprintf("; its input box holds typed text nobody sent: %q", r.Typed)
	case "suggestion":
		line += "; its input box shows only Claude Code's suggestion"
	}
	for _, j := range r.Jobs {
		line += "; a background job of its runs: " + clip(j.Command, 100)
	}
	fmt.Println(line)
	if r.View.Session != "" { // a closed or gone session has nothing to act on
		fmt.Printf("view: %s\n", r.View)
	}
	if r.Next != "" {
		fmt.Printf("next: %s\n", r.Next)
	}
	return 0
}

func cmdWait(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"quiet", "bg", "sleep", "note"}, []string{"notify", "json"})
	if a == nil {
		return code
	}
	// A watch drive started carries its name; a signal from anyone but drive
	// is recorded, so a watch never ends without a word.
	watch := os.Getenv("DRIVE_WATCH")
	if a.on("notify") && watch != "" {
		label := "timer " + a.str("note")
		if len(a.pos) == 1 {
			label = a.pos[0]
		}
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
		go func() {
			sig := <-sigs
			if d.Killed(watch, label, sig) {
				os.Exit(3)
			}
			os.Exit(0)
		}()
	}
	if s := a.str("sleep"); s != "" {
		after, err := seconds(s, 0)
		if err != nil {
			return bad(err.Error())
		}
		time.Sleep(after)
		if a.on("notify") {
			if err := d.Ring(watch, a.str("note")); err != nil {
				return fail(err)
			}
		}
		fmt.Printf("timer done: %s\n", a.str("note"))
		return 0
	}
	if len(a.pos) != 1 {
		return bad("wait needs one SESSION")
	}
	quiet, err := seconds(a.str("quiet"), time.Minute)
	if err != nil {
		return bad(err.Error())
	}
	bg, err := seconds(a.str("bg"), 10*time.Minute)
	if err != nil {
		return bad(err.Error())
	}
	var e *drive.Ending
	if a.on("notify") {
		e, err = d.Notify(watch, a.pos[0], quiet, bg)
	} else {
		e, err = d.Wait(a.pos[0], quiet, bg)
	}
	if err != nil {
		return fail(err)
	}
	if a.on("notify") {
		fmt.Printf("%s woke the driver for %s (%s)\n", time.Now().Format(time.RFC3339), e.Label, e.State)
	}
	if a.on("json") {
		printJSON(map[string]any{"session": e.Label, "block": rex.Short(e.Block), "state": e.State, "error": e.Error,
			"shells": e.Shells, "orphans": e.Orphans, "jobs": e.Jobs, "reply": e.Reply, "screen": e.Screen, "view": e.View.String(), "next": e.Next})
		return 0
	}
	fmt.Print(e.Text())
	return 0
}

func cmdRecord(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"source", "scope", "question", "recommendation", "if-unanswered", "took", "session"}, []string{"explained"})
	if a == nil {
		return code
	}
	if len(a.pos) == 0 {
		return bad("record needs a kind: grant, ask, answer or note")
	}
	var id string
	var err error
	switch a.pos[0] {
	case "grant":
		id, err = d.Grant(a.str("source"), a.str("scope"))
	case "ask":
		if len(a.pos) != 2 {
			return bad("record ask needs the SESSION it holds (\"-\" for the batch)")
		}
		id, err = d.Ask(strings.TrimPrefix(a.pos[1], "-"), a.str("question"), a.str("recommendation"), a.str("if-unanswered"))
	case "answer":
		if len(a.pos) != 2 {
			return bad("record answer needs the ASK it answers, such as A3")
		}
		id, err = d.Answer(a.pos[1], a.str("source"), a.str("took"), a.on("explained"))
	case "note":
		id, err = d.Note(a.str("session"), strings.Join(a.pos[1:], " "))
	default:
		return bad(fmt.Sprintf("record %q: grant, ask, answer or note", a.pos[0]))
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("recorded %s %s\n", a.pos[0], id)
	return 0
}

func cmdLog(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"since"}, []string{"json"})
	if a == nil {
		return code
	}
	var since time.Time
	if s := a.str("since"); s != "" {
		// Read in the zone drive prints times in, so a time copied from its output means what it said.
		zone := time.Local
		if b, err := d.Batch(); err == nil {
			zone = b.Zone()
		}
		var err error
		if since, err = parseWhen(s, zone); err != nil {
			return bad(err.Error())
		}
	}
	b, evs, err := d.Log(since)
	if err != nil {
		return fail(err)
	}
	if a.on("json") {
		for _, e := range evs {
			line, _ := json.Marshal(e)
			fmt.Println(string(line))
		}
		return 0
	}
	zone, now := b.Zone(), time.Now()
	for _, e := range evs {
		fmt.Printf("%s  %-12s %-8s %s\n", clock(e.At, zone, now), e.Kind, e.Session, describe(e))
	}
	return 0
}

func parseWhen(s string, zone *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, zone); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: give a date or a date and time, such as \"2026-10-22 16:00\"", s)
}

func describe(e batch.Event) string {
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

func cmdMetrics(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, []string{"json"})
	if a == nil {
		return code
	}
	name := ""
	if len(a.pos) > 0 {
		name = a.pos[0]
	}
	m, err := d.Metrics(name)
	if err != nil {
		return fail(err)
	}
	if a.on("json") {
		printJSON(m)
		return 0
	}
	fmt.Printf("batch %s, %s → %s, %d sessions\n", m.Batch, m.From.In(m.Zone).Format("01-02 15:04"), m.To.In(m.Zone).Format("01-02 15:04 MST"), m.Sessions)
	fmt.Printf("asks: %d (%d answered, %d open); took the recommendation %d, amended %d, delegated %d, moot %d; needed explaining %d\n",
		m.Asks, m.Answered, m.Unanswered, m.Took["recommended"], m.Took["amended"], m.Took["delegated"], m.Took["moot"], m.Explained)
	fmt.Printf("owner wait: %s in all, median answer %s", m.OwnerWait.Round(time.Minute), m.MedianAnswer.Round(time.Minute))
	if len(m.Merged) > 0 {
		fmt.Printf(", %s per merged change", m.PerMerge.Round(time.Minute))
	}
	fmt.Println()
	fmt.Printf("merged: %d", len(m.Merged))
	for _, p := range m.Merged {
		if p.Branch != "" {
			fmt.Printf(" %s %s, no pull request (%s)", filepath.Base(p.Repo), p.Branch, strings.Join(p.Sessions, ", "))
			continue
		}
		fmt.Printf(" %s#%d (%s)", p.Repo, p.Number, strings.Join(p.Sessions, ", "))
	}
	if m.Unread > 0 {
		fmt.Printf("; %d session(s) whose progress could not be read, so this may be short", m.Unread)
	}
	fmt.Println()
	fmt.Printf("sends: %d — %d the owner's snippets, %d slash commands typed, %d the driver's own words; %d retyped after a lost start, %d left garbled, %d received as something else; %d with delivery unknown\n",
		m.Sends, m.Snippets, m.Commands, m.OwnWords, m.Recovered, m.Garbled, m.Other, m.Unknown)
	if len(m.ByKey) > 0 {
		var keys []string
		for k, n := range m.ByKey {
			keys = append(keys, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(keys)
		fmt.Printf("snippets: %s\n", strings.Join(keys, ", "))
	}
	fmt.Printf("wakes: %d posted for sessions and %d for timers, %d failed; %d followed by no action on the session before its next wake or the batch's end\n",
		m.Wakes, m.Timers, m.WakeFails, m.Unhandled)
	fmt.Printf("turns left unwatched: %d (a send or respond that no wake, park or close of its session followed)\n", m.Unwatched)
	if len(m.Refusals) > 0 {
		var rs []string
		for k, n := range m.Refusals {
			rs = append(rs, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(rs)
		fmt.Printf("refused: %s\n", strings.Join(rs, ", "))
	}
	for _, f := range m.Failures {
		fmt.Printf("failure: %s\n", f)
	}
	return 0
}

func cmdBatch(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, []string{"wake", "webhook", "rex-session", "tz", "after-wake"}, nil)
	if a == nil {
		return code
	}
	switch {
	case len(a.pos) == 0:
		b, err := d.Batch()
		if err != nil {
			return fail(err)
		}
		evs, _ := b.Events()
		fmt.Printf("batch %s, started %s · wakes: %s · Rex session %s · zone %s · %d sessions · %s\n", b.Meta.Name,
			b.Meta.Started.In(b.Zone()).Format("2006-01-02 15:04 MST"), b.Meta.Wake, orDash(b.Meta.RexSession),
			orDash(b.Meta.TZ), len(batch.Members(evs)), b.Dir)
	case a.pos[0] == "new" && len(a.pos) == 2:
		wake := a.str("wake")
		switch {
		case wake == "" && a.str("webhook") != "":
			wake = "webhook"
		case wake == "":
			wake = "none"
		case wake == "none" && a.str("webhook") != "":
			return bad("--webhook wakes a chat driver, and --wake none wakes no one: give one of them")
		case wake != "webhook" && wake != "none":
			return bad("--wake is webhook (a chat driver woken by its webhook) or none (a driver that waits itself)")
		}
		tz := a.str("tz")
		if tz == "" {
			tz = d.Cfg.TZ
		}
		if tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return bad(fmt.Sprintf("--tz %q is not a zone name such as Asia/Shanghai", tz))
			}
		}
		rs := a.str("rex-session")
		if rs == "" {
			rs = d.Cfg.RexSession
		}
		if rs == "" {
			rs = os.Getenv("REX_SESSION")
		}
		var after []string
		if c := a.str("after-wake"); c != "" {
			after = []string{c}
		}
		again := "drive batch"
		for _, w := range argv {
			again += " " + shellWord(w)
		}
		st, err := d.StartBatch(batch.Meta{Name: a.pos[1], Started: time.Now(), Wake: wake, Webhook: a.str("webhook"),
			RexSession: rs, TZ: tz, AfterWake: after}, again)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("batch %s started and current (%s)\n", st.Batch.Meta.Name, st.Batch.Dir)
		if st.RexStarted {
			fmt.Printf("started Rex session %s for its sessions\n", rs)
		}
		if st.Stopped > 0 {
			fmt.Printf("stopped %d watches of batch %s\n", st.Stopped, st.Prev.Meta.Name)
		}
	case a.pos[0] == "use" && len(a.pos) == 2:
		if err := d.Store.Use(a.pos[1]); err != nil {
			return fail(&drive.Refused{Reason: err.Error()})
		}
		fmt.Printf("batch %s is current\n", a.pos[1])
	case a.pos[0] == "list":
		metas, err := d.Store.List()
		if err != nil {
			return fail(err)
		}
		cur, _ := d.Store.Current()
		for _, m := range metas {
			mark := " "
			if cur != nil && cur.Meta.Name == m.Name {
				mark = "*"
			}
			fmt.Printf("%s %s  started %s  wakes: %s\n", mark, m.Name, m.Started.Local().Format("2006-01-02 15:04"), m.Wake)
		}
	default:
		return bad("batch takes: nothing, new NAME, use NAME, or list")
	}
	return 0
}

func cmdSnippet(d *drive.Driver, argv []string) int {
	a, code := parseOr(argv, nil, []string{"list"})
	if a == nil {
		return code
	}
	if a.on("list") {
		dir := "."
		if len(a.pos) > 0 {
			dir = a.pos[0]
		}
		from, keys, err := snippet.List(d.Cfg.Home, dir)
		if err != nil {
			return fail(err)
		}
		for _, k := range keys {
			fmt.Printf("%s\t%s\n", k, from[k])
		}
		return 0
	}
	if len(a.pos) == 0 {
		return bad("snippet needs a KEY, or --list")
	}
	dir := "."
	if len(a.pos) > 1 {
		dir = a.pos[1]
	}
	s, err := snippet.Expand(d.Cfg.Home, dir, a.pos[0], strings.Join(a.pos[min(2, len(a.pos)):], " "))
	if err != nil {
		return fail(&drive.Refused{Reason: err.Error()})
	}
	fmt.Fprintf(os.Stderr, "from: %s\n", s.From)
	fmt.Println(s.Text)
	return 0
}

// viewOrEmpty is a view as JSON gives it: "" for a closed or gone session, which has none.
func viewOrEmpty(v turn.View) string {
	if v.Session == "" {
		return ""
	}
	return v.String()
}

// shellWord quotes an argument so a command printed for a driver runs as written.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@._-/=:") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
