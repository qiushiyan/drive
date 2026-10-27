package drive_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/testkit"
)

// pageProgress is what claude-steps and gh answer for the page: each
// session's steps by its id, each pull request's state by its number.
// While hold is set, a read of steps waits on it, as a lookup that hangs
// does, saying on waiting that it does.
type pageProgress struct {
	steps   map[string]progress.Steps
	prs     map[int]progress.PR
	prReads atomic.Int32
	reads   atomic.Int32 // reads of steps

	mu      sync.Mutex
	hold    chan struct{}
	waiting chan struct{}
}

func (p *pageProgress) Steps(id string) (progress.Steps, error) {
	p.reads.Add(1)
	p.mu.Lock()
	hold, waiting := p.hold, p.waiting
	p.mu.Unlock()
	if hold != nil {
		select {
		case waiting <- struct{}{}:
		default:
		}
		<-hold
	}
	if s, ok := p.steps[id]; ok {
		return s, nil
	}
	return progress.Steps{}, errors.New("claude-steps: no such session")
}

func (p *pageProgress) PR(ref progress.PRRef) (progress.PR, error) {
	p.prReads.Add(1)
	if pr, ok := p.prs[ref.Number]; ok {
		return pr, nil
	}
	return progress.PR{}, errors.New("gh: no such pull request")
}

func (p *pageProgress) Landed(string, []string, []progress.Commit) (progress.Landing, error) {
	return progress.Landing{}, errors.New("gwt: not here")
}

// holdSteps makes reads of steps wait until the returned release.
func (p *pageProgress) holdSteps() (release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold, p.waiting = make(chan struct{}), make(chan struct{}, 1)
	hold := p.hold
	return func() {
		p.mu.Lock()
		p.hold = nil
		p.mu.Unlock()
		close(hold)
	}
}

