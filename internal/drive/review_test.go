package drive_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/testkit"
)

// adopted puts a session in the batch under label with one finished step,
// so later sends carry no notice.
func adopted(t *testing.T, r *rig, label string) *testkit.Claude {
	t.Helper()
	c := r.w.Add(label)
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "start"}, drive.SendOptions{Label: label}); err != nil {
		t.Fatal(err)
	}
	return c
}

// admitted waits until a send of text to session is on record: past its
// admission, on its way to the session.
func admitted(r *rig, session, text string) {
	for i := 0; i < 2000; i++ {
		for _, e := range r.events(batch.KindSendIntent) {
			if e.Session == session && e.Text == text {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	r.t.Fatalf("no send of %q to %s was admitted", text, session)
}

// A send that has recorded its hold but not yet finished typing holds it:
// a second session's send for the same resource waits.
func TestAHoldIsTakenWhenTheSendIsAdmitted(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T5")
	adopted(t, r, "T3")
	gate := make(chan struct{})
	a.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
	va, vb := r.view("T5"), r.view("T3")
	first := make(chan error, 1)
	go func() {
		_, err := r.d.Send(va, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
		first <- err
	}()
	// Let the first send get past admission and block in Type.
	admitted(r, "T5", "release")
	_, err := r.d.Send(vb, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
	close(gate)
	if ferr := <-first; ferr != nil {
		t.Fatalf("first send: %v", ferr)
	}
	if !isRefused(err) || !strings.Contains(err.Error(), "held by T5") {
		t.Fatalf("a hold belongs to the send that took it from admission on, got %v", err)
	}
}

// A turn that ends while the send is still confirming releases its hold.
func TestAQuickTurnReleasesItsHold(t *testing.T) {
	r := newRig(t, "none")
	adopted(t, r, "T5")
	adopted(t, r, "T3")
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	// T5's fake answered at once: its turn is over.
	if _, err := r.d.Send(r.view("T3"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("a hold whose turn already ended is free: %v", err)
	}
}

// The old send-turn required a new prompt; a prompt seconds old with the same
// text is not this send's.
func TestALostEnterIsNotTakenForAnArrival(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	c.Edit(func(c *testkit.Claude) { c.LoseEnter = true })
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if err == nil {
		t.Fatalf("a line that never reached the session is not arrived: %+v", sent)
	}
	var unknown int
	for _, e := range r.events(batch.KindSend) {
		if e.Outcome == "unknown" {
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatalf("a send nobody can confirm is recorded unknown, not lost: %+v", r.events(batch.KindSend))
	}
	m, _ := r.d.Metrics("")
	if m.Unknown != 1 {
		t.Fatalf("metrics count it: %+v", m.Unknown)
	}
}

// Typing Enter into a permission prompt answers it.
func TestQueueNeverTypesIntoAPrompt(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("clean up")
	c.Ask("rm -rf build")
	typed0, keys0 := c.Seen()
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "also this"}, drive.SendOptions{Queue: true})
	if !isRefused(err) || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("a session on a permission prompt takes no send, queued or not: %v", err)
	}
	if typed, keys := c.Seen(); len(typed) != len(typed0) || len(keys) != len(keys0) {
		t.Fatalf("nothing typed or pressed: %q %q", typed, keys)
	}
}

func TestANoticeTurnThatStopsOnAPromptHoldsTheStep(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	c.Reply = func(c *testkit.Claude, _ string) { c.Ask("cat NOTICE.md") }
	_, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "/write-spec go"}, drive.SendOptions{Label: "T1"})
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("the step waits when the notice's turn stops on a prompt: %v", err)
	}
	if typed, _ := c.Seen(); len(typed) != 1 || !strings.HasPrefix(typed[0], "Read ") {
		t.Fatalf("only the notice went: %q", typed)
	}
}

// noticeSent says whether a session's notice has an outcome on record.
func noticeSent(r *rig, label string) bool {
	for _, e := range r.events(batch.KindSend) {
		if e.Mode == "notice" && e.Session == label {
			return true
		}
	}
	return false
}

// A typed first step follows the notice once the notice's turn has ended.
// The hold the step takes stands through that gap, until the step itself
// has gone: the notice's arrival is not the step's.
func TestAHoldStandsBetweenTheNoticeAndItsStep(t *testing.T) {
	r := newRig(t, "none")
	adopted(t, r, "T3")
	c := r.w.Add("T5")
	done(c, "onboard", "analysis")
	gate := make(chan struct{})
	c.Edit(func(c *testkit.Claude) {
		// The notice's turn ends, and the step's typing is caught after it.
		c.Reply = func(c *testkit.Claude, _ string) {
			c.Edit(func(c *testkit.Claude) { c.TypeGate, c.Reply = gate, nil })
			c.Done("read it")
		}
	})
	v := r.view(c.Block[len(c.Block)-6:])
	first := make(chan error, 1)
	go func() {
		_, err := r.d.Send(v, drive.Step{Line: "release"}, drive.SendOptions{Label: "T5", Hold: "release"})
		first <- err
	}()
	for i := 0; i < 2000 && !noticeSent(r, "T5"); i++ {
		time.Sleep(time.Millisecond)
	}
	_, err := r.d.Send(r.view("T3"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
	close(gate)
	if ferr := <-first; ferr != nil {
		t.Fatalf("T5's step goes after its notice: %v", ferr)
	}
	if !isRefused(err) || !strings.Contains(err.Error(), "held by T5") {
		t.Fatalf("the hold stands between the notice and the step: %v", err)
	}
}

// A notice nobody could confirm may never have reached the session: the
// step sent after it carries it again.
func TestANoticeLeftUnknownGoesAgain(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	c.Edit(func(c *testkit.Claude) { c.LoseEnter = true })
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"}); err == nil {
		t.Fatal("a notice whose Enter was lost is not an arrival")
	}
	c.Edit(func(c *testkit.Claude) { c.LoseEnter = false })
	if _, err := r.d.Respond(r.view("T1"), "clear"); err != nil {
		t.Fatal(err)
	}
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "go"}, drive.SendOptions{})
	if err != nil || !sent.Notice {
		t.Fatalf("the next send carries the notice: %+v %v", sent, err)
	}
	if typed, _ := c.Seen(); len(typed) != 3 || !strings.HasPrefix(typed[1], "Read ") || typed[2] != "go" {
		t.Fatalf("the notice, then the step: %q", typed)
	}
}

