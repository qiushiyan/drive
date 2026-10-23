package drive_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// A chat driver comes back to a session only through its wake, so every
// action that types or presses into a session leaves it watched, whatever
// came of it.

func watchOf(c *testkit.Claude) string { return "drive-b1-" + c.Block[len(c.Block)-6:] }

// woken retires a session's watch as a watch does once its turn has ended.
func woken(r *rig, c *testkit.Claude) {
	r.watches.Retire(watchOf(c))
}

func TestEveryActionThatTouchesASessionLeavesItWatched(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(r *rig, c *testkit.Claude) error
	}{
		{"an approve", func(r *rig, c *testkit.Claude) error {
			c.Busy("build")
			c.Ask("make build")
			_, err := r.d.Respond(r.view("T1"), "approve")
			return err
		}},
		{"a deny", func(r *rig, c *testkit.Claude) error {
			c.Busy("clean up")
			c.Ask("rm -rf /tmp/x")
			_, err := r.d.Respond(r.view("T1"), "deny")
			return err
		}},
		{"an interrupt", func(r *rig, c *testkit.Claude) error {
			c.Busy("a long build")
			_, err := r.d.Respond(r.view("T1"), "interrupt")
			return err
		}},
		{"a send whose Enter was lost", func(r *rig, c *testkit.Claude) error {
			c.Edit(func(c *testkit.Claude) { c.LoseEnter = true })
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			if err == nil {
				t.Fatal("a lost Enter is not an arrival")
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "webhook")
			c := adopted(t, r, "T1")
			woken(r, c)
			if err := tc.act(r, c); err != nil {
				t.Fatal(err)
			}
			if !r.watches.Alive(watchOf(c)) {
				t.Fatal("the session is left with no wake")
			}
		})
	}
	// An action that touched nothing leaves the session to its driver, and a park unwatches it.
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	woken(r, c)
	c.Busy("working")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{}); !isRefused(err) {
		t.Fatalf("refused: %v", err)
	}
	if r.watches.Alive(watchOf(c)) {
		t.Fatal("a refusal arms nothing")
	}
	if err := r.d.Park(r.view("T1"), "waits on the owner"); err != nil {
		t.Fatal(err)
	}
	if r.watches.Alive(watchOf(c)) {
		t.Fatal("a parked session wakes no one")
	}
}

// The watchdog is one command: it arms what has no wake and needs one, and
// reminds a driver of a wake nothing was done about once per view.
func TestDueArmsWhatHasNoWake(t *testing.T) {
	r := newRig(t, "webhook")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	a, b, p := adopted(t, r, "A"), adopted(t, r, "B"), adopted(t, r, "P")
	// A's watch died; B was woken and nothing was done; P is parked on the owner.
	r.watches.Stop(watchOf(a))
	woken(r, b)
	view := r.view("B")
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "B", HTTP: 204, View: view})
	if err := r.d.Park(r.view("P"), "waits on the owner"); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(11 * time.Minute)
	armed, err := r.d.Due()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range armed {
		got[x.Label] = true
	}
	if !got["A"] || !got["B"] || got["P"] || len(armed) != 2 {
		t.Fatalf("armed the dead watch and the unhandled wake, not the park: %+v", armed)
	}
	// The reminder is posted for the same view; nothing more is due for it.
	woken(r, b)
	r.watches.Retire(watchOf(a))
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "B", HTTP: 204, View: view})
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "A", HTTP: 204, View: r.view("A")})
	r.w.Clock.Sleep(11 * time.Minute)
	if armed, _ := r.d.Due(); len(armed) != 1 || armed[0].Label != "A" {
		t.Fatalf("a wake is re-posted once per view: %+v", armed)
	}
	// B then moves on by itself, as a usage limit's reset continues a turn:
	// that turn's end is news the driver has had no wake for.
	b.Busy("Your usage limit has reset. Continue the task.")
	if armed, _ := r.d.Due(); len(armed) != 1 || armed[0].Label != "B" {
		t.Fatalf("a session that moved on by itself since its wake is due: %+v", armed)
	}
	_ = p
}

// A watch that has begun posting is not one that ended without a wake.
func TestAWakeBeingPostedReadsPosting(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	woken(r, c)
	r.w.Clock.Sleep(time.Second)
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindPosting, Session: "T1", Block: c.Block})
	st, _ := r.d.Status(false)
	if !strings.HasPrefix(st.Rows[0].Watch, "posting") || st.Rows[0].Due != "" {
		t.Fatalf("a post under way is neither lost nor due: %+v", st.Rows[0])
	}
}

