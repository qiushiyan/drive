package drive_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/testkit"
)

// TestMain points HOME and drive's directories at a sandbox, so a path
// built from the real home by mistake lands there and not in live state.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "drive-test-home")
	os.Setenv("HOME", dir)
	os.Setenv("DRIVE_STATE_DIR", filepath.Join(dir, "state"))
	os.Setenv("DRIVE_CONFIG_DIR", filepath.Join(dir, "config"))
	code := m.Run()
	if strings.HasPrefix(dir, os.TempDir()) {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

const noticeText = "I am away; another session sends your next steps."

type rig struct {
	t       *testing.T
	w       *testkit.World
	d       *drive.Driver
	watches *testkit.Watches
	b       *batch.Batch
}

func newRig(t *testing.T, wake string) *rig {
	t.Helper()
	w := testkit.New(t)
	home := filepath.Join(w.Root, "home")
	notice := filepath.Join(home, "NOTICE.md")
	_ = os.MkdirAll(home, 0o755)
	if err := os.WriteFile(notice, []byte(noticeText+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := batch.Store{Dir: filepath.Join(w.Root, "state")}
	b, _, err := store.New(batch.Meta{Name: "b1", Started: w.Clock.Now(), Wake: wake})
	if err != nil {
		t.Fatal(err)
	}
	watches := &testkit.Watches{}
	d := &drive.Driver{Term: w, Host: w, Store: store, Watches: watches, Self: "/bin/drive",
		Cfg: config.Config{Home: home, StateDir: store.Dir, Notice: notice, Launcher: "x"},
		Now: w.Clock.Now, Sleep: w.Clock.Sleep, Poll: 2 * time.Second}
	if wake == "webhook" {
		// A batch that wakes its driver has somewhere to post; a test that reads the posts brings its own server.
		d.Cfg.Webhook = config.Webhook{URL: "http://127.0.0.1:9/unused"}
	}
	return &rig{t: t, w: w, d: d, watches: watches, b: b}
}

func (r *rig) view(ref string) string {
	r.t.Helper()
	rd, err := r.d.Read(ref, drive.ReadLast, 10)
	if err != nil {
		r.t.Fatalf("read %s: %v", ref, err)
	}
	return rd.View.String()
}

func (r *rig) events(kind string) []batch.Event {
	evs, err := r.b.Events()
	if err != nil {
		r.t.Fatal(err)
	}
	var out []batch.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// done puts a session at the end of a finished turn, as a driver finds it.
func done(c *testkit.Claude, prompt, reply string) {
	c.Busy(prompt)
	c.Done(reply)
}

func isRefused(err error) bool {
	var r *drive.Refused
	return errors.As(err, &r)
}

// Two drivers decided on one view: the second arrives while the first is
// typing and is refused by the lock, not by the view the first will move.
func TestTwoDriversOneView(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	v := r.view("T1")
	gate := make(chan struct{})
	c.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
	first := make(chan error, 1)
	go func() {
		_, err := r.d.Send(v, drive.Step{Line: "step from A"}, drive.SendOptions{})
		first <- err
	}()
	admitted(r, "T1", "step from A")
	second := make(chan error, 1)
	go func() {
		_, err := r.d.Send(v, drive.Step{Line: "step from B"}, drive.SendOptions{})
		second <- err
	}()
	var err error
	select {
	case err = <-second:
	case <-time.After(5 * time.Second):
		err = errors.New("the second driver went on to type while the first held the session")
	}
	close(gate)
	if ferr := <-first; ferr != nil {
		t.Fatalf("the first goes: %v", ferr)
	}
	if !isRefused(err) || !strings.Contains(err.Error(), "your own drive run is sending T1") {
		t.Fatalf("the second is refused while the first holds the session: %v", err)
	}
	if typed, _ := c.Seen(); contains(typed, "step from B") {
		t.Fatalf("only the first reaches the session: %q", typed)
	}
}

func TestWorkingSessionNeedsQueue(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	ref := c.Block[len(c.Block)-6:]
	// Adopt it with one send, then put it to work.
	if _, err := r.d.Send(r.view(ref), drive.Step{Line: "build"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	c.Busy("next")
	v := r.view("T1")
	if _, err := r.d.Send(v, drive.Step{Line: "also run fable"}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "--queue") {
		t.Fatalf("a working session needs --queue, got %v", err)
	}
	// While it works it streams more; a queued send needs only no new prompt.
	c.Say("still working")
	sent, err := r.d.Send(v, drive.Step{Line: "also run fable"}, drive.SendOptions{Queue: true})
	if err != nil || sent.Outcome != "queued" {
		t.Fatalf("queued send: %+v %v", sent, err)
	}
}

// Claude Code queues a slash command typed while busy without its text; the
// old send-turn reported that as "arrived differently" (7 of 7 in a batch).
func TestQueuedSlashCommandIsQueued(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "decisions"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	c.Busy("work")
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "/write-spec Go with D1"}, drive.SendOptions{Queue: true})
	if err != nil || sent.Outcome != "queued" {
		t.Fatalf("a queued slash command is queued: %+v %v", sent, err)
	}
}

func TestFirstSendCarriesNoticeOnce(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	step := filepath.Join(r.w.Root, "step.md")
	_ = os.WriteFile(step, []byte("Build it end to end.\n"), 0o644)
	sent, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{File: step}, drive.SendOptions{Label: "T1"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(sent.Path)
	if !sent.Notice || !strings.HasPrefix(string(body), noticeText) || !strings.Contains(string(body), "Build it end to end.") {
		t.Fatalf("the first step carries the notice ahead of it: %q", body)
	}
	if sent.Typed != "@"+sent.Path || sent.Outcome != "arrived" {
		t.Fatalf("a file step is typed as its copy's @-mention, which Claude Code attaches: %+v", sent)
	}
	if typed, keys := c.Seen(); len(typed) != 1 || typed[0] != "@"+sent.Path+" " || len(keys) != 1 {
		t.Fatalf("a space closes the file completion, so one Enter sends: %q %q", typed, keys)
	}
	sent2, err := r.d.Send(r.view("T1"), drive.Step{File: step}, drive.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(sent2.Path)
	if sent2.Notice || strings.Contains(string(body2), noticeText) || sent2.Path == sent.Path {
		t.Fatalf("later steps go without the notice, each in its own file: %q", body2)
	}
	if err := os.WriteFile(sent.Path, []byte("changed"), 0o644); err == nil {
		t.Fatal("a sent file stays as it was sent")
	}
}

// Sent alone ahead of a typed step, the notice was a turn of its own: on
// 10-24 to 10-28 all 13 such turns ended on "the step isn't here", and 12
// went looking through the batch's files for it. It rides the step's line
// as an @-mention instead, after a skill's arguments too, which Claude Code
// attaches as it does in a prompt. A command no skill file backs may read
// its arguments itself, such as /model, and a line the mention would
// take past what Claude Code keeps of typing, keep the turn of their own.
func TestTypedFirstStepCarriesTheNotice(t *testing.T) {
	r := newRig(t, "none")
	spec := filepath.Join(r.d.Cfg.Home, ".claude", "skills", "write-spec")
	_ = os.MkdirAll(spec, 0o755)
	_ = os.WriteFile(filepath.Join(spec, "SKILL.md"), []byte("---\nname: write-spec\ndisable-model-invocation: true\n---\n"), 0o644)
	long := strings.Repeat("go on ", 80)
	for _, c := range []struct {
		line  string
		alone bool
	}{
		{"Go with D1 and D2.", false},
		{"/write-spec Go with D1", false},
		{"/model sonnet", true},
		{long, true},
	} {
		s := r.w.Add("T1")
		done(s, "onboard", "analysis")
		sent, err := r.d.Send(r.view(s.Block[len(s.Block)-6:]), drive.Step{Line: c.line}, drive.SendOptions{Label: "S" + s.Block[len(s.Block)-3:]})
		if err != nil || !sent.Notice {
			t.Fatalf("%.20q: %+v %v", c.line, sent, err)
		}
		typed, _ := s.Seen()
		line := strings.TrimSpace(c.line)
		switch {
		case c.alone && (len(typed) != 2 || !strings.HasPrefix(typed[0], "@") || typed[1] != line):
			t.Errorf("%.20q: the notice goes first as a file, then the line alone: %q", c.line, typed)
		case !c.alone && (len(typed) != 1 || !strings.HasPrefix(typed[0], line+" @") || !strings.HasSuffix(typed[0], ".md ")):
			t.Errorf("%.20q: the notice rides the line as an @-mention: %q", c.line, typed)
		case !c.alone:
			notice, _ := os.ReadFile(strings.TrimSpace(strings.TrimPrefix(typed[0], line+" @")))
			if !strings.HasPrefix(string(notice), noticeText) || sent.Typed != strings.TrimSpace(typed[0]) {
				t.Errorf("%.20q: the mention is the notice's copy: %q, typed %q", c.line, notice, sent.Typed)
			}
		}
	}
}

// A mention typed while the session works is taken as words, with no file
// attached, so a step sent mid-turn asks for its file to be read.
func TestAQueuedFileStepAsksForItsFileToBeRead(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "decisions"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	c.Busy("work")
	step := filepath.Join(r.w.Root, "step.md")
	_ = os.WriteFile(step, []byte("Build it end to end.\n"), 0o644)
	sent, err := r.d.Send(r.view("T1"), drive.Step{File: step}, drive.SendOptions{Queue: true})
	if err != nil || sent.Typed != "Read "+sent.Path+" and do what it asks." {
		t.Fatalf("a queued file step is the sentence claude-steps matches: %+v %v", sent, err)
	}
}

// T4 and R0 on 10-21: the first prompt arrived and the session answered
// "Login expired"; the old path logged it "arrived" for 53 minutes.
func TestLoginRejectedOnFirstStep(t *testing.T) {
	r := newRig(t, "webhook")
	r.w.OnOpen = func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) {
			c.Fail("authentication_failed", "Login expired · Please run /login")
			c.Set("blocked", "auth", "Login expired · Please run /login")
		}
	}
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "other@example.com", "sessions"), 0o755)
	o, err := r.d.Open("T4", drive.Step{Line: "onboard this"}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work")})
	var f *drive.Failed
	if !errors.As(err, &f) || !f.Login || !strings.Contains(f.Error(), "Login expired") || !strings.Contains(f.Next, "--account other@example.com") ||
		!strings.Contains(f.Next, "--dir "+filepath.Join(r.w.Root, "work")) {
		t.Fatalf("a rejected login is a login failure with the relaunch to run, got %v", err)
	}
	if o == nil || o.Label != "T4" {
		t.Fatalf("the open itself is reported: %+v", o)
	}
	if n := len(r.events(batch.KindFailure)); n != 1 {
		t.Fatalf("the failure is recorded, got %d", n)
	}
	if _, err := r.d.Send(r.view("T4"), drive.Step{Line: "again"}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "login was rejected") {
		t.Fatalf("a session whose login was rejected takes no more steps, got %v", err)
	}
}

func TestOpenRefusesALabelAlreadyOpen(t *testing.T) {
	r := newRig(t, "none")
	dir := filepath.Join(r.w.Root, "work")
	o, err := r.d.Open("sr-9531", drive.Step{Line: "/strategic-review 9531"}, drive.OpenOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if o.Sent == nil || o.Sent.Outcome != "arrived" {
		t.Fatalf("the first step arrived: %+v", o.Sent)
	}
	if _, err := r.d.Open("sr-9531", drive.Step{Line: "/strategic-review 9531"}, drive.OpenOptions{Dir: dir}); !isRefused(err) || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("a second open of one label is refused, got %v", err)
	}
	blocks, _ := r.w.Blocks("")
	if len(blocks) != 1 {
		t.Fatalf("one session, not two: %d blocks", len(blocks))
	}
}

// A goal is what the owner reads a session was for. It is checked whole
// before anything opens, stands on the member and its status row once the
// block opens, can be recorded later for a session drive did not open or
// one since closed, and rides along on any open a next: gives.
func TestAGoalIsRecordedWithTheOpen(t *testing.T) {
	r := newRig(t, "none")
	dir := filepath.Join(r.w.Root, "work")
	for _, bad := range []string{"", "  \n ", "…", "stop\x07here", strings.Repeat("long ", 121)} {
		if _, err := r.d.Open("S1", drive.Step{Line: "onboard"}, drive.OpenOptions{Dir: dir, Goal: &bad}); !isRefused(err) {
			t.Fatalf("goal %q is refused, got %v", bad, err)
		}
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 0 || len(r.events(batch.KindOpenIntent)) != 0 {
		t.Fatalf("a refused goal opens nothing and records nothing: %d blocks", len(blocks))
	}
	goal := "Make Loopy's summary keep\n  settled facts."
	if _, err := r.d.Open("S1", drive.Step{Line: "onboard"}, drive.OpenOptions{Dir: dir, Goal: &goal}); err != nil {
		t.Fatal(err)
	}
	st, err := r.d.Status(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Rows) != 1 || st.Rows[0].Goal != "Make Loopy's summary keep settled facts." {
		t.Fatalf("the goal, one paragraph, is on the row: %+v", st.Rows)
	}
	if doc, _ := json.Marshal(st.JSON()); !strings.Contains(string(doc), `"goal":"Make Loopy's summary keep settled facts."`) {
		t.Fatalf("status --json carries it: %s", doc)
	}
	rd, err := r.d.Read("S1", drive.ReadLast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := json.Marshal(rd.JSON()); !strings.Contains(rd.Text(), "goal: Make Loopy's summary keep settled facts.\n") || !strings.Contains(string(doc), `"goal":"Make Loopy's summary keep settled facts."`) {
		t.Fatalf("read prints it, as text and --json, so the driver tells the owner the goal the page shows: %s", doc)
	}

	// A session that joined on a send, and one since closed, take one later.
	c := r.w.Add("P")
	done(c, "x", "y")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go on"}, drive.SendOptions{Label: "P0"}); err != nil {
		t.Fatal(err)
	}
	r.w.Session(c.Block).Done("ok")
	if err := r.d.Close(r.view("P0")); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Goal("P0", "Retire the old run-plan fields."); err != nil {
		t.Fatalf("a closed session takes a goal: %v", err)
	}
	if err := r.d.Goal("S9", "Never here."); !isRefused(err) || !strings.Contains(err.Error(), "never been in batch") {
		t.Fatalf("a label the batch never had is refused: %v", err)
	}
	evs, _ := r.b.Events()
	if m := batch.Find(batch.Members(evs), "P0"); m == nil || m.Goal != "Retire the old run-plan fields." {
		t.Fatalf("the recorded goal is the member's: %+v", m)
	}

	// An open refused on a hold gives the open to run once it is free, goal and all.
	a := r.w.Add("A")
	done(a, "x", "y")
	if _, err := r.d.Send(r.view(a.Block[len(a.Block)-6:]), drive.Step{Line: "land it"}, drive.SendOptions{Label: "T3"}); err != nil {
		t.Fatal(err)
	}
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the release runs on
	if _, err := r.d.Send(r.view("T3"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	waiting := "-Ship the\n  release notes." // as one paragraph, so the next: stays one line
	_, err = r.d.Open("S2", drive.Step{Line: "write them"}, drive.OpenOptions{Dir: dir, Hold: "release", Goal: &waiting})
	var refused *drive.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Next, "--goal=-Ship the release notes.") {
		t.Fatalf("the open in next: keeps its goal as one word: %v", err)
	}
}

// A driver that ended after Rex opened its block, before it recorded the
// open, leaves the goal on its intent; the next driver to own the label
// settles the open from the block, and the goal stands with it.
func TestAGoalStandsWhenAnOpenIsSettled(t *testing.T) {
	r := newRig(t, "none")
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindOpenIntent, Session: "S3", Intent: "gone",
		Dir: filepath.Join(r.w.Root, "work"), Goal: "Give each saved schedule edit a receipt."})
	r.w.Add("S3") // the block Rex opened under its label
	if _, err := r.d.Open("S3", drive.Step{Line: "onboard"}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work")}); !isRefused(err) || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("the settled open is the session, already open: %v", err)
	}
	evs, _ := r.b.Events()
	if m := batch.Find(batch.Members(evs), "S3"); m == nil || m.Goal != "Give each saved schedule edit a receipt." {
		t.Fatalf("the settled session keeps the goal its open carried: %+v", m)
	}
}

// A line that lost its head is taken back while its turn runs and typed
// again: behind eight spaces, which absorb a second loss, unless it is a
// slash command, which behind spaces is words.
func TestLostHeadIsRetyped(t *testing.T) {
	for _, tc := range []struct{ line, retype string }{
		{"warm up the cache, then run the suite", "        warm up the cache, then run the suite"},
		{"/pl-loopy-handoff-distill merged and deployed", "/pl-loopy-handoff-distill merged and deployed"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			r := newRig(t, "none")
			c := adopted(t, r, "T1")
			c.Edit(func(c *testkit.Claude) {
				c.DropHead = 3
				// The garbled prompt starts a turn that runs on, as a real one
				// would, while Rex still reads the turn before's done.
				c.Reply = func(c *testkit.Claude, p string) {
					if !strings.HasPrefix(p, "\n") {
						c.Done("ok")
					} else {
						c.RexOnly("done", "", "")
					}
				}
			})
			sent, err := r.d.Send(r.view("T1"), drive.Step{Line: tc.line}, drive.SendOptions{})
			if err != nil || sent.Outcome != "recovered" {
				t.Fatalf("a line that lost its head is taken back and retyped: %+v %v", sent, err)
			}
			typed, _ := c.Seen()
			if last := typed[len(typed)-1]; last != tc.retype {
				t.Fatalf("retyped as %q, want %q", last, tc.retype)
			}
			if log, _ := os.ReadFile(c.Transcript()); !strings.Contains(string(log), "[Request interrupted by user]") {
				t.Fatal("the garbled turn is interrupted while it runs")
			}
		})
	}
}

// Ctrl+C stops whatever turn runs: once the garbled turn has ended, drive
// leaves it and says so, though Rex has not heard it end.
func TestAGarbledTurnThatEndedIsNotTakenBack(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Edit(func(c *testkit.Claude) {
		c.DropHead = 3
		c.Reply = func(c *testkit.Claude, _ string) { c.Done("ok"); c.RexOnly("working", "", "") }
	})
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "/pl-loopy-handoff-distill merged and deployed"}, drive.SendOptions{})
	if err == nil || sent == nil || sent.Outcome != "garbled" || !strings.Contains(err.Error(), "drive --batch b1 read T1") {
		t.Fatalf("a garbled turn that already ended is reported, not retyped: %+v %v", sent, err)
	}
	if _, keys := c.Seen(); contains(keys, "ctrl+c") {
		t.Fatalf("no Ctrl+C into a finished turn: %q", keys)
	}
}