// dyingTerm ends the driver's run the moment it would type line, as a shell
// that timed out in the notice's turn kills it: what it deferred runs, as
// the lock's release does when a process dies, and nothing else.
type dyingTerm struct {
	*testkit.World
	line string
}

func (d *dyingTerm) Type(block, text string) error {
	if text == d.line {
		runtime.Goexit()
	}
	return d.World.Type(block, text)
}

// A driver gone after the notice leaves its step in flight, for the next
// driver of the session to settle from the transcript.
func TestADriverGoneAfterTheNoticeLeavesItsStepToSettle(t *testing.T) {
	r := newRig(t, "none")
	c := r.w.Add("T5")
	done(c, "onboard", "analysis")
	v := r.view(c.Block[len(c.Block)-6:])
	r.d.Term = &dyingTerm{World: r.w, line: "release"}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = r.d.Send(v, drive.Step{Line: "release"}, drive.SendOptions{Label: "T5", Hold: "release"})
	}()
	<-gone
	r.d.Term = r.w
	st, _ := r.d.Status(false)
	if len(st.InFlight) != 1 || st.InFlight[0].Text != "release" || st.InFlight[0].Driver != nil {
		t.Fatalf("the step is in flight with its driver gone: %+v", st.InFlight)
	}
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("the next driver sends it: %v", err)
	}
	settled := 0
	for _, e := range append(r.events(batch.KindSend), r.events(batch.KindFailure)...) {
		if e.Intent == st.InFlight[0].Intent {
			if e.Kind != batch.KindFailure {
				t.Fatalf("the notice's arrival is not the step's: %+v", e)
			}
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("the step that never went is settled once, as not sent: %d", settled)
	}
}