// pageBatch is a batch with a session in each place one can stand: W
// working, E whose turn ended with a wake nobody acted on, P parked, Q on a
// permission prompt, and C closed. P and C both name #1, which merged; C
// opened #2, whose checks fail. P has an ask, and the batch has one of its
// own.
func pageBatch(t *testing.T) (*rig, *pageProgress) {
	t.Helper()
	r := newRig(t, "none")
	prog := &pageProgress{steps: map[string]progress.Steps{}, prs: map[int]progress.PR{
		1: {Number: 1, State: "MERGED"},
		2: {Number: 2, State: "OPEN", Failed: 1, Passed: 3},
	}}
	r.d.Progress = prog
	join := func(label string) *testkit.Claude {
		c := r.w.Add(label)
		done(c, "x", "y")
		if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "onboard"}, drive.SendOptions{Label: label}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	w, p, q, c := join("W"), join("P"), join("Q"), join("C")
	join("E")
	ref := func(n int, opened bool) progress.PRRef {
		return progress.PRRef{Repo: "acme/app", Number: n, URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", n), Opened: opened}
	}
	one, two := ref(1, true), ref(2, true)
	prog.steps[p.SID] = progress.Steps{Stage: "review", StageAt: r.w.Clock.Now(), PR: &one, PRs: []progress.PRRef{one}}
	prog.steps[c.SID] = progress.Steps{Stage: "docs", StageAt: r.w.Clock.Now(), PR: &two, PRs: []progress.PRRef{ref(1, false), two}}

	w.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} }) // its build runs on
	if _, err := r.d.Send(r.view("W"), drive.Step{Line: "build it"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(time.Minute)
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWatch, Session: "E"})
	_ = r.b.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindWake, Session: "E", HTTP: 200, View: r.view("E")})
	if err := r.d.Park(r.view("P"), "waits on the S3 baseline"); err != nil {
		t.Fatal(err)
	}
	q.Ask("pnpm install")
	if err := r.d.Close(r.view("C")); err != nil {
		t.Fatal(err)
	}
	for label, goal := range map[string]string{"W": "Build the importer.", "P": "Keep instruction pages whole.", "C": "Retire the old run-plan fields."} {
		if err := r.d.Goal(label, goal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.d.Ask("P", "Merge before the baseline?", "wait for it", "P stays parked"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Ask("", "Release tonight?", "", "nothing releases"); err != nil {
		t.Fatal(err)
	}
	return r, prog
}

func labels(ov *drive.Overview) map[string][]string {
	out := map[string][]string{}
	for _, g := range ov.Groups {
		for _, c := range g.Cards {
			out[g.Title] = append(out[g.Title], c.Member.Label)
		}
	}
	return out
}

func card(ov *drive.Overview, label string) *drive.Card {
	for _, g := range ov.Groups {
		for i := range g.Cards {
			if g.Cards[i].Member.Label == label {
				return &g.Cards[i]
			}
		}
	}
	return nil
}

// The owner reads each session where it stands, by what was read rather
// than by any claim that its work is done: a closed session is closed, a
// park says when and why beside what has happened since, and every pull
// request a session opened or named shows, each read once.
func TestTheOverviewGroupsSessionsByWhereTheyStand(t *testing.T) {
	r, prog := pageBatch(t)
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	got := labels(ov)
	want := map[string][]string{drive.GroupWorking: {"W"}, drive.GroupEnded: {"E"}, drive.GroupParked: {"P"}, drive.GroupPrompt: {"Q"}, drive.GroupClosed: {"C"}}
	for title, ls := range want {
		if strings.Join(got[title], ",") != strings.Join(ls, ",") {
			t.Fatalf("%s holds %v, got %v (all: %v)", title, ls, got[title], got)
		}
	}
	if len(ov.Asks) != 2 || card(ov, "P").Asks[0] != "A1" {
		t.Fatalf("both open asks show, and P's names it: %+v %+v", ov.Asks, card(ov, "P").Asks)
	}
	c := card(ov, "C")
	if len(c.PRs) != 2 || c.PRs[0].Opened || c.PRs[0].PR == nil || c.PRs[0].PR.State != "MERGED" || c.PRs[1].PR == nil || c.PRs[1].PR.Failed != 1 {
		t.Fatalf("a closed session's pull requests, the one it named and the one it opened, as read: %+v", c.PRs)
	}
	if n := prog.prReads.Load(); n != 2 {
		t.Fatalf("each pull request is read once, #1 though two sessions name it: %d reads", n)
	}

	page := string(ov.HTML(r.w.Clock.Now()))
	for _, want := range []string{
		"Build the importer.", "No goal recorded.", "Parked by the driver at", "waits on the S3 baseline",
		"Waits on you: A1", "A2 · the whole batch", "Pull requests it opened:", "Pull requests it named:",
		"1 check failing", "The driver was woken at", "Waiting on a permission prompt", "Closed at",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if strings.Contains(page, "Done") {
		t.Error("the page claims no session's work is done")
	}
	if strings.Contains(page, "No wake is armed") {
		t.Error("a batch whose driver waits itself has no wakes to arm")
	}
}

// With Rex down, the page still shows what the record holds: goals, asks,
// parks and the sessions closed, and says the sessions could not be read.
func TestTheOverviewKeepsTheRecordWhenRexIsDown(t *testing.T) {
	r, _ := pageBatch(t)
	r.w.BlocksFail = 1
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ov.Live, "could not be read") {
		t.Fatalf("the page says what could not be read: %q", ov.Live)
	}
	got := labels(ov)
	if strings.Join(got[drive.GroupUnread], ",") != "W,Q,E" || strings.Join(got[drive.GroupParked], ",") != "P" || strings.Join(got[drive.GroupClosed], ",") != "C" {
		t.Fatalf("the record's places stand without Rex: %v", got)
	}
	if card(ov, "W").Member.Goal != "Build the importer." || len(ov.Asks) != 2 || len(card(ov, "C").PRs) != 2 {
		t.Fatal("goals, asks and a closed session's pull requests still show")
	}
	if page := string(ov.HTML(r.w.Clock.Now())); !strings.Contains(page, "Not read live: the sessions could not be read") {
		t.Fatal("the page leads with what it could not read")
	}
}

func get(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// The page reads on request, reuses a read for its window rather than
// costing a lookup per session per reload, and answers from the record
// while a read runs past its deadline. It only reads.
func TestThePageReadsOnRequest(t *testing.T) {
	r, prog := pageBatch(t)
	p := drive.NewPage(r.d)
	srv := httptest.NewServer(p)
	defer srv.Close()

	code, h, body := get(t, srv.URL)
	if code != 200 || h.Get("Cache-Control") != "no-store" || !strings.Contains(body, "Batch b1") || !strings.Contains(body, "Build the importer.") {
		t.Fatalf("the page: %d %v\n%s", code, h, body)
	}
	reads := prog.reads.Load()
	get(t, srv.URL)
	if prog.reads.Load() != reads {
		t.Fatal("a reload within the window reuses the read")
	}
	r.w.Clock.Sleep(31 * time.Second)
	get(t, srv.URL)
	if prog.reads.Load() == reads {
		t.Fatal("a reload past the window reads again")
	}

	p.Deadline = 50 * time.Millisecond
	release := prog.holdSteps()
	r.w.Clock.Sleep(31 * time.Second)
	_, _, body = get(t, srv.URL)
	if !strings.Contains(body, "still being read") || !strings.Contains(body, "Build the importer.") || !strings.Contains(body, "waits on the S3 baseline") {
		t.Fatalf("a read past its deadline is answered from the record:\n%s", body)
	}
	release()
	p.Deadline = 5 * time.Second
	if _, _, body = get(t, srv.URL); strings.Contains(body, "Not read live") {
		t.Fatalf("once the read ends, the page is live again:\n%s", body)
	}

	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("the page changes nothing: %d", resp.StatusCode)
	}
	if code, _, _ := get(t, srv.URL+"/x"); code != http.StatusNotFound {
		t.Fatalf("the page is one page: %d", code)
	}
}

// A read is of one batch, the one current when it began: a batch started
// while it runs shows on the next read, never spliced into this one.
func TestAPageIsOneBatch(t *testing.T) {
	r, prog := pageBatch(t)
	srv := httptest.NewServer(drive.NewPage(r.d))
	defer srv.Close()
	release := prog.holdSteps()
	got := make(chan string)
	go func() {
		_, _, body := get(t, srv.URL)
		got <- body
	}()
	<-prog.waiting
	if _, err := r.d.Store.New(batch.Meta{Name: "b2", Started: r.w.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	release()
	if body := <-got; !strings.Contains(body, "Batch b1") || !strings.Contains(body, "Build the importer.") || strings.Contains(body, "b2") {
		t.Fatalf("the read under way is wholly b1's:\n%s", body)
	}
	r.w.Clock.Sleep(31 * time.Second)
	if _, _, body := get(t, srv.URL); !strings.Contains(body, "Batch b2") || !strings.Contains(body, "No session has joined the batch yet.") {
		t.Fatalf("the next read is b2's:\n%s", body)
	}
}