func contains(xs []string, x string) bool {
	return slices.Contains(xs, x)
}

// /strategic-review sent as a file pointer was refused inside the session
// three times in one batch, and drive's refusal of it 15 times since, each
// time a bot typed the line with the rest attached: drive does that itself,
// and what reaches the session is the skill's own line, typable as any
// other.
func TestATypedOnlySkillInAFileIsTypedWithTheRestAttached(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("look at the door ", 20))
	for _, tc := range []struct {
		name, file string
		noticed    bool   // the session read the notice before
		typed      string // what is typed ahead of the attachment's mention, or all that is typed
		attached   string // what the attachment holds; "" when there is none
	}{
		{"its words fit the line", "/strategic-review 9531\n\nLook hardest at the door.\n", false, "/strategic-review 9531", "Look hardest at the door."},
		{"its words are too long to type", "/strategic-review " + long + "\n", false, "/strategic-review", long},
		{"its words hold a tab", "/strategic-review\t9531\nLook hardest at the door.\n", false, "/strategic-review", "9531"},
		{"it is only the line, the notice read", "/strategic-review 9531\n", true, "/strategic-review 9531", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "none")
			c := r.w.Add("T1")
			done(c, "onboard", "analysis")
			typedOnly(t, r)
			ref := c.Block[len(c.Block)-6:]
			if tc.noticed {
				if _, err := r.d.Send(r.view(ref), drive.Step{Line: "start"}, drive.SendOptions{Label: "T1"}); err != nil {
					t.Fatal(err)
				}
				ref = "T1"
			}
			f := filepath.Join(r.w.Root, "sr.md")
			_ = os.WriteFile(f, []byte(tc.file), 0o644)
			sent, err := r.d.Send(r.view(ref), drive.Step{File: f}, drive.SendOptions{Label: "T1"})
			if err != nil {
				t.Fatal(err)
			}
			if m, err := r.d.Metrics(); err != nil || m.Commands != 1 {
				t.Fatalf("metrics count the skill's line as a slash command: %+v %v", m, err)
			}
			typed, _ := c.Seen()
			got := typed[len(typed)-1]
			if strings.ContainsFunc(got, unicode.IsControl) || !strings.Contains(sent.Text(), tc.typed) {
				t.Fatalf("typed %q, printed %q", got, sent.Text())
			}
			if tc.attached == "" {
				if got != tc.typed {
					t.Fatalf("only the skill's line is typed: %q", got)
				}
				return
			}
			path, ok := strings.CutPrefix(got, tc.typed+" @")
			if !ok {
				t.Fatalf("the skill's line is typed, its rest attached: %q", got)
			}
			rest, _ := os.ReadFile(strings.TrimSpace(path))
			if !strings.Contains(string(rest), tc.attached) || strings.Contains(string(rest), "/strategic-review") || !strings.Contains(string(rest), noticeText) {
				t.Fatalf("the attachment is the rest, the notice ahead of it: %q", rest)
			}
		})
	}
	t.Run("queued, in a snippet, and on an open", func(t *testing.T) {
		r := newRig(t, "none")
		c := adopted(t, r, "T1")
		typedOnly(t, r)
		f := filepath.Join(r.w.Root, "sr.md")
		_ = os.WriteFile(f, []byte("/strategic-review 9531\nLook hardest at the door.\n"), 0o644)
		// Mid-turn a mention attaches nothing, so a queued one waits for the turn's end.
		c.Busy("the review")
		if _, err := r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{Queue: true}); !isRefused(err) || !strings.Contains(err.Error(), "starts only when typed") {
			t.Fatalf("queued, it is refused: %v", err)
		}
		// A snippet headed by the skill is typed the same way, under its key.
		c.Done("reviewed")
		_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".config/tabtype"), 0o755)
		_ = os.WriteFile(filepath.Join(r.d.Cfg.Home, ".config/tabtype/config.toml"),
			[]byte("[[snippets]]\nkey = \"sr\"\nexpand = \"/strategic-review 9531\\nLook hardest at the door.\"\n"), 0o644)
		sent, err := r.d.Send(r.view("T1"), drive.Step{Key: "sr"}, drive.SendOptions{})
		if err != nil || !strings.HasPrefix(sent.Typed, "/strategic-review 9531 @") {
			t.Fatalf("a snippet headed by it is typed as the skill's line: %+v %v", sent, err)
		}
		if rest, _ := os.ReadFile(sent.Path); strings.TrimSpace(string(rest)) != "Look hardest at the door." {
			t.Fatalf("the snippet's other lines are attached: %q", rest)
		}
		if evs := r.events(batch.KindSend); evs[len(evs)-1].Key != "sr" {
			t.Fatalf("its send keeps the snippet's key: %+v", evs[len(evs)-1])
		}
		// An open types it the same way.
		o, err := r.d.Open("sr", drive.Step{File: f}, drive.OpenOptions{Dir: r.w.Root})
		if err != nil || o.Sent == nil {
			t.Fatalf("%+v %v", o, err)
		}
		if typed, _ := r.w.Session(o.Block).Seen(); len(typed) == 0 || !strings.HasPrefix(typed[len(typed)-1], "/strategic-review 9531 @") {
			t.Fatalf("an open types the skill's line too: %q", typed)
		}
	})
}