// A line Claude Code would not record as typed is refused before anything,
// the notice included, reaches the session.
func TestAStepRefusedBeforeAnyEffect(t *testing.T) {
	for _, line := range []string{
		strings.Repeat("x", 600), // Claude Code keeps only the end
		"run the suite\tthen merge",
		"merge it\rnow",
		"!rm -rf build",   // a shell command
		"# always rebase", // a memory
	} {
		t.Run(clipName(line), func(t *testing.T) {
			r := newRig(t, "none")
			c := r.w.Add("T1")
			done(c, "onboard", "analysis")
			_, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: line}, drive.SendOptions{Label: "T1"})
			if !isRefused(err) {
				t.Fatalf("refused: %v", err)
			}
			if typed, _ := c.Seen(); len(typed) != 0 {
				t.Fatalf("refused means nothing was sent, the notice included: %q", typed)
			}
			if len(r.events(batch.KindFailure)) != 0 {
				t.Fatalf("a refusal is not a failure: %+v", r.events(batch.KindFailure))
			}
		})
	}
}

func clipName(s string) string {
	if len(s) > 20 {
		return s[:20]
	}
	return s
}

// The owner typing into the box as drive types: the session receives a
// line that is neither the step nor its tail. It reached the session, so
// the hold its step took stands, and the message says to read it before
// interrupting anything.
func TestALineReceivedOtherwiseReachedTheSession(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T5")
	adopted(t, r, "T3")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the release runs on
	va, vb := r.view("T5"), r.view("T3")
	owner := true
	r.d.Sleep = func(d time.Duration) {
		if owner {
			owner = false
			a.Edit(func(c *testkit.Claude) { c.Input += " and the changelog" })
		}
		r.w.Clock.Sleep(d)
	}
	sent, err := r.d.Send(va, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
	var f *drive.Failed
	if !errors.As(err, &f) || sent == nil || sent.Outcome != "other" {
		t.Fatalf("a line received as something else is a send that reached the session: %+v %v", sent, err)
	}
	if !strings.Contains(f.Next, "drive read T5") || !strings.Contains(f.Next, "drive respond T5@") {
		t.Fatalf("it says to read before interrupting, with the view to interrupt on: %q", f.Next)
	}
	if _, err := r.d.Send(vb, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by T5") {
		t.Fatalf("the hold stands while that turn runs: %v", err)
	}
}

func TestStatusReadsManySessions(t *testing.T) {
	r := newRig(t, "none")
	for i := 0; i < 20; i++ {
		adopted(t, r, fmt.Sprintf("S%d", i))
	}
	st, err := r.d.Status(false)
	if err != nil || len(st.Rows) != 20 {
		t.Fatalf("status: %d rows, %v", len(st.Rows), err)
	}
	for i, row := range st.Rows {
		if row.Label != fmt.Sprintf("S%d", i) || row.State != "done" {
			t.Fatalf("row %d: %+v", i, row)
		}
	}
}

type prs map[string]progress.Steps

func (p prs) Steps(id string) (progress.Steps, error) {
	if s, ok := p[id]; ok {
		return s, nil
	}
	return progress.Steps{}, errors.New("claude-steps: no such session")
}

func (p prs) PR(ref progress.PRRef) (progress.PR, error) {
	return progress.PR{Number: ref.Number, State: "MERGED", MergedAt: time.Now().Add(time.Hour)}, nil
}

// The builder and its strategic reviewer name the same pull request, and
// the builder an earlier one it landed first.
func TestMetricsCountAMergedChangeOnce(t *testing.T) {
	r := newRig(t, "none")
	b, s := adopted(t, r, "T1"), adopted(t, r, "sr-9531")
	adopted(t, r, "T2")
	first, ref := progress.PRRef{Repo: "planlab-ai/main", Number: 9520}, progress.PRRef{Repo: "planlab-ai/main", Number: 9531}
	r.d.Progress = prs{b.SID: {PR: &ref, PRs: []progress.PRRef{first, ref}}, s.SID: {PR: &ref, PRs: []progress.PRRef{ref}}}
	m, err := r.d.Metrics("")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Merged) != 2 {
		t.Fatalf("every pull request a session linked counts, each once however many sessions name it: %+v", m.Merged)
	}
	for _, p := range m.Merged {
		if want := map[int]int{9520: 1, 9531: 2}[p.Number]; len(p.Sessions) != want {
			t.Fatalf("#%d is named by %d sessions: %+v", p.Number, want, p.Sessions)
		}
	}
	if m.Unread != 1 {
		t.Fatalf("a session whose progress could not be read is counted, so the measure says it is incomplete: %+v", m.Unread)
	}
}

