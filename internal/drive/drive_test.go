package drive_test

import (
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
	b, err := store.New(batch.Meta{Name: "b1", Started: w.Clock.Now(), Wake: wake})
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

func TestSendNeedsAView(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	_, err := r.d.Send(c.Block[len(c.Block)-6:], drive.Step{Line: "go"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "drive read") {
		t.Fatalf("a bare session name must be refused with where a view comes from, got %v", err)
	}
	if typed, _ := c.Seen(); len(typed) != 0 {
		t.Fatalf("nothing may be typed, got %q", typed)
	}
}

// The 10-17 race: run A reads turn T; run B sends the next step and that
// turn finishes; A then sends what it decided on T.
func TestStaleDecisionRefusedAfterAnotherTurn(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	ref := c.Block[len(c.Block)-6:]
	stale := r.view(ref)
	if _, err := r.d.Send(stale, drive.Step{Line: "consult-design"}, drive.SendOptions{}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	// That turn has finished; the session reads done again.
	if _, err := r.d.Send(stale, drive.Step{Line: "recap"}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "moved past") {
		t.Fatalf("a send decided on the old turn must be refused, got %v", err)
	}
	typed, _ := c.Seen()
	if steps := without(typed, "Read "); len(steps) != 1 || steps[0] != "consult-design" {
		t.Fatalf("only the first send may reach the session, got %q", typed)
	}
	if n := len(r.events(batch.KindRefused)); n != 1 {
		t.Fatalf("the refusal is recorded once, got %d", n)
	}
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
	if !isRefused(err) || !strings.Contains(err.Error(), "another driver is sending T1") {
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
	if sent.Typed != "Read "+sent.Path+" and do what it asks." {
		t.Fatalf("a file step is typed as the sentence claude-steps matches, got %q", sent.Typed)
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

func TestTypedFirstStepSendsNoticeAlone(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "/write-spec Go with D1"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	typed, _ := c.Seen()
	if len(typed) != 2 || !strings.HasPrefix(typed[0], "Read ") || typed[1] != "/write-spec Go with D1" {
		t.Fatalf("the notice goes first as a file, then the command is typed alone: %q", typed)
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
				// The garbled prompt starts a turn that runs on, as a real one would.
				c.Reply = func(c *testkit.Claude, p string) {
					if !strings.HasPrefix(p, "\n") {
						c.Done("ok")
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
// leaves it and says so.
func TestAGarbledTurnThatEndedIsNotTakenBack(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Edit(func(c *testkit.Claude) { c.DropHead = 3 })
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "/pl-loopy-handoff-distill merged and deployed"}, drive.SendOptions{})
	if err == nil || sent == nil || sent.Outcome != "garbled" || !strings.Contains(err.Error(), "drive read T1") {
		t.Fatalf("a garbled turn that already ended is reported, not retyped: %+v %v", sent, err)
	}
	if _, keys := c.Seen(); contains(keys, "ctrl+c") {
		t.Fatalf("no Ctrl+C into a finished turn: %q", keys)
	}
}

// without drops the lines starting with prefix: the notice a first step sends ahead of it.
func without(xs []string, prefix string) []string {
	var out []string
	for _, x := range xs {
		if !strings.HasPrefix(x, prefix) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	return slices.Contains(xs, x)
}

// /strategic-review sent as a file pointer was refused inside the session
// three times in one batch.
func TestTypedOnlySkillInAFileIsRefused(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	sk := filepath.Join(r.d.Cfg.Home, ".claude/skills/strategic-review")
	_ = os.MkdirAll(sk, 0o755)
	_ = os.WriteFile(filepath.Join(sk, "SKILL.md"), []byte("---\nname: strategic-review\ndisable-model-invocation: true\n---\nbody\n"), 0o644)
	f := filepath.Join(r.w.Root, "sr.md")
	_ = os.WriteFile(f, []byte("/strategic-review 9531\n\nLook hardest at the door.\n"), 0o644)
	_, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{File: f}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "starts only when typed") || !strings.Contains(err.Error(), "/strategic-review 9531") {
		t.Fatalf("a typed-only skill in a file is refused with the line to type, got %v", err)
	}
	if typed, _ := c.Seen(); len(typed) != 0 {
		t.Fatalf("nothing typed, got %q", typed)
	}
	// On an open, the line to type goes on the open.
	_, err = r.d.Open("sr", drive.Step{File: f}, drive.OpenOptions{Dir: r.w.Root})
	if !isRefused(err) || !strings.Contains(err.Error(), "drive open sr --dir "+r.w.Root+" '/strategic-review 9531") {
		t.Fatalf("an open refused for a typed-only skill prints the open to run: %v", err)
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
	if !isRefused(err) || !strings.Contains(err.Error(), "drive read "+c.Block[len(c.Block)-6:]+" --screen") {
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

// A session outside the batch is answered by its block id, and the answer
// is recorded under that id, as a refusal or a close of it is.
func TestRespondReachesASessionOutsideTheBatch(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	c.Busy("clean up")
	c.Ask("rm -rf node_modules/.vite")
	short := c.Block[len(c.Block)-6:]
	if _, err := r.d.Respond(r.view(short), "approve"); err != nil {
		t.Fatal(err)
	}
	if ev := r.events(batch.KindRespond); len(ev) != 1 || ev[0].Session != short || ev[0].Action != "approve" {
		t.Fatalf("recorded under its block: %+v", ev)
	}
}

func TestSendArmsTheWakeInAWebhookBatch(t *testing.T) {
	r := newRig(t, "webhook")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	sent, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"})
	if err != nil || !sent.Watched {
		t.Fatalf("a send arms the wake: %+v %v", sent, err)
	}
	argv := r.watches.Running["drive-b1-"+c.Block[len(c.Block)-6:]]
	if strings.Join(argv, " ") != "/bin/drive --batch b1 wait T1 --notify" {
		t.Fatalf("the wake is pinned to its batch: %q", argv)
	}
	if err := r.d.Park("T1", "waits on Qiushi's workspace"); !isRefused(err) || !strings.Contains(err.Error(), "drive read T1") {
		t.Fatalf("a park is decided on a view too: %v", err)
	}
	if err := r.d.Park(r.view("T1"), "waits on Qiushi's workspace"); err != nil {
		t.Fatal(err)
	}
	if r.watches.Alive("drive-" + c.Block[len(c.Block)-6:]) {
		t.Fatal("parking stops the wake")
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
		if _, err := r.d.Notify("drive-b1-"+c.Block[len(c.Block)-6:], label, time.Second, time.Minute); err != nil {
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
	if m, _ := r.d.Metrics(); m.Wakes != 2 || m.Unhandled != 0 {
		t.Fatalf("a wake followed by a re-armed watch was handled: %d of %d unhandled", m.Unhandled, m.Wakes)
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
	if m.Wakes != 5 || m.Unhandled != 1 {
		t.Fatalf("of five wakes, T1's last is the one no driver action followed: %d of %d unhandled", m.Unhandled, m.Wakes)
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
	if _, err := r.d.Open("judge", drive.Step{}, drive.OpenOptions{Dir: r.w.Root, Account: "other@example.com", Model: "fable", Resume: "sid-1"}); err != nil {
		t.Fatal(err)
	}
	want := "PLANLAB_AGENT_NUDGES=off x-other@example.com --model fable --resume sid-1; exec zsh -l"
	if len(got) != 3 || got[0] != "/bin/zsh" || got[1] != "-lic" || got[2] != want {
		t.Fatalf("launch: %q", got)
	}
	// The primary account's launcher is named by the owner's shell, not "primary".
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
		if !strings.Contains(string(body), "which T5s held: T3s") || !strings.Contains(string(body), "drive send T3s@") ||
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