// planlab's closeout key is one line, "/pl-loopy-handoff-distill merged and
// deployed", whose skill starts only when typed. Sent with words after it,
// drive refused it ten times across three batches (11-03 to 11-10), and each
// time the driver typed the command itself with the words in a file. drive
// does that: the command's line typed, where the session starts the skill,
// the words attached, and the snippet's key kept on the record, which is
// what metrics counts it by.
func TestAKeyedTypedOnlyCommandIsTypedWithItsWordsAttached(t *testing.T) {
	const (
		line  = "/pl-loopy-handoff-distill merged and deployed"
		rider = "The merged record has no marker for its owed read: add one in a docs pull request and merge it."
	)
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Skill("pl-loopy-handoff-distill", true)
	tab := filepath.Join(c.Cwd, ".tabtype.local.toml")
	if err := os.WriteFile(tab, []byte("[[snippets]]\nkey = \"closeout\"\nexpand = \"\"\"\n"+line+"\n\"\"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What a file does: told to run the skill from one, as the snippet's
	// pointer would tell it, the session cannot start it.
	told := filepath.Join(r.w.Root, "told.md")
	_ = os.WriteFile(told, []byte(line+"\n\n"+rider+"\n"), 0o644)
	_ = r.w.Type(c.Block, "@"+told+" ")
	_ = r.w.Key(c.Block, "enter")
	if rd, err := r.d.Read("T1", drive.ReadLast, 1); err != nil || !strings.Contains(rd.Body, "runs only when you type it") {
		t.Fatalf("a session told to run the skill from a file refuses: %+v %v", rd, err)
	}
	sent, err := r.d.Send(r.view("T1"), drive.Step{Key: "closeout", Text: rider}, drive.SendOptions{Hold: "planlab-main"})
	if err != nil {
		t.Fatalf("the key goes with its words: %v", err)
	}
	path, ok := strings.CutPrefix(sent.Typed, line+" @")
	if typed, _ := c.Seen(); !ok || typed[len(typed)-1] != sent.Typed+" " {
		t.Fatalf("the command's line is typed, its words attached: %q, typed %q", sent.Typed, typed)
	}
	if body, _ := os.ReadFile(path); strings.TrimSpace(string(body)) != rider {
		t.Fatalf("the attachment is the words alone: %q", body)
	}
	// Told to run it from a file, the session says it cannot start the skill.
	if rd, err := r.d.Read("T1", drive.ReadLast, 1); err != nil || rd.Body != "ok" {
		t.Fatalf("the session starts the skill: %+v %v", rd, err)
	}
	if !strings.Contains(sent.Text(), line) || !strings.Contains(sent.Text(), "snippet from "+tab) {
		t.Fatalf("the send prints the line typed and the snippet's file: %q", sent.Text())
	}
	for _, kind := range []string{batch.KindSendIntent, batch.KindSend} {
		evs := r.events(kind)
		if e := evs[len(evs)-1]; e.Key != "closeout" || e.Text != sent.Typed || e.Path != path || e.Hold != "planlab-main" {
			t.Fatalf("the %s keeps the key, the line typed and the hold: %+v", kind, e)
		}
	}
	if m, err := r.d.Metrics(); err != nil || m.Snippets != 1 || m.ByKey["closeout"] != 1 || m.Commands != 0 {
		t.Fatalf("metrics count it as the owner's snippet: %+v %v", m, err)
	}
	// Mid-turn a mention attaches nothing, so queued it still waits for the turn's end.
	c.Busy("the closeout")
	typed, _ := c.Seen()
	_, err = r.d.Send(r.view("T1"), drive.Step{Key: "closeout", Text: rider}, drive.SendOptions{Queue: true})
	if after, _ := c.Seen(); !isRefused(err) || !strings.Contains(err.Error(), "starts only when typed") || len(after) != len(typed) {
		t.Fatalf("queued, it is refused and nothing is typed: %v", err)
	}
}

// A relaunch carries the step the session was last sent as the session got
// it: a typed-only skill's line with its file attached, typed again, since
// the attached file alone no longer starts the skill.
func TestARelaunchRetypesATypedOnlySkillsLine(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	typedOnly(t, r)
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "other@example.com", "sessions"), 0o755)
	f := filepath.Join(r.w.Root, "sr.md")
	_ = os.WriteFile(f, []byte("/strategic-review 9531\nLook hardest at the door.\n"), 0o644)
	sent, err := r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.Fail("authentication_failed", "Login expired · Please run /login")
	c.Set("blocked", "auth", "Login expired · Please run /login")
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || !strings.Contains(rd.Next, "--account other@example.com '"+sent.Typed+"'") {
		t.Fatalf("the relaunch types the skill's line and its attachment again (%s): %+v %v", sent.Typed, rd, err)
	}
}