func TestRespondAndCloseAreRecorded(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("clean up")
	c.Ask("rm -rf node_modules/.vite")
	if _, err := r.d.Respond(r.view("T1"), "approve"); err != nil {
		t.Fatal(err)
	}
	ev := r.events(batch.KindRespond)
	if len(ev) != 1 || ev[0].Action != "approve" || !strings.Contains(ev[0].Call, "node_modules/.vite") {
		t.Fatalf("an approval is recorded with what it approved: %+v", ev)
	}
	c.Done("cleaned")
	if err := r.d.Close(r.view("T1")); err != nil {
		t.Fatal(err)
	}
	if ev := r.events(batch.KindClose); len(ev) != 1 || ev[0].Session != "T1" {
		t.Fatalf("a close is recorded: %+v", ev)
	}
}

// The race the per-session lock closes: driver B reads T1, driver A sends
// and finishes between B's read and B's send, and B sends on the old turn.
func TestADecisionOvertakenMidReadIsRefused(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	v := r.view("T1")
	typed0, _ := c.Seen()
	var aErr error
	r.w.OnCapture = func() {
		_, aErr = r.d.Send(v, drive.Step{Line: "recap"}, drive.SendOptions{})
	}
	_, bErr := r.d.Send(v, drive.Step{Line: "closeout"}, drive.SendOptions{})
	typed, _ := c.Seen()
	if n := len(typed) - len(typed0); n != 1 || (aErr == nil) == (bErr == nil) {
		t.Fatalf("one of two sends decided on one view goes: %d typed, A %v, B %v", n, aErr, bErr)
	}
	for _, err := range []error{aErr, bErr} {
		if err != nil && (!isRefused(err) || !strings.Contains(err.Error(), "another driver is sending T1")) {
			t.Fatalf("the loser hears another driver is sending: %v", err)
		}
	}
}

// Respond and close are actions on a session too.
func TestCloseWaitsForADriverMidSend(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	v := r.view("T1")
	gate := make(chan struct{})
	c.Edit(func(c *testkit.Claude) { c.TypeGate = gate })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = r.d.Send(v, drive.Step{Line: "recap"}, drive.SendOptions{})
	}()
	admitted(r, "T1", "recap")
	err := r.d.Close(v)
	close(gate)
	wg.Wait()
	if !isRefused(err) || !strings.Contains(err.Error(), "another driver") {
		t.Fatalf("a close while another driver sends is refused: %v", err)
	}
	if r.w.Session(c.Block) == nil {
		t.Fatal("the block is still open")
	}
}

