package drive_test

import (
	"errors"
	"fmt"
	"maps"
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
	"github.com/qiushiyan/drive/internal/session"
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
	for range 2000 {
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
	m, _ := r.d.Metrics()
	if m.Unknown != 1 {
		t.Fatalf("metrics count it: %+v", m.Unknown)
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
	if typed, _ := c.Seen(); len(typed) != 1 || !strings.HasPrefix(typed[0], "@") {
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

// A typed first step the notice cannot ride, such as a command no skill file
// backs, follows the notice once the notice's turn has ended. The hold the
// step takes stands through that gap, until the step itself has gone: the
// notice's arrival is not the step's.
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
		_, err := r.d.Send(v, drive.Step{Line: "/release"}, drive.SendOptions{Label: "T5", Hold: "release"})
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
// step sent after it carries it again, in a copy of its own.
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
	if typed, _ := c.Seen(); len(typed) != 2 || !strings.HasPrefix(typed[1], "go @") || typed[1] == typed[0] {
		t.Fatalf("the step, with the notice again: %q", typed)
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
	r.d.Term = &dyingTerm{World: r.w, line: "/release"}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = r.d.Send(v, drive.Step{Line: "/release"}, drive.SendOptions{Label: "T5", Hold: "release"})
	}()
	<-gone
	r.d.Term = r.w
	st, _ := r.d.Status(false)
	if len(st.InFlight) != 1 || st.InFlight[0].Text != "/release" || st.InFlight[0].Driver != nil {
		t.Fatalf("the step is in flight with its driver gone: %+v", st.InFlight)
	}
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "/release"}, drive.SendOptions{Hold: "release"}); err != nil {
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
	if !strings.Contains(f.Next, "drive --batch b1 read T5") || !strings.Contains(f.Next, "drive --batch b1 respond T5@") {
		t.Fatalf("it says to read before interrupting, with the view to interrupt on: %q", f.Next)
	}
	if _, err := r.d.Send(vb, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); !isRefused(err) || !strings.Contains(err.Error(), "held by T5") {
		t.Fatalf("the hold stands while that turn runs: %v", err)
	}
	if m, _ := r.d.Metrics(); m.Other != 1 {
		t.Fatalf("metrics count the line received as something else: %d", m.Other)
	}
}

func TestStatusReadsManySessions(t *testing.T) {
	r := newRig(t, "none")
	for i := range 20 {
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

func (p prs) Landed(string, []string, []progress.Commit) (progress.Landing, error) {
	return progress.Landing{}, errors.New("gwt: not here")
}

// landing is prs with what git and gwt answer: the branch each directory
// has checked out, the id of each commit by its subject, and the branches
// and commits merged into main. A branch absent from both maps was removed.
type landing struct {
	prs
	at, ids map[string]string
	merged  map[string]bool
}

func (l landing) Landed(dir string, branches []string, commits []progress.Commit) (progress.Landing, error) {
	out := progress.Landing{Repo: "/work/tool", Trunk: "main", Branches: append([]string{}, branches...), Merged: map[string]bool{"main": true}}
	for _, c := range commits {
		if b, ok := l.at[c.Dir]; ok {
			out.Branches = append(out.Branches, b)
		}
	}
	if n := len(commits); n > 0 {
		out.Commit = l.ids[commits[n-1].Text]
	}
	maps.Copy(out.Merged, l.merged)
	return out, nil
}

// A change merged into the trunk without a pull request, as a personal
// tool's closeout merges it, counts as merged: the session's newest commit
// is in the trunk, though the closeout removed its branch, or its branch
// merged.
func TestMetricsCountAChangeMergedWithoutAPullRequest(t *testing.T) {
	r := newRig(t, "none")
	worked := func(c *testkit.Claude, branch string) {
		c.Write(map[string]any{"type": "attachment", "gitBranch": branch}, map[string]any{"type": "attachment", "gitBranch": "main"})
	}
	a, b, c, e := adopted(t, r, "T1"), adopted(t, r, "T2"), adopted(t, r, "T3"), adopted(t, r, "T4")
	worked(a, "shout")
	worked(b, "subtract")
	worked(e, "wip")
	at := time.Now()
	r.d.Progress = landing{
		prs: prs{
			a.SID: {Commits: []progress.Commit{{At: at, Text: "Add --shout", Dir: "/work/tool/.wt/shout"}}}, // its closeout removed the worktree and the branch
			b.SID: {},
			c.SID: {Commits: []progress.Commit{{At: at, Text: "Add publish.py", Dir: "/work/tool/.wt/publish"}}}, // worked there through its shell: its transcript names only main
			e.SID: {},
		},
		at:     map[string]string{"/work/tool/.wt/publish": "publish"},
		ids:    map[string]string{"Add --shout": "c0ffee1", "Add publish.py": "beef002"},
		merged: map[string]bool{"c0ffee1": true, "subtract": true, "beef002": true, "wip": false},
	}
	if err := r.d.Close(r.view("T1")); err != nil {
		t.Fatal(err)
	}
	m, err := r.d.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range m.Merged {
		got = append(got, p.Branch+"@"+p.Commit+":"+strings.Join(p.Sessions, ","))
	}
	if strings.Join(got, " ") != "shout@c0ffee1:T1 subtract@:T2 publish@beef002:T3" || m.Unread != 0 {
		t.Fatalf("each landed change once, the unmerged one not: %v (unread %d)", got, m.Unread)
	}
}

// The builder and its strategic reviewer name the same pull request, and
// the builder an earlier one it landed first.
func TestMetricsCountAMergedChangeOnce(t *testing.T) {
	r := newRig(t, "none")
	b, s := adopted(t, r, "T1"), adopted(t, r, "sr-9531")
	adopted(t, r, "T2")
	first, ref := progress.PRRef{Repo: "planlab-ai/main", Number: 9520}, progress.PRRef{Repo: "planlab-ai/main", Number: 9531}
	r.d.Progress = prs{b.SID: {PR: &ref, PRs: []progress.PRRef{first, ref}}, s.SID: {PR: &ref, PRs: []progress.PRRef{ref}}}
	m, err := r.d.Metrics()
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

// A response the session's state does not call for is refused, and nothing
// is pressed.
func TestRespondRefusesWhatTheSessionDoesNotWaitOn(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		state        func(c *testkit.Claude)
	}{
		{"approve with no prompt", "approve", func(*testkit.Claude) {}},
		{"deny with no prompt", "deny", func(*testkit.Claude) {}},
		{"interrupt with no turn running", "interrupt", func(*testkit.Claude) {}},
		{"clear with nothing typed", "clear", func(*testkit.Claude) {}},
		// A prompt's options sit behind the ❯ the input box uses (seen live).
		{"clear on a prompt", "clear", func(c *testkit.Claude) { c.Busy("clean up"); c.Ask("rm -rf build") }},
		{"an unknown response", "yes", func(*testkit.Claude) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "none")
			c := adopted(t, r, "T1")
			tc.state(c)
			_, keys0 := c.Seen()
			_, err := r.d.Respond(r.view("T1"), tc.action)
			if !isRefused(err) {
				t.Fatalf("refused: %v", err)
			}
			if _, keys := c.Seen(); len(keys) != len(keys0) {
				t.Fatalf("nothing pressed: %q", keys[len(keys0):])
			}
		})
	}
}

// Every action is decided on a view and refused once the session has moved
// past it, in a state where it would otherwise go: nothing typed, pressed,
// parked or closed, and the refusal recorded once, as metrics count it.
func TestAnActionOnAViewTheSessionMovedPastIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		working bool // an interrupt goes on a running turn; the others on a finished one
		act     func(r *rig, view string) error
	}{
		{"a send", false, func(r *rig, v string) error {
			_, err := r.d.Send(v, drive.Step{Line: "recap"}, drive.SendOptions{})
			return err
		}},
		{"an interrupt", true, func(r *rig, v string) error { _, err := r.d.Respond(v, "interrupt"); return err }},
		{"a park", false, func(r *rig, v string) error { return r.d.Park(v, "waits on the owner") }},
		{"a close", false, func(r *rig, v string) error { return r.d.Close(v) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "none")
			c := adopted(t, r, "T1")
			c.Busy("build A")
			old := r.view("T1")
			c.Done("A built")
			c.Busy("build B")
			if !tc.working {
				c.Done("B built")
			}
			typed0, keys0 := c.Seen()
			if err := tc.act(r, old); !isRefused(err) || !strings.Contains(err.Error(), "moved past") {
				t.Fatalf("refused on the old view: %v", err)
			}
			if typed, keys := c.Seen(); len(typed) != len(typed0) || len(keys) != len(keys0) || r.w.Session(c.Block) == nil || len(r.events(batch.KindPark)) != 0 {
				t.Fatalf("nothing typed, pressed, parked or closed: %q %q", typed[len(typed0):], keys[len(keys0):])
			}
			if n := len(r.events(batch.KindRefused)); n != 1 {
				t.Fatalf("the refusal is recorded once, got %d", n)
			}
		})
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
	wg.Go(func() {
		_, _ = r.d.Send(v, drive.Step{Line: "recap"}, drive.SendOptions{})
	})
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

// An open whose hold another session holds opens nothing, and says how to
// open it once the hold is free.
func TestAnOpenChecksItsHoldFirst(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T5")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // the release runs on
	if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "/release"}, drive.SendOptions{Hold: "release"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.w.Root, "work")
	_, err := r.d.Open("T9", drive.Step{Line: "release"}, drive.OpenOptions{Dir: dir, Hold: "release"})
	if !isRefused(err) || !strings.Contains(err.Error(), "held by T5") || !strings.Contains(err.Error(), "drive --batch b1 open T9 --dir "+dir+" --hold release release") {
		t.Fatalf("refused, with the open to run once the hold is free: %v", err)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 1 {
		t.Fatalf("no block opened: %d blocks", len(blocks))
	}
}

// Once the block is open, a step stopped by a hold another session took
// meanwhile is a failure that says the block is open, not a refusal.
func TestAnOpenWhoseHoldIsTakenWhileItOpensFails(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T3")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	v := r.view("T3")
	var taken error
	r.d.Term = &recordingTerm{World: r.w, run: func([]string) {
		_, taken = r.d.Send(v, drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
	}}
	o, err := r.d.Open("T9", drive.Step{Line: "release"}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work"), Hold: "release"})
	if taken != nil {
		t.Fatal(taken)
	}
	var f *drive.Failed
	if isRefused(err) || !errors.As(err, &f) || !strings.Contains(f.What, "held by T3") || o == nil || r.w.Session(o.Block) == nil {
		t.Fatalf("a failure naming the open block: %+v %v", o, err)
	}
}

// Two drivers opening one label at once open one block.
func TestTwoOpensOfOneLabelOpenOneBlock(t *testing.T) {
	r := newRig(t, "none")
	dir := filepath.Join(r.w.Root, "work")
	var second error
	r.d.Term = &recordingTerm{World: r.w, run: func([]string) {
		if second == nil {
			second = errors.New("not run")
			_, second = r.d.Open("T9", drive.Step{Line: "go"}, drive.OpenOptions{Dir: dir})
		}
	}}
	if _, err := r.d.Open("T9", drive.Step{Line: "go"}, drive.OpenOptions{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if !isRefused(second) || !strings.Contains(second.Error(), "another driver is opening T9") {
		t.Fatalf("the second is refused while the first opens: %v", second)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 1 {
		t.Fatalf("one block: %d", len(blocks))
	}
}

func TestASendWhoseWakeFailsSaysSo(t *testing.T) {
	r := newRig(t, "webhook")
	c := r.w.Add("T1")
	done(c, "onboard", "analysis")
	r.watches.Fail = errors.New("fork/exec: no such file")
	sent, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "go"}, drive.SendOptions{Label: "T1"})
	if err == nil || !strings.Contains(err.Error(), "wake did not start") || !strings.Contains(err.Error(), "drive --batch b1 watch T1") {
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
	var seen, freed sync.Once
	posting, release := make(chan struct{}), make(chan struct{})
	free := func() { freed.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Do(func() { close(posting) })
		<-release
	}))
	defer srv.Close()
	defer free() // a failing check lets the post finish rather than hang the test
	r.d.Cfg.Webhook = config.Webhook{URL: srv.URL}
	c := adopted(t, r, "T1")
	name := watchOf(c)
	woke := make(chan error, 1)
	go func() {
		_, err := r.d.Notify(name, "T1", time.Second, time.Minute)
		woke <- err
	}()
	<-posting
	if r.watches.Alive(name) {
		t.Fatal("the watch retired before it posted")
	}
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "next"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if !r.watches.Alive(name) || len(r.events(batch.KindWatch)) != 2 {
		t.Fatal("the send made while the wake posted armed a fresh watch")
	}
	free()
	if err := <-woke; err != nil {
		t.Fatal(err)
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

// shellWords splits a command as a shell would: words apart on spaces,
// '…' quoting, '\” a quote inside.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	var w strings.Builder
	in, started := false, false
	for _, r := range s {
		switch {
		case r == '\'':
			in, started = !in, true
		case r == ' ' && !in:
			if started {
				out, started = append(out, w.String()), false
				w.Reset()
			}
		default:
			w.WriteRune(r)
			started = true
		}
	}
	if in {
		t.Fatalf("unbalanced quote in %q", s)
	}
	if started {
		out = append(out, w.String())
	}
	return out
}

// relaunch runs a relaunch as a message prints it: each drive command of
// "drive --batch b1 close … && drive --batch b1 open …" in turn, as a driver would.
func relaunch(t *testing.T, r *rig, next string) *drive.Opened {
	t.Helper()
	i := strings.Index(next, "drive --batch b1 close ")
	if i < 0 {
		t.Fatalf("no relaunch in %q", next)
	}
	var opened *drive.Opened
	for cmd := range strings.SplitSeq(next[i:], " && ") {
		w := shellWords(t, cmd)
		if w[1] == "--batch" {
			w = append(w[:1], w[3:]...)
		}
		switch w[1] {
		case "close":
			if err := r.d.Close(w[2]); err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
		case "open":
			var opt drive.OpenOptions
			var step drive.Step
			for j := 3; j < len(w); j++ {
				switch w[j] {
				case "--dir":
					j++
					opt.Dir = w[j]
				case "--resume":
					j++
					opt.Resume = w[j]
				case "--account":
					j++
					opt.Account = w[j]
				default:
					step.Line = w[j]
				}
			}
			o, err := r.d.Open(w[2], step, opt)
			if err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			opened = o
		default:
			t.Fatalf("not a relaunch: %s", cmd)
		}
	}
	return opened
}

// The relaunch drive prints for a rejected login runs as printed: the close
// on the session's view as it stands, the open on another account with the
// step. The resumed session reads the old error in the transcript it shares
// as the process it replaced's.
func TestARelaunchedSessionTakesItsStep(t *testing.T) {
	r := newRig(t, "none")
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude-accounts", "other@example.com", "sessions"), 0o755)
	_ = os.MkdirAll(filepath.Join(r.d.Cfg.Home, ".claude", "sessions"), 0o755)
	r.w.Primary, r.d.Cfg.PrimaryName = "owner", "owner"
	c := adopted(t, r, "T4")
	c.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, _ string) {
			c.Fail("authentication_failed", "Login expired · Please run /login")
			c.Set("blocked", "auth", "Login expired · Please run /login")
		}
	})
	_, err := r.d.Send(r.view("T4"), drive.Step{Line: "implement-spec"}, drive.SendOptions{})
	var f *drive.Failed
	if !errors.As(err, &f) || !f.Login {
		t.Fatalf("the step's answer is a rejected login: %v", err)
	}
	r.w.Clock.Sleep(time.Minute) // the driver reads the failure and relaunches
	o := relaunch(t, r, f.Next)
	if o.Account != "other@example.com" || o.Sent == nil || o.Sent.Outcome != "arrived" || o.Sent.Typed != "implement-spec" || o.Sent.Notice {
		t.Fatalf("resumed on another account, it takes its step with no second notice: %+v %+v", o, o.Sent)
	}

	// Rejected there too, the next relaunch is offered on the primary.
	n := r.w.Session(o.Block)
	n.Prompt("go on")
	n.Fail("authentication_failed", "Login expired · Please run /login")
	n.Set("blocked", "auth", "Login expired")
	_, err = r.d.Send(r.view("T4"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), "--account primary 'go on'") {
		t.Fatalf("the refusal prints the relaunch on the primary, the step on it: %v", err)
	}
	r.w.Clock.Sleep(time.Minute)
	if err := r.d.Close(r.view("T4")); err != nil {
		t.Fatal(err)
	}
	// Resumed without its step, it still shows the old error until it takes one.
	p, err := r.d.Open("T4", drive.Step{}, drive.OpenOptions{Dir: filepath.Join(r.w.Root, "work"), Resume: c.SID, Account: "primary"})
	if err != nil || p.Account != "primary" {
		t.Fatalf("opened on the primary: %+v %v", p, err)
	}
	if rd, _ := r.d.Read("T4", drive.ReadLast, 1); rd.State != "done" {
		t.Fatalf("the error belongs to the process it replaced: %+v", rd)
	}
	sent, err := r.d.Send(r.view("T4"), drive.Step{Line: "go on"}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" || sent.Notice {
		t.Fatalf("the resumed session takes the step, with no second notice: %+v %v", sent, err)
	}

	// With every account seen rejected, none is offered.
	m := r.w.Session(p.Block)
	m.Prompt("go on")
	m.Fail("authentication_failed", "Login expired · Please run /login")
	m.Set("blocked", "auth", "Login expired")
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
			c.Edit(func(c *testkit.Claude) {
				c.Shells = append(c.Shells, session.Shell{Started: r.w.Clock.Now(), Command: "gh pr checks --watch"})
			})
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