// typedOnly installs /strategic-review as a skill that starts only when typed.
func typedOnly(t *testing.T, r *rig) {
	t.Helper()
	sk := filepath.Join(r.d.Cfg.Home, ".claude/skills/strategic-review")
	_ = os.MkdirAll(sk, 0o755)
	if err := os.WriteFile(filepath.Join(sk, "SKILL.md"), []byte("---\nname: strategic-review\ndisable-model-invocation: true\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTypedInputIsLeftAlone(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	c.Edit(func(c *testkit.Claude) { c.Input = "Go heavy, take your recommendations" })
	v := r.view(c.Block[len(c.Block)-6:])
	if _, err := r.d.Send(v, drive.Step{Line: "consult-design"}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "typed text") {
		t.Fatalf("a line someone typed and did not send stops the send, got %v", err)
	}
	if err := r.d.Close(v); !isRefused(err) {
		t.Fatalf("and the close, got %v", err)
	}
	// A screen with no input box on it takes no typing: it would land in whatever is there.
	c.Edit(func(c *testkit.Claude) { c.Input, c.Screen = "", "Transcript view · ctrl+o to return" })
	_, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "consult-design"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "drive --batch b1 read "+c.Block[len(c.Block)-6:]+" --screen") {
		t.Fatalf("a screen with no input box stops the send, saying how to see it: %v", err)
	}
	if rd, _ := r.d.Read(c.Block[len(c.Block)-6:], drive.ReadScreen, 0); !strings.Contains(rd.Body, "Transcript view") {
		t.Fatalf("read --screen shows it: %q", rd.Body)
	}
	if typed, _ := c.Seen(); len(typed) != 0 {
		t.Fatalf("nothing typed: %q", typed)
	}
	// Claude Code's own dimmed suggestion is not typed text.
	c.Edit(func(c *testkit.Claude) { c.Screen, c.Suggestion = "", "Read /tmp/T1-5.md and do what it asks." })
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "consult-design"}, drive.SendOptions{}); err != nil {
		t.Fatalf("a suggestion does not stop a send: %v", err)
	}
}