// The block is on record the moment Rex opens it.
func TestAnOpenThatNeverStartsLeavesItsBlockOnRecord(t *testing.T) {
	r := newRig(t, "none")
	r.w.OnOpen = func(c *testkit.Claude) { c.Running = false }
	_, err := r.d.Open("T9", drive.Step{}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work")})
	if err == nil {
		t.Fatal("Claude Code never started")
	}
	opens := r.events(batch.KindOpen)
	if len(opens) != 1 || opens[0].Block == "" || opens[0].Session != "T9" {
		t.Fatalf("the block is recorded: %+v", opens)
	}
	if st, _ := r.d.Status(false); len(st.InFlight) != 0 {
		t.Fatalf("the open is resolved: %+v", st.InFlight)
	}
}

// An open whose step would be refused opens nothing.
func TestAnOpenChecksItsStepFirst(t *testing.T) {
	r := newRig(t, "none")
	_, err := r.d.Open("T9", drive.Step{Line: strings.Repeat("x", 600)}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work")})
	if !isRefused(err) {
		t.Fatalf("refused: %v", err)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 0 {
		t.Fatalf("no block opened: %+v", blocks)
	}
}

func TestASendWhoseWakeFailsSaysSo(t *testing.T) {
	r := newRig(t, "webhook")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	r.watches.Fail = errors.New("fork/exec: no such file")
	sent, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"})
	if err == nil || !strings.Contains(err.Error(), "wake did not start") || !strings.Contains(err.Error(), "drive watch T1") {
		t.Fatalf("a send whose wake failed says so: %v", err)
	}
	if sent == nil || sent.Outcome != "arrived" || sent.Watched {
		t.Fatalf("the step itself went: %+v", sent)
	}
}

// A watch takes itself off the books before it posts, so a send made while
// the old watch is still posting arms a new one.
func TestASendDuringAWakeArmsAFreshWatch(t *testing.T) {
	r := newRig(t, "webhook")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"}); err != nil {
		t.Fatal(err)
	}
	name := "drive-b1-" + c.Block[len(c.Block)-6:]
	if !r.watches.Alive(name) {
		t.Fatalf("armed as %s: %v", name, r.watches.Running)
	}
	if _, err := r.d.Notify(name, "T1", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if r.watches.Alive(name) {
		t.Fatal("the watch retired before it posted")
	}
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "next"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if !r.watches.Alive(name) || len(r.events(batch.KindWatch)) != 2 {
		t.Fatal("the next send armed a fresh watch")
	}
}

// No hook fires on a deny: Rex keeps reading blocked. The session's own
// record says the turn ended, and the next step goes.
func TestADeniedPromptEndsTheTurn(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("clean up")
	c.Ask("rm -rf /tmp/x")
	res, err := r.d.Respond(r.view("T1"), "deny")
	if err != nil || res.After != "interrupted" {
		t.Fatalf("a deny is confirmed from the transcript: %+v %v", res, err)
	}
	if st, _ := r.w.Status(c.Block); st.State != "blocked" {
		t.Fatalf("the fake keeps Rex's record, as the real one does: %+v", st)
	}
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "skip that; go on"}, drive.SendOptions{}); err != nil {
		t.Fatalf("the next step goes after a deny: %v", err)
	}
}

// drive prescribes an interrupt after a garbled line; the wait that follows
// has to end.
func TestAnInterruptedTurnEndsTheWait(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("a long build")
	if _, err := r.d.Respond(r.view("T1"), "interrupt"); err != nil {
		t.Fatal(err)
	}
	ended := make(chan *drive.Ending, 1)
	go func() {
		e, _ := r.d.Wait("T1", time.Minute, 10*time.Minute)
		ended <- e
	}()
	select {
	case e := <-ended:
		if e == nil || e.State != "interrupted" || !strings.Contains(e.Text(), "next: ") {
			t.Fatalf("the wait ends on the interrupt, saying what next: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never ended: Rex still reads working")
	}
	if st, _ := r.d.Status(false); st.Rows[0].State != "interrupted" {
		t.Fatalf("status reads it: %+v", st.Rows[0])
	}
}

// The relaunch drive prescribes for a rejected login runs to the end: the
// resumed session reads the old error in the transcript it shares.
func TestARelaunchedSessionTakesItsStep(t *testing.T) {
	r := newRig(t, "none")
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "other@example.com", "sessions"), 0o755)
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "third@example.com", "sessions"), 0o755)
	c := adopted(t, r, "T4")
	c.Prompt("implement-spec")
	c.Fail("authentication_failed", "Login expired · Please run /login")
	c.Set("blocked", "auth", "Login expired · Please run /login")
	_, err := r.d.Send(r.view("T4"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "--resume "+c.SID+" --account other@example.com '<the step>'") {
		t.Fatalf("the refusal prints the relaunch with the step on it: %v", err)
	}
	if err := r.d.Close(r.view("T4")); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(time.Minute) // the driver reads the refusal and relaunches
	// Resumed without its step, it still shows the old error until it takes one.
	o, err := r.d.Open("T4", drive.Step{}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work"), Resume: c.SID, Account: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if rd, _ := r.d.Read("T4", drive.ReadLast, 1); rd.State != "done" {
		t.Fatalf("the error belongs to the process it replaced: %+v", rd)
	}
	sent, err := r.d.Send(r.view("T4"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" || sent.Notice {
		t.Fatalf("the resumed session takes the step, with no second notice: %+v %v", sent, err)
	}
	// A second rejected login steers the next relaunch clear of both accounts.
	_ = r.b.Append(batch.Event{Kind: batch.KindFailure, Session: "T4", Account: "third@example.com", Reason: "login was rejected"})
	n := r.w.Session(o.Block)
	n.Prompt("go on")
	n.Fail("authentication_failed", "Login expired · Please run /login")
	n.Set("blocked", "auth", "Login expired")
	_, err = r.d.Send(r.view("T4"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "every other account's login was rejected") {
		t.Fatalf("no account known to be dead is offered: %v", err)
	}
}

// merge-when-ready ends its turn waiting on checks in a background shell;
// the release stays held until that shell is done.
func TestAHoldLastsWhileItsTurnsShellRuns(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T3")
	adopted(t, r, "T5")
	a.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) {
			c.Edit(func(c *testkit.Claude) { c.Shells = append(c.Shells, r.w.Clock.Now()) })
			c.Done("waiting on the required checks in the background")
		}
	})
	if _, err := r.d.Send(r.view("T3"), drive.Step{Line: "merge-when-ready"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "merge-when-ready"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by T3") {
		t.Fatalf("held while the checks run: %v", err)
	}
	a.Edit(func(c *testkit.Claude) { c.Shells = nil })
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "merge-when-ready"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatalf("free once they end: %v", err)
	}
}

