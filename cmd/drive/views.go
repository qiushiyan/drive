package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qiushiyan/drive/internal/drive"
)

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
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
	if a.on("json") {
		printJSON(st.JSON())
		return 0
	}
	fmt.Print(st.Text(clockNow(d)))
	return 0
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
		printJSON(r.JSON())
		return 0
	}
	fmt.Print(r.Text())
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
		fmt.Printf("%s woke the driver for %s (%s)\n", clockNow(d).Format(time.RFC3339), e.Label, e.State)
	}
	if a.on("json") {
		printJSON(e.JSON())
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
	fmt.Print(drive.LogText(b, evs, clockNow(d)))
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
	fmt.Print(m.Text())
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
		fmt.Print(drive.BatchText(b))
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
				return bad(fmt.Sprintf("--tz %q is not a zone name such as Europe/London", tz))
			}
		}
		var after []string
		if c := a.str("after-wake"); c != "" {
			after = []string{c}
		}
		st, err := d.StartBatch(drive.BatchOptions{Name: a.pos[1], Wake: wake, Webhook: a.str("webhook"),
			RexSession: a.str("rex-session"), TZ: a.str("tz"), AfterWake: after})
		if err != nil {
			return fail(err)
		}
		fmt.Print(st.Text())
	case a.pos[0] == "use" && len(a.pos) == 2:
		if err := d.UseBatch(a.pos[1]); err != nil {
			return fail(err)
		}
		fmt.Printf("batch %s is current\n", a.pos[1])
	case a.pos[0] == "list":
		metas, cur, err := d.Batches()
		if err != nil {
			return fail(err)
		}
		fmt.Print(drive.BatchesText(metas, cur))
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
		from, keys, err := d.Snippets(dir)
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
	s, err := d.Snippet(dir, a.pos[0], strings.Join(a.pos[min(2, len(a.pos)):], " "))
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "from: %s\n", s.From)
	fmt.Println(s.Text)
	return 0
}