func TestHoldIsExclusiveUntilTheTurnEnds(t *testing.T) {
	r := newRig(t, "none")
	a, b := r.w.Add("A"), r.w.Add("B")
	done(a, "x", "y")
	done(b, "x", "y")
	for label, c := range map[string]*testkit.Claude{"T3": a, "T5": b} {
		if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "land it"}, drive.SendOptions{Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the release runs on
	if _, err := r.d.Send(r.view("T3"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by T3") {
		t.Fatalf("a second release while the first runs is refused, got %v", err)
	}
	a.Done("released")
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("once the holder's turn ends the hold is free: %v", err)
	}
}

// A driver that died after recording its send leaves an intent. The next
// driver to own the session settles it from the transcript, whatever its age.
func TestAnIntentLeftByAGoneDriverIsSettled(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "x"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindSendIntent, Session: "T1", Intent: "lost", Text: "implement-spec"})
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "y"}, drive.SendOptions{}); err != nil {
		t.Fatalf("an intent whose driver has gone does not block: %v", err)
	}
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindSendIntent, Session: "T1", Intent: "arrived", Text: "implement-spec"})
	r.w.Clock.Sleep(time.Second)
	done(c, "implement-spec", "implemented")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "z"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// The owner's own words after an intent are not its arrival.
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindSendIntent, Session: "T1", Intent: "overtaken", Text: "implement-spec"})
	r.w.Clock.Sleep(time.Second)
	done(c, "hold off", "holding")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "w"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	got, why := map[string]string{}, map[string]string{}
	for _, e := range append(r.events(batch.KindSend), r.events(batch.KindFailure)...) {
		if e.Intent == "lost" || e.Intent == "arrived" || e.Intent == "overtaken" {
			got[e.Intent], why[e.Intent] = e.Kind+" "+e.Outcome, e.Reason
		}
	}
	if got["lost"] != "failure " || got["arrived"] != "send arrived" || got["overtaken"] != "failure " {
		t.Fatalf("settled from the transcript: %v", got)
	}
	if !strings.Contains(why["overtaken"], "hold off") {
		t.Fatalf("a step overtaken by another prompt says what arrived instead: %q", why["overtaken"])
	}
	if st, _ := r.d.Status(false); len(st.InFlight) != 0 {
		t.Fatalf("nothing is left in flight: %+v", st.InFlight)
	}
}

// A session that cannot be read for a moment says nothing about whether a
// step an earlier driver left arrived: the step stays in flight, and the
// hold it took stands, until a read can tell.
func TestAnUnreadableSessionSettlesNothing(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T5")
	adopted(t, r, "T3")
	// T5's driver admitted a release and went before recording how it went;
	// the release reached T5 and runs.
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindSendIntent, Session: "T5", Intent: "left", Hold: "release", Text: "release"})
	r.w.Clock.Sleep(time.Second)
	a.Busy("release")
	v := r.view("T5")
	r.w.StatusFails = 1 // Rex is busy for a moment as the next driver reads T5
	if _, err := r.d.Send(v, drive.Step{Line: "and the changelog"}, drive.SendOptions{Queue: true}); !isRefused(err) || !strings.Contains(nextOf(err), "try it again") {
		t.Fatalf("an unreadable session is a refusal to try again: %v", err)
	}
	if st, _ := r.d.Status(false); len(st.InFlight) != 1 || st.InFlight[0].Intent != "left" {
		t.Fatalf("the step stays in flight: %+v", st.InFlight)
	}
	if _, err := r.d.Send(r.view("T3"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by T5") {
		t.Fatalf("and its hold stands: %v", err)
	}
	if _, err := r.d.Send(v, drive.Step{Line: "and the changelog"}, drive.SendOptions{Queue: true}); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.events(batch.KindSend) {
		if e.Intent == "left" && e.Outcome == "arrived" {
			return
		}
	}
	t.Fatalf("once T5 reads, the step is settled as arrived: %+v", r.events(batch.KindSend))
}