// A detached watch is a real process outside any terminal: it runs, a second
// one by its name is refused, Retire takes only its own off the books, and
// Stop ends it.
func TestADetachedWatchRunsAndStops(t *testing.T) {
	w := drive.Detached{Dir: t.TempDir()}
	log := filepath.Join(t.TempDir(), "watches.log")
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err != nil {
		t.Fatal(err)
	}
	if !w.Alive("drive-test") {
		t.Fatal("the watch runs")
	}
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err == nil {
		t.Fatal("a second watch by one name is refused")
	}
	if w.Retire("drive-test") {
		t.Fatal("only the watch's own process retires it")
	}
	if err := w.Stop("drive-test"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && w.Alive("drive-test"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if w.Alive("drive-test") {
		t.Fatal("stopped")
	}
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err != nil {
		t.Fatalf("the name is free again: %v", err)
	}
	_ = w.Stop("drive-test")
}

// A queued line that lost its start waits behind the session's own turn:
// Ctrl+C would stop that turn, a build or a merge, so drive leaves it.
func TestAGarbledQueuedLineInterruptsNothing(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("pnpm build")
	c.Edit(func(c *testkit.Claude) { c.DropHead = 3 })
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "also update the changelog"}, drive.SendOptions{Queue: true})
	if err == nil || sent == nil || sent.Outcome != "garbled" {
		t.Fatalf("reported, not retyped: %+v %v", sent, err)
	}
	if _, keys := c.Seen(); contains(keys, "ctrl+c") {
		t.Fatalf("the running turn is left alone: %q", keys)
	}
}