// A new batch stops the old one's watches, timers included.
func TestANewBatchStopsTheOldOnesWatches(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	if err := r.d.Timer(5*time.Minute, "veto"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.d.StopWatches(r.b); err != nil || n != 2 {
		t.Fatalf("stopped the watch and the timer: %d %v", n, err)
	}
	if r.watches.Alive(watchOf(c)) || len(r.watches.Running) != 0 {
		t.Fatalf("nothing of the old batch still runs: %v", r.watches.Running)
	}
}

// One unreadable look at Rex is not a session gone.
func TestAMomentaryRexFailureEndsNoWait(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("build")
	r.w.StatusFails = 2
	ended := make(chan *drive.Ending, 1)
	go func() {
		e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
		ended <- e
	}()
	time.Sleep(100 * time.Millisecond)
	c.Done("built")
	if e := <-ended; e == nil || e.State != "done" {
		t.Fatalf("the wait rode out the failure: %+v", e)
	}
}

// An after-wake command that hangs is killed, and none runs for a wake that was never posted.
func TestAfterWakeIsBounded(t *testing.T) {
	r := newRig(t, "webhook")
	code := 204
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	marker := filepath.Join(r.w.Root, "ran")
	r.d.Cfg.AfterWake = []string{"echo ran >> " + marker + "; sleep 30"}
	limit := drive.AfterWakeLimit
	drive.AfterWakeLimit = 200 * time.Millisecond
	defer func() { drive.AfterWakeLimit = limit }()
	c := adopted(t, r, "T1")
	e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
	start := time.Now()
	if err := r.d.Wake("T1", e, ""); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the hung command held the wake")
	}
	if notes := r.events(batch.KindNote); len(notes) != 1 || !strings.Contains(notes[0].Note, "killed") {
		t.Fatalf("the kill is noted: %+v", notes)
	}
	code = 500
	r.d.Sleep = func(time.Duration) {}
	_ = r.d.Wake("T1", e, "")
	if b, _ := os.ReadFile(marker); strings.Count(string(b), "ran") != 1 {
		t.Fatalf("no after-wake command runs for a wake that was never posted: %q", b)
	}
	_ = c
}

// A key Rex refused reached nothing, and arms nothing.
func TestAKeyRexRefusedArmsNoWake(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	woken(r, c)
	c.Busy("build")
	c.Ask("make build")
	r.w.KeyFails = 1
	if _, err := r.d.Respond(r.view("T1"), "approve"); err == nil {
		t.Fatal("the failed press is reported")
	}
	if r.watches.Alive(watchOf(c)) {
		t.Fatal("nothing reached the session, so nothing is armed")
	}
}

// An open refused on a hold waits for it like a send, and the holder's wake
// names the open to run.
func TestARefusedOpenWaitsForItsHold(t *testing.T) {
	r := newRig(t, "webhook")
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	a := adopted(t, r, "T5")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.d.Open("sr-1", drive.Step{Line: "/strategic-review 9531"}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work"), Hold: "release"})
	if !isRefused(err) {
		t.Fatalf("the open waits for the hold: %v", err)
	}
	if st, _ := r.d.Status(false); len(st.Holds) != 1 || !strings.Contains(st.Holds[0], "sr-1") {
		t.Fatalf("status lists the open among the waiters: %q", st.Holds)
	}
	a.Done("released")
	woken(r, a)
	e, _ := r.d.Wait("T5", time.Minute, 10*time.Minute)
	if err := r.d.Wake("T5", e, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "drive open sr-1") || !strings.Contains(body, "--hold release") {
		t.Fatalf("the holder's wake names the open to run: %s", body)
	}
}

// A wake for a rejected login prints the relaunch with the step the session
// was last sent, which a driver with no memory cannot supply.
func TestALoginWakeRelaunchesWithTheLastStep(t *testing.T) {
	r := newRig(t, "none")
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "other@example.com", "sessions"), 0o755)
	c := adopted(t, r, "T4")
	c.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) {
			c.Fail("authentication_failed", "Login expired · Please run /login")
		}
	})
	if _, err := r.d.Send(r.view("T4"), drive.Step{Line: "implement-spec"}, drive.SendOptions{}); err == nil {
		t.Fatal("the rejected login fails the send")
	}
	e, err := r.d.Wait("T4", time.Minute, 10*time.Minute)
	if err != nil || e.State != "blocked:auth" {
		t.Fatalf("%+v %v", e, err)
	}
	if !strings.Contains(e.Next, "--account other@example.com implement-spec") || strings.Contains(e.Next, "<the step>") {
		t.Fatalf("the relaunch carries the last step: %s", e.Next)
	}
}