func TestWaitEndsOnAnAPIErrorRexWasNotToldAbout(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T2")
	c.Busy("build it")
	c.Fail("authentication_failed", "Login expired · Please run /login") // Rex still reads working
	e, err := r.d.Wait(c.Block[len(c.Block)-6:], time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "blocked:auth" || e.Error == nil || !strings.Contains(e.Text(), "Login expired") {
		t.Fatalf("the wait returns on the error at once: %+v", e)
	}
}

func TestWaitIgnoresShellsOlderThanTheTurn(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T2")
	old := r.w.Clock.Now().Add(-9 * time.Hour)
	c.Edit(func(c *testkit.Claude) { c.Shells = []session.Shell{{Started: old, Command: "sleep 600"}} })
	r.w.Clock.Sleep(time.Second)
	done(c, "build", "build report done")
	start := r.w.Clock.Now()
	e, err := r.d.Wait(c.Block[len(c.Block)-6:], time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if took := r.w.Clock.Now().Sub(start); took > 5*time.Minute || e.Orphans != 1 || e.Capped {
		t.Fatalf("a shell left from an older turn neither holds the wait nor counts as this turn's: took %s, %+v", took, e)
	}
}

// status listed Rex's blocks again for every Claude session outside the
// batch, though the list it read them from was in hand: with ten outside
// across nine Rex sessions, 237 rex commands where 37 read the same rows.
func TestStatusListsRexsBlocksNoMoreForSessionsOutsideTheBatch(t *testing.T) {
	r := newRig(t, "none")
	adopted(t, r, "T1")
	listings := func() (n int, st *drive.Status) {
		t.Helper()
		before := r.w.Listings
		st, err := r.d.Status(false)
		if err != nil {
			t.Fatal(err)
		}
		return r.w.Listings - before, st
	}
	alone, _ := listings()
	for _, label := range []string{"a", "b", "c", "d", "e"} {
		done(r.w.Add(label), "onboard", "analysis")
	}
	shell := r.w.Add("a shell")
	shell.Edit(func(c *testkit.Claude) { c.Running = false })
	among, st := listings()
	if len(st.Rows) != 1 || len(st.Others) != 5 {
		t.Fatalf("the batch's session, and the five running Claude Code outside it: %d and %d", len(st.Rows), len(st.Others))
	}
	if among != alone {
		t.Fatalf("a session outside the batch is read in the block the list named: %d listings with six blocks outside, %d with none", among, alone)
	}
}

// A session outside the batch is answered by its block id, and the answer
// is recorded under that id with the call it answered, as a refusal or a
// close of it is.
func TestRespondReachesASessionOutsideTheBatch(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	c.Busy("clean up")
	c.Ask("rm -rf node_modules/.vite")
	short := c.Block[len(c.Block)-6:]
	if _, err := r.d.Respond(r.view(short), "approve"); err != nil {
		t.Fatal(err)
	}
	if ev := r.events(batch.KindRespond); len(ev) != 1 || ev[0].Session != short || ev[0].Action != "approve" || !strings.Contains(ev[0].Call, "node_modules/.vite") {
		t.Fatalf("recorded under its block, with what it approved: %+v", ev)
	}
}

// A watch or timer re-execs drive as this drive's own hidden command, pinned
// to its batch; TestOlderDrivesCommandLinesStillRun holds that each parses.
func TestAWakeRunsThisDrivesCommandInItsBatch(t *testing.T) {
	r := newRig(t, "webhook")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	sent, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"})
	if err != nil || !sent.Watched {
		t.Fatalf("a send arms the wake: %+v %v", sent, err)
	}
	if argv := r.watches.Running["drive-b1-"+c.Block[len(c.Block)-6:]]; strings.Join(argv, " ") != "/bin/drive --batch b1 notify T1" {
		t.Fatalf("the watch: %q", argv)
	}
	if err := r.d.Timer(5*time.Minute, "-v veto"); err != nil {
		t.Fatal(err)
	}
	if argv := r.watches.Running["drive-b1-timer--v-veto"]; strings.Join(argv, " ") != "/bin/drive --batch b1 ring 300 -- -v veto" {
		t.Fatalf("the timer: %q", argv)
	}
}

func TestRecordsAndMetrics(t *testing.T) {
	r := newRig(t, "webhook")
	var mu sync.Mutex
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	notify := func(c *testkit.Claude, label string) {
		t.Helper()
		if _, err := r.d.Notify("drive-b1-"+c.Block[len(c.Block)-6:], label, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	g, err := r.d.Grant(`"you can merge the PR on my behalf" (chat 02:08)`, "merge and release this batch's PRs")
	if err != nil || g != "G1" {
		t.Fatalf("grant: %s %v", g, err)
	}
	a, _ := r.d.Ask("T1", "Blind-judge the gate pages?", "a fresh Fable session judges them", "T1 waits before merge")
	r.w.Clock.Sleep(13 * time.Minute)
	if _, err := r.d.Answer(a, `"ask a fresh Fable session"`, "delegated", true); err != nil {
		t.Fatal(err)
	}
	_, _ = r.d.Ask("T1", "Phase C boxes?", "no", "nothing waits")

	// T1's first wake is handled by re-arming its watch, which wakes the
	// driver again; its last wake has nothing after it. Its /compact arrives
	// without its start, and the driver re-arms the watch on what it made of it.
	notify(c, "T1")
	if _, err := r.d.Watch("T1"); err != nil {
		t.Fatal(err)
	}
	notify(c, "T1")
	c.Edit(func(c *testkit.Claude) { c.DropHead = 3 })
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "/compact"}, drive.SendOptions{}); err == nil {
		t.Fatal("a line that lost its start in a turn that ended is reported")
	}
	if m, _ := r.d.Metrics(); m.Wakes != 2 || m.WakesUntraced != 1 || m.WakesActedOn+m.WakesHeardOnly != 1 {
		t.Fatalf("a watch armed again shows no driver, since the watchdog arms one with no run behind it: of %d wakes, %d acted on, %d heard only, %d untraced",
			m.Wakes, m.WakesActedOn, m.WakesHeardOnly, m.WakesUntraced)
	}
	if _, err := r.d.Watch("T1"); err != nil {
		t.Fatal(err)
	}
	notify(c, "T1")

	// T2 stops on a prompt and is approved; the end of that turn is never
	// posted, so the approval is a turn left unwatched.
	c2 := r.w.Add("T2")
	done(c2, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c2.Block[len(c2.Block)-6:]), drive.Step{Line: "start"}, drive.SendOptions{Label: "T2"}); err != nil {
		t.Fatal(err)
	}
	notify(c2, "T2")
	c2.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) { c.Ask("rm -rf node_modules/.vite") }
	})
	step := filepath.Join(t.TempDir(), "step.md")
	if err := os.WriteFile(step, []byte("Clear the build cache, then run the tests.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("T2"), drive.Step{File: step}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	notify(c2, "T2")
	if _, err := r.d.Send(r.view("T2"), drive.Step{Line: "go on"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("a step into a prompt is refused: %v", err)
	}
	if _, err := r.d.Respond(r.view("T2"), "approve"); err != nil {
		t.Fatal(err)
	}
	c2.Done("cleaned, tests pass")
	c2.Edit(func(c *testkit.Claude) {
		c.Shells = []session.Shell{{Started: r.w.Clock.Now(), Command: "pnpm test --watch"}}
	})
	if err := r.d.Close(r.view("T2")); !isRefused(err) {
		t.Fatalf("a close while a background shell runs is refused: %v", err)
	}
	c2.Edit(func(c *testkit.Claude) { c.Shells = nil })
	r.w.Clock.Sleep(time.Minute)
	c2.Prompt("the owner typed this")
	if _, err := r.d.Send(r.view("T2"), drive.Step{Line: "go on"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("a step on a prompt nobody has answered is refused: %v", err)
	}

	// A timer's wake, and one whose post fails.
	for i, note := range []string{"veto", "merge window"} {
		mu.Lock()
		status = []int{200, 503}[i]
		mu.Unlock()
		if err := r.d.Timer(5*time.Minute, note); err != nil {
			t.Fatal(err)
		}
		_ = r.d.Ring("drive-b1-timer-"+strings.ReplaceAll(note, " ", "-"), note)
	}

	r.w.Clock.Sleep(30 * time.Minute)
	if _, err := r.d.Note("T1", "the night ends here"); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(5 * time.Hour) // metrics read the morning after
	m, err := r.d.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.Asks != 2 || m.Answered != 1 || m.Took["delegated"] != 1 || m.Explained != 1 || m.Unanswered != 1 || m.MedianAnswer != 13*time.Minute {
		t.Fatalf("ask measures: %+v", m)
	}
	evs, _ := r.b.Events()
	open := r.events(batch.KindAsk)[1]
	if m.To != evs[len(evs)-1].At || m.OwnerWait != 13*time.Minute+m.To.Sub(open.At) {
		t.Fatalf("an open ask waits until the batch's last event, not until the metrics are read: to %s, wait %s", m.To, m.OwnerWait)
	}
	r.w.Clock.Sleep(time.Hour)
	if again, _ := r.d.Metrics(); again.OwnerWait != m.OwnerWait {
		t.Fatalf("owner wait does not grow with every reading: %s then %s", m.OwnerWait, again.OwnerWait)
	}
	if m.Sends != 4 || m.Sessions != 2 || m.Snippets+m.Commands+m.OwnWords != m.Sends {
		t.Fatalf("send measures: %+v", m)
	}
	if m.Commands != 1 || m.OwnWords != 3 {
		t.Fatalf("a typed /compact is a command, not the driver's own words; a typed line and a file are: %d commands, %d own", m.Commands, m.OwnWords)
	}
	// T1's first wake drew a watch armed again and nothing else; its second a
	// step, and both of T2's a step or an answered prompt; its last a note, and
	// the driver's work on T2, with T1 left as it was.
	if m.Wakes != 5 || m.WakesActedOn != 3 || m.WakesHeardOnly != 1 || m.WakesUntraced != 1 {
		t.Fatalf("each of five wakes is counted once, by what followed it: %d acted on, %d heard only, %d untraced of %d",
			m.WakesActedOn, m.WakesHeardOnly, m.WakesUntraced, m.Wakes)
	}
	if m.Unwatched != 1 {
		t.Fatalf("the approval that left T2's turn with no wake after it is counted: %d", m.Unwatched)
	}
	if m.Garbled != 1 {
		t.Fatalf("the /compact that lost its start is counted: %d", m.Garbled)
	}
	if m.Timers != 1 || m.WakeFails != 1 {
		t.Fatalf("a timer's wake is counted, and its failed post with the failures: %d timers, %d failed", m.Timers, m.WakeFails)
	}
	if len(m.Refusals) != 3 || m.Refusals["other"] != 0 {
		t.Fatalf("a step into a prompt, a close over a running shell and a step on an unanswered prompt are each a kind: %v", m.Refusals)
	}
}

// Five batches of 29 October to 2 November read "0 wakes no action followed"
// while 27 of their 184 wakes had drawn no step: a watch armed again counted
// as the wake handled, and the watchdog arms one with no run behind it.
func TestAWakeIsCountedByWhatItDrew(t *testing.T) {
	r := newRig(t, "webhook")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	first := r.view(c.Block[len(c.Block)-6:])
	if _, err := r.d.Send(first, drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	wake := func() {
		t.Helper()
		r.w.Clock.Sleep(2 * time.Minute)
		if _, err := r.d.Notify("drive-b1-"+c.Block[len(c.Block)-6:], "T1", time.Second); err != nil {
			t.Fatal(err)
		}
		r.w.Clock.Sleep(2 * time.Minute)
	}
	counts := func() [3]int {
		t.Helper()
		m, err := r.d.Metrics()
		if err != nil {
			t.Fatal(err)
		}
		if m.WakesActedOn+m.WakesHeardOnly+m.WakesUntraced != m.Wakes+m.Carried {
			t.Fatalf("every wake is one of the three: %+v", m)
		}
		return [3]int{m.WakesActedOn, m.WakesHeardOnly, m.WakesUntraced}
	}

	wake() // the watchdog arms it again, with no run behind it
	if _, err := r.d.Watch("T1"); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != [3]int{0, 0, 1} {
		t.Fatalf("a watch armed again is no trace of a driver: %v", got)
	}
	wake() // a run reads the batch and arms the watch again
	r.d.Seen()
	if _, err := r.d.Watch("T1"); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != [3]int{0, 1, 1} {
		t.Fatalf("a run that read the batch and left the session was heard: %v", got)
	}
	wake() // a run refuses itself a step, then parks the session
	if _, err := r.d.Send(first, drive.Step{Line: "go on"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("a step on a view the session moved past is refused: %v", err)
	}
	if got := counts(); got != [3]int{0, 2, 1} {
		t.Fatalf("a refusal changes nothing in the session: %v", got)
	}
	if err := r.d.Park(r.view("T1"), "waits on a release", r.w.Clock.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != [3]int{1, 1, 1} {
		t.Fatalf("a park is the driver's decision on the session: %v", got)
	}
}

// notify.log was empty after ~130 waits in a batch, so a delivered wake and
// a dead wait looked the same. Every post is now recorded.
func TestWakeRecordsEveryPost(t *testing.T) {
	r := newRig(t, "webhook")
	var bodies []string
	var mu sync.Mutex
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		code := status
		mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer k" {
			code = 401
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL, Auth: "Bearer k"}
	archived := filepath.Join(r.w.Root, "archived")
	r.d.Cfg.AfterWake = []string{"echo archived >> " + archived, "exit 7"}
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	e, err := r.d.Wait("T1", time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r.watches.Retire("drive-b1-" + c.Block[len(c.Block)-6:]) // as a watch does before it posts
	if err := r.d.Wake("T1", e, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(archived); string(b) != "archived\n" {
		t.Fatalf("the after-wake command ran once the wake was posted: %q", b)
	}
	if notes := r.events(batch.KindNote); len(notes) != 1 || !strings.Contains(notes[0].Note, `"exit 7" failed`) {
		t.Fatalf("a failing after-wake command is noted, not fatal: %+v", notes)
	}
	mu.Lock()
	status = 400
	mu.Unlock()
	if err := r.d.Wake("T1", e, ""); err == nil {
		t.Fatal("a wake nobody accepted is an error")
	}
	wakes := r.events(batch.KindWake)
	if len(wakes) != 2 || wakes[0].HTTP != 200 || wakes[0].View != e.View.String() || wakes[1].HTTP != 0 || !strings.Contains(wakes[1].Reason, "400") {
		t.Fatalf("both posts are recorded, the failure with why: %+v", wakes)
	}
	mu.Lock()
	first := bodies[0]
	mu.Unlock()
	if !strings.Contains(first, `"view":"T1@`) || !strings.Contains(first, "its turn ended") {
		t.Fatalf("the wake carries the view to act on: %s", first)
	}
	st, err := r.d.Status(false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows[0].Watch == "armed" || !st.Rows[0].Unhandled {
		t.Fatalf("a wake no driver acted on shows: %+v", st.Rows[0])
	}
}

func TestLaunchCommand(t *testing.T) {
	r := newRig(t, "none")
	r.d.Cfg.LaunchEnv = map[string]string{"PLANLAB_AGENT_NUDGES": "off"}
	var got []string
	term := &recordingTerm{World: r.w, run: func(cmd []string) { got = cmd }}
	r.d.Term = term
	if _, err := r.d.Open("judge", drive.Step{}, drive.OpenOptions{Dir: r.w.Root, Account: "other@example.com", Model: "fable", Effort: "xhigh", Resume: "sid-1"}); err != nil {
		t.Fatal(err)
	}
	want := "PLANLAB_AGENT_NUDGES=off x-other@example.com --model fable --effort xhigh --resume sid-1; exec zsh -l"
	if len(got) != 3 || got[0] != "/bin/zsh" || got[1] != "-lic" || got[2] != want {
		t.Fatalf("launch: %q", got)
	}
	// The primary account's launcher is named by the owner's shell, not "primary";
	// an open that names no model or effort adds neither.
	r.d.Cfg.PrimaryName = "owner"
	if _, err := r.d.Open("judge2", drive.Step{}, drive.OpenOptions{Dir: r.w.Root, Account: "primary"}); err != nil {
		t.Fatal(err)
	}
	if want := "PLANLAB_AGENT_NUDGES=off x-owner; exec zsh -l"; len(got) != 3 || got[2] != want {
		t.Fatalf("launch on the primary: %q", got)
	}
}

type recordingTerm struct {
	*testkit.World
	run func([]string)
}

func (r *recordingTerm) Run(o rex.Open) (string, error) {
	r.run(o.Command)
	return r.World.Run(o)
}

func TestHoldWaitersAreListedAndTheRefusalSaysHowBack(t *testing.T) {
	r := newRig(t, "webhook")
	a, b := r.w.Add("A"), r.w.Add("B")
	done(a, "x", "y")
	done(b, "x", "y")
	for label, c := range map[string]*testkit.Claude{"T5s": a, "T3s": b} {
		if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "land it"}, drive.SendOptions{Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("T5s"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.d.Send(r.view("T3s"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
	if !isRefused(err) || !strings.Contains(err.Error(), "T5s's wake comes when its turn ends") {
		t.Fatalf("a chat driver is told the holder's wake brings it back: %v", err)
	}
	st, _ := r.d.Status(false)
	if len(st.Holds) != 1 || !strings.Contains(st.Holds[0], "held by T5s") || !strings.Contains(st.Holds[0], "waiting for it: T3s (refused release") {
		t.Fatalf("status names the holder, who waits and the step it was refused: %q", st.Holds)
	}
	a.Done("released")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "which T5s held: T3s") || !strings.Contains(string(body), "drive --batch b1 send T3s@") ||
			!strings.Contains(string(body), "--hold release release") {
			t.Errorf("the holder's wake names who waits for what it held: %s", body)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	e, err := r.d.Wait("T5s", time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.Wake("T5s", e, ""); err != nil {
		t.Fatal(err)
	}
	st, _ = r.d.Status(false)
	if len(st.Holds) != 1 || !strings.HasPrefix(st.Holds[0], "release: free; waiting for it: T3s") {
		t.Fatalf("once free, the waiter is still listed until it is sent: %q", st.Holds)
	}
	if _, err := r.d.Send(r.view("T3s"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	st, _ = r.d.Status(false)
	for _, h := range st.Holds {
		if strings.Contains(h, "waiting for it: T3s") {
			t.Fatalf("a sent waiter is no longer listed: %q", st.Holds)
		}
	}
}

// On 2026-11-09 a send to a block outside the batch was refused on release,
// which S2 held. A minute later the block joined the batch as S1 on another
// step, and status went on listing "waiting for it: bf4e51 (refused --file
// /tmp/s1-a1.md at 14:46)" through S1's parks, its merge and its close: the
// refusal named the block, and nothing after it did. A waiter is done once
// its session is sent a step or closes, under the label it joined as.
func TestAWaiterRefusedBeforeItJoinedIsDoneOnceItIsSent(t *testing.T) {
	r := newRig(t, "webhook")
	holder := adopted(t, r, "S2")
	holder.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the release runs on
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	waits := func(want string) {
		t.Helper()
		st, err := r.d.Status(false)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, h := range st.Holds {
			if _, who, ok := strings.Cut(h, "waiting for it: "); ok {
				got, _, _ = strings.Cut(who, " ")
			}
		}
		if got != want {
			t.Fatalf("%q waits for release, want %q: %q", got, want, st.Holds)
		}
	}
	c := r.w.Add("S1")
	done(c, "onboard", "analysis")
	ref := c.Block[len(c.Block)-6:]
	merge := filepath.Join(r.w.Root, "merge.md")
	_ = os.WriteFile(merge, []byte("Merge when the checks are green, and release.\n"), 0o644)
	if _, err := r.d.Send(r.view(ref), drive.Step{File: merge}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by S2") {
		t.Fatalf("held: %v", err)
	}
	waits(ref)
	if _, err := r.d.Send(r.view(ref), drive.Step{Line: "Fold the review's findings first."}, drive.SendOptions{Label: "S1"}); err != nil {
		t.Fatal(err)
	}
	waits("")
	// The holder's wake names no one it would have the driver send to again.
	holder.Done("released")
	l := hook(t, r)
	e, err := r.d.Wait("S2", time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.Wake("S2", e, ""); err != nil || strings.Contains(l.output(0), "waiting for release") {
		t.Fatalf("its wake names no waiter: %v\n%s", err, l.output(0))
	}
	// One of the batch's own, refused and then closed, waits no more either.
	if _, err := r.d.Send(r.view("S2"), drive.Step{Line: "and the changelog"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	holder.Edit(func(c *testkit.Claude) { c.Reply = nil })
	holder.Busy("the changelog")
	if _, err := r.d.Send(r.view("S1"), drive.Step{File: merge}, drive.SendOptions{Hold: "release"}); !isRefused(err) {
		t.Fatalf("held again: %v", err)
	}
	waits("S1")
	if err := r.d.Close(r.view("S1")); err != nil {
		t.Fatal(err)
	}
	waits("")
}

func TestAStuckWatchIsFlaggedAndRestarted(t *testing.T) {
	r := newRig(t, "webhook")
	r.d.Cfg.Webhook = config.Webhook{URL: "http://127.0.0.1:1/none"}
	c := r.w.Add("T4")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "review"}, drive.SendOptions{Label: "T4"}); err != nil {
		t.Fatal(err)
	}
	c.Ask("pnpm install")
	r.w.Clock.Sleep(time.Hour) // the watch never posted
	st, _ := r.d.Status(false)
	if !strings.Contains(st.Rows[0].Watch, "stuck") {
		t.Fatalf("a watch alive an hour after the turn ended is flagged: %q", st.Rows[0].Watch)
	}
	before := r.watches.Running
	w, err := r.d.Watch("T4")
	if err != nil || !w.Restarted || len(before) != 1 {
		t.Fatalf("watch restarts it: %+v %v", w, err)
	}
	if n := len(r.events(batch.KindWatch)); n != 2 {
		t.Fatalf("the restart is a new watch: %d", n)
	}
}

// Every account runs Claude Code's vim editor mode. An Escape on an idle box,
// the session's own or a split terminal sequence's, leaves it in NORMAL mode,
// where typing "Reply with the word ok." sent "th the word ok." (seen live,
// 2.1.312). A step typed into such a box goes in INSERT mode first.
func TestAStepTypedIntoAVimBoxInNormalModeArrivesWhole(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.VimMode()
	if err := r.w.Key(c.Block, "escape"); err != nil {
		t.Fatal(err)
	}
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "Reply with the word ok."}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" {
		t.Fatalf("the step arrives whole: %+v %v", sent, err)
	}
}

// A stray "?25h", the tail of a terminal sequence, stayed in ITELL's P2 box
// through two clears on 10-28: ctrl+u deletes only what stands before the
// cursor, and in NORMAL mode only part of the line. The clear reaches the
// line's end in INSERT mode first.
func TestClearEmptiesTheBoxWhereverTheCursorStands(t *testing.T) {
	for name, set := range map[string]func(c *testkit.Claude){
		"cursor at the start": func(c *testkit.Claude) { c.Input, c.AtStart = "?25h", true },
		"normal mode":         func(c *testkit.Claude) { c.Input, c.Normal = "stray text", true },
	} {
		r := newRig(t, "none")
		c := adopted(t, r, "T1")
		c.VimMode()
		c.Edit(set)
		if _, err := r.d.Respond(r.view("T1"), "clear"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		c.Edit(func(c *testkit.Claude) {
			if c.Input != "" || c.Normal {
				t.Errorf("%s: the box holds %q, normal mode %t", name, c.Input, c.Normal)
			}
		})
	}
}

// later is an hour from the rig's now: a park's --until for a test whose park
// is not about what ends it.
func (r *rig) later() time.Time { return r.w.Clock.Now().Add(time.Hour) }
