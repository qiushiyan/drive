package drive_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
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
// working, E whose turn ended with a wake nobody acted on and whose steps
// cannot be read, L whose login was rejected, P parked, Q on a permission
// prompt, and C and then D closed. W and P, both open, and C name #1, which
// merged; C opened #2, whose checks fail. P has an ask, and the batch has
// one of its own.
func pageBatch(t *testing.T) (*rig, *pageProgress) {
	t.Helper()
	r := newRig(t, "none")
	prog := &pageProgress{steps: map[string]progress.Steps{}, prs: map[int]progress.PR{
		1: {Number: 1, State: "MERGED", MergedAt: r.w.Clock.Now().Add(time.Minute)},
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
	l, dd := join("L"), join("D")
	ref := func(n int, opened bool) progress.PRRef {
		return progress.PRRef{Repo: "acme/app", Number: n, URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", n), Opened: opened}
	}
	one, two := ref(1, true), ref(2, true)
	now := r.w.Clock.Now()
	prog.steps[w.SID] = progress.Steps{Stage: "build", StageAt: now, PR: &one, PRs: []progress.PRRef{one}}
	prog.steps[p.SID] = progress.Steps{Stage: "review", StageAt: now, PR: &one, PRs: []progress.PRRef{one}}
	prog.steps[q.SID] = progress.Steps{Stage: "build", StageAt: now}
	prog.steps[l.SID] = progress.Steps{Stage: "onboard", StageAt: now}
	prog.steps[c.SID] = progress.Steps{Stage: "docs", StageAt: now, PR: &two, PRs: []progress.PRRef{ref(1, false), two}}
	prog.steps[dd.SID] = progress.Steps{Stage: "docs", StageAt: now}

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
	l.Fail("authentication_failed", "Login expired · Please run /login")
	l.Set("blocked", "auth", "Login expired · Please run /login")
	if err := r.d.Close(r.view("C")); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(time.Minute)
	if err := r.d.Close(r.view("D")); err != nil {
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

// clockOf is when the batch recorded the newest event of kind for session,
// as the page prints a time from today.
func clockOf(t *testing.T, r *rig, kind, session string) string {
	t.Helper()
	at := time.Time{}
	for _, e := range r.events(kind) {
		if e.Session == session {
			at = e.At
		}
	}
	if at.IsZero() {
		t.Fatalf("no %s for %s", kind, session)
	}
	return "at " + at.In(r.b.Zone()).Format("15:04")
}

// The owner reads each session where it stands, by what was read rather
// than by any claim that its work is done: a closed session is closed, a
// park says when and why beside what has happened since, a question waiting
// on the owner says what it asks and what happens unanswered, and every
// pull request a session opened or named shows, each read once however
// many sessions, open or closed, name it.
func TestTheOverviewGroupsSessionsByWhereTheyStand(t *testing.T) {
	r, prog := pageBatch(t)
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	got := labels(ov)
	want := map[string][]string{drive.GroupWorking: {"W"}, drive.GroupNext: {"E", "L"}, drive.GroupParked: {"P"},
		drive.GroupPrompt: {"Q"}, drive.GroupClosed: {"D", "C"}}
	if len(got) != len(want) {
		t.Fatalf("groups: %v", got)
	}
	for title, ls := range want {
		if strings.Join(got[title], ",") != strings.Join(ls, ",") {
			t.Fatalf("%s holds %v in that order, got %v (all: %v)", title, ls, got[title], got)
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
		t.Fatalf("each pull request is read once, #1 though three sessions name it: %d reads", n)
	}

	page := string(ov.HTML(r.w.Clock.Now()))
	for _, want := range []string{
		"Build the importer.", "No goal recorded.",
		`<span class="why">waits on the S3 baseline</span>`, "Parked by the driver " + clockOf(t, r, batch.KindPark, "P") + ".",
		`Closed</span><span class="for">` + clockOf(t, r, batch.KindClose, "C"), `Closed</span><span class="for">` + clockOf(t, r, batch.KindClose, "D"),
		// A question links to its session's card, and the card back to the question.
		`<article class="ask" id="b1/ask-A1" data-batch="b1">`, `<span class="ask-id">A1</span><a href="#b1/P">P</a>`, `<details class="strip st-park" id="b1/P">`,
		`<a class="chip ask" href="#b1/ask-A1">Waits on you: A1</a>`, `<span class="ask-id">A2</span><span>the whole batch</span>`,
		"Merge before the baseline?", "<dt>Recommended</dt><dd>wait for it</dd>", "<dt>If nobody answers</dt><dd>P stays parked</dd>", "<dd>nothing releases</dd>",
		"Pull requests it opened:", "Pull requests it named:", "1 check failing",
		// A pull request names its repository, since a batch's sessions may work in several.
		`<a class="chip pr pr-failing" href="https://github.com/acme/app/pull/2">app#2 <span class="pr-st">open, 1 check failing</span></a>`,
		"The driver was woken at", "Waiting on a permission prompt", "Its login was rejected",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if n := strings.Count(page, "Not read: its stage and pull requests"); n != 1 {
		t.Errorf("only E's steps could not be read, and the page says so once: %d", n)
	}
	if strings.Contains(page, "Done") {
		t.Error("the page claims no session's work is done")
	}
	if strings.Contains(page, "wake is armed") {
		t.Error("a batch whose driver waits itself has no wakes to speak of")
	}
}

// In a batch that wakes its driver, the page says whether the driver will
// hear of a session: a watch that ended leaves none armed, and one still
// running long after its turn ended is armed but stuck, not absent.
func TestTheWakeLineSaysWhatTheWakeIs(t *testing.T) {
	r := newRig(t, "webhook")
	r.d.Cfg.Webhook = config.Webhook{URL: "http://127.0.0.1:1/none"}
	for _, label := range []string{"K", "N"} {
		c := r.w.Add(label)
		done(c, "onboard", "analysis")
		if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "review"}, drive.SendOptions{Label: label}); err != nil {
			t.Fatal(err)
		}
		if label == "K" {
			c.Ask("pnpm install")
		}
	}
	_ = r.watches.Stop("drive-b1-" + keyOfBlock(r, "N")) // N's watch ended without a wake
	r.w.Clock.Sleep(time.Hour)                           // K's watch never posted
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	page := string(ov.HTML(r.w.Clock.Now()))
	if !strings.Contains(page, "Its wake is armed but has not come") || !strings.Contains(page, "No wake is armed for it") {
		t.Fatalf("K's stuck wake is armed, N has none:\n%s", page)
	}
	if strings.Count(page, "No wake is armed for it") != 1 {
		t.Fatal("only N has no wake armed")
	}
}

// keyOfBlock is the part of a session's watch name its block gives.
func keyOfBlock(r *rig, label string) string {
	evs, _ := r.b.Events()
	m := batch.Find(batch.Members(evs), label)
	return m.Block[len(m.Block)-6:]
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
	if strings.Join(got[drive.GroupUnread], ",") != "W,Q,E,L" || strings.Join(got[drive.GroupParked], ",") != "P" || strings.Join(got[drive.GroupClosed], ",") != "D,C" {
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
		t.Error(err)
		return 0, nil, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// The page reads on request and reuses a read for its window rather than
// costing a lookup per session per reload. It only reads.
func TestThePageReadsOnRequest(t *testing.T) {
	r, prog := pageBatch(t)
	srv := httptest.NewServer(drive.NewPage(r.d))
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

// A read slower than the window is still fresh when it ends: the window
// counts from when it ended, so the next visit shows it instead of reading
// again.
func TestASlowReadIsReusedOnceItEnds(t *testing.T) {
	r, prog := pageBatch(t)
	p := drive.NewPage(r.d)
	p.Deadline = 5 * time.Second
	srv := httptest.NewServer(p)
	defer srv.Close()
	release := prog.holdSteps()
	got := make(chan string)
	go func() {
		_, _, body := get(t, srv.URL)
		got <- body
	}()
	<-prog.waiting
	r.w.Clock.Sleep(40 * time.Second) // the read outlasts the window
	release()
	if body := <-got; strings.Contains(body, "Not read live") {
		t.Fatalf("the read that ended answers its request:\n%s", body)
	}
	reads := prog.reads.Load()
	if _, _, body := get(t, srv.URL); strings.Contains(body, "Not read live") || prog.reads.Load() != reads {
		t.Fatalf("the next visit reuses it: %d reads, then %d", reads, prog.reads.Load())
	}
}

// A read that outlasts its deadline is answered from the record, every
// visit while it runs and at once once it is overdue, with no second read
// started beside it; one that runs past the point of giving up on it is
// left to finish, and the next visit starts another.
func TestAnOverdueReadIsAnsweredFromTheRecord(t *testing.T) {
	r, prog := pageBatch(t)
	p := drive.NewPage(r.d)
	p.Deadline = 400 * time.Millisecond
	srv := httptest.NewServer(p)
	defer srv.Close()
	release := prog.holdSteps()
	defer release()

	_, _, body := get(t, srv.URL)
	if !strings.Contains(body, "still being read") || !strings.Contains(body, "Build the importer.") || !strings.Contains(body, "waits on the S3 baseline") {
		t.Fatalf("a read past its deadline is answered from the record:\n%s", body)
	}
	reads := prog.reads.Load()
	start := time.Now()
	_, _, body = get(t, srv.URL)
	if took := time.Since(start); took > 200*time.Millisecond || !strings.Contains(body, "still being read") {
		t.Fatalf("a visit to an overdue read is answered at once, not after a deadline of its own: %s", took)
	}
	if prog.reads.Load() != reads {
		t.Fatal("one read at a time: the second visit started none")
	}
	p.GiveUpAfter = 0
	get(t, srv.URL)
	if prog.reads.Load() == reads {
		t.Fatal("a read held past the point of giving up on it is replaced by a new one")
	}
}

// Each batch has its own read: a batch started while the last one's read
// is still fresh shows at once, not the old batch's page.
func TestABatchStartedMeanwhileShowsAtOnce(t *testing.T) {
	r, _ := pageBatch(t)
	srv := httptest.NewServer(drive.NewPage(r.d))
	defer srv.Close()
	if _, _, body := get(t, srv.URL); !strings.Contains(body, "Batch b1") {
		t.Fatalf("b1's page:\n%s", body)
	}
	if _, _, err := r.d.Store.New(batch.Meta{Name: "b2", Started: r.w.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, _, body := get(t, srv.URL); !strings.Contains(body, "Batch b2") || !strings.Contains(body, "No session has joined the batch yet.") {
		t.Fatalf("b2's page at once, though b1's read is fresh:\n%s", body)
	}
}

// drive status shows each open session's newest pull request as read, and
// drive metrics counts a session whose pull request could not be read as
// unread rather than unmerged: both now read through the page's one reader.
func TestStatusAndMetricsReadPullRequestsThroughOneReader(t *testing.T) {
	r, prog := pageBatch(t)
	st, err := r.d.Status(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range st.Rows {
		if row.Label == "W" && (row.PR == nil || row.PR.State != "MERGED") {
			t.Fatalf("W's newest pull request, #1, as read: %+v %q", row.PR, row.PRErr)
		}
	}
	if n := prog.prReads.Load(); n != 1 {
		t.Fatalf("#1, the newest of two open sessions, is read once: %d", n)
	}
	unreadable := progress.PRRef{Repo: "acme/app", Number: 3}
	d := prog.steps[sidOf(r, "D")]
	d.PRs = []progress.PRRef{unreadable}
	prog.steps[sidOf(r, "D")] = d
	m, err := r.d.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.Unread != 2 || len(m.Merged) != 1 || strings.Join(m.Merged[0].Sessions, ",") != "W,P,C" {
		t.Fatalf("E's steps and D's pull request could not be read; #1 merged, named by W, P and C: unread %d, %+v", m.Unread, m.Merged)
	}
}

func sidOf(r *rig, label string) string {
	evs, _ := r.b.Events()
	return batch.Find(batch.Members(evs), label).SID
}

// The owner reads every driver's batch on one page: each under its own
// heading, every batch's questions first, each anchored by its batch, since
// every batch numbers its asks from A1 and its labels are its own. A batch
// whose record cannot be read says so beside the others, and --driver
// shows one driver's alone.
func TestThePageShowsEveryDriversBatch(t *testing.T) {
	r, _ := pageBatch(t)
	it := newDriverBatch(t, r, "itell-2026-10-28", "itell")
	adopted(t, r, "P")
	if _, err := r.d.Ask("P", "Take the payment milestone first?", "yes", "it waits"); err != nil {
		t.Fatal(err)
	}
	r.d.DriverName = ""
	srv := httptest.NewServer(drive.NewPage(r.d))
	defer srv.Close()
	_, _, body := get(t, srv.URL)
	for _, want := range []string{
		// Each batch is a tab, named for its project, beside All.
		`<a class="tab" href="#all" data-tab="all">`, `<a class="tab" href="#b1" data-tab="b1"><span class="tab-name">b1</span>`,
		`<a class="tab" href="#itell-2026-10-28" data-tab="itell-2026-10-28"><span class="tab-name">itell</span>`,
		`<section class="batch" id="itell-2026-10-28" data-batch="itell-2026-10-28" data-view>`, "Batch itell-2026-10-28 · started", "Driven by itell, which waits itself",
		`id="b1/ask-A1"`, `id="itell-2026-10-28/ask-A1"`,
		`<span class="ask-id">A1</span><span class="bt">itell</span><a href="#itell-2026-10-28/P">P</a>`,
		`id="b1/P"`, `id="itell-2026-10-28/P"`, "Take the payment milestone first?", "Build the importer.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if strings.Index(body, "Take the payment milestone first?") > strings.Index(body, `<section class="batch"`) {
		t.Error("every batch's questions come before any batch's sessions")
	}

	r.w.Clock.Sleep(31 * time.Second)
	log := filepath.Join(it.Dir, "events.jsonl")
	if err := os.Chmod(log, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(log, 0o644)
	if code, _, body := get(t, srv.URL); code != 200 || !strings.Contains(body, "Not read live: its record could not be read") ||
		!strings.Contains(body, `<span class="tab-name">itell</span><span class="warn" title="not read live">!</span>`) ||
		!strings.Contains(body, "Build the importer.") {
		t.Fatalf("an unreadable batch keeps none of the others off the page: %d\n%s", code, body)
	}
	os.Chmod(log, 0o644)

	r.d.DriverName = "itell"
	one := httptest.NewServer(drive.NewPage(r.d))
	defer one.Close()
	if _, _, body := get(t, one.URL); !strings.Contains(body, "Batch itell-2026-10-28 · started") || strings.Contains(body, "Build the importer.") || strings.Contains(body, `data-tab=`) {
		t.Fatalf("--driver shows that driver's batch alone:\n%s", body)
	}
}

// Each batch is named for its project: the Rex session its sessions open
// in, else its driver, else its own name. Two batches that would read alike
// are told apart by their drivers, and a Rex session an older drive
// recorded by its id names nothing.
func TestTheTabsNameEachBatchByItsProject(t *testing.T) {
	now := time.Date(2026, 10, 29, 13, 17, 0, 0, time.UTC)
	ov := func(m batch.Meta) *drive.Overview {
		m.Started = now.Add(-time.Hour)
		return &drive.Overview{Batch: &m, Zone: time.UTC, At: now, Wakes: m.Wake == "webhook"}
	}
	page := string(drive.PageHTML([]*drive.Overview{
		ov(batch.Meta{Name: "2026-10-28", Driver: "session-driver", RexSession: "planlab", Wake: "webhook"}),
		ov(batch.Meta{Name: "planlab-2", Driver: "nightly", RexSession: "planlab"}),
		ov(batch.Meta{Name: "verify", Driver: "attest", RexSession: "session:01a17cd1"}),
		ov(batch.Meta{Name: "dogfood-verify", RexSession: "session:01a17cd1"}),
	}, now))
	for _, want := range []string{
		`data-tab="2026-10-28"><span class="tab-name">planlab · session-driver</span>`,
		`data-tab="planlab-2"><span class="tab-name">planlab · nightly</span>`,
		`data-tab="verify"><span class="tab-name">attest</span>`,
		`data-tab="dogfood-verify"><span class="tab-name">dogfood-verify</span>`,
		"Rex session <b>planlab</b>", "Driven by session-driver, woken by webhook",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if strings.Contains(page, "<b>session:") {
		t.Error("a Rex session recorded by its id is never shown as a name")
	}
}

// What the record holds sits beside what was read: a park keeps the words
// it was written in, with what came after it; an answered question keeps
// its link, now on the decision, and says who settled it; a session's
// history holds its asks, their answers and the driver's notes verbatim,
// and each stage the configuration names shows when the session last
// stepped under it.
func TestTheRecordShowsWhatHappenedSinceThePark(t *testing.T) {
	r, prog := pageBatch(t)
	r.w.Clock.Sleep(time.Minute)
	if _, err := r.d.Answer("A1", "the baseline landed meanwhile", "moot", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Answer("A2", `"yes, tonight" (14:02)`, "recommended", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Note("P", "N1: baseline landed; P waits for its next step"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Note("", "plan: W builds, P reviews"); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.b.Events()
	w := batch.Find(batch.Members(evs), "W")
	now := r.w.Clock.Now()
	prog.steps[w.SID] = progress.Steps{Stage: "build", StageAt: now, Labels: []progress.Label{
		{Name: "onboard", At: now.Add(-time.Hour)}, {Name: "spec"}, {Name: "build", At: now},
	}}
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	page := string(ov.HTML(r.w.Clock.Now()))
	answered := clockOf(t, r, batch.KindAnswer, "")
	for _, want := range []string{
		`<span class="why">waits on the S3 baseline</span>`,
		`<span class="since">Since the park: A1 was recorded moot ` + answered + `.</span>`,
		`<div class="row st-done" id="b1/ask-A1">`, `Recorded moot</span>the baseline landed meanwhile`,
		`<div class="row st-work" id="b1/ask-A2">`, `As recommended</span>&#34;yes, tonight&#34; (14:02)`,
		`<span class="kind">A1 was recorded moot</span> the baseline landed meanwhile`,
		`<span class="kind">Note N1</span><span class="note">N1: baseline landed; P waits for its next step</span>`,
		"Notes for the whole batch", "plan: W builds, P reviews",
		"<dt>Your decisions</dt><dd>1 answered<small>1 as recommended, 1 recorded moot</small>",
		`<p class="empty" data-none>Nothing waits on you.</p>`,
		`<li class="on"><b></b><span>onboard</span><em>1h00m</em></li>`, `<li class=""><b></b><span>spec</span><em>·</em></li>`,
		`<li class="on newest"><b></b><span>build</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if n := strings.Count(page, `id="b1/ask-A1"`); n != 1 {
		t.Errorf("an answered question is on the page once, under its own link: %d", n)
	}
	if strings.Contains(page, "you took the recommendation on A1") || strings.Contains(page, "you amended A1") {
		t.Error("a question that stopped mattering is no answer of the owner's")
	}
	if !strings.Contains(page[strings.Index(page, `id="b1/C"`):], "What the record holds") {
		t.Error("a closed session keeps its history")
	}
}

// A session's history shows its newest entries and folds the rest away,
// still on the page.
func TestASessionsHistoryFoldsPastTheNewest(t *testing.T) {
	r, _ := pageBatch(t)
	for i := 1; i <= 14; i++ {
		if _, err := r.d.Note("W", fmt.Sprintf("step %02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	page := string(ov.HTML(r.w.Clock.Now()))
	strip := page[strings.Index(page, `id="b1/W"`):]
	strip = strip[:strings.Index(strip, "</details>\n</div>")]
	fold := strings.Index(strip, `<details class="earlier"><summary>6 earlier</summary>`)
	if fold < 0 {
		t.Fatalf("W's 18 entries show 12 and fold 6:\n%s", strip)
	}
	if newest, oldest := strings.Index(strip, "step 14"), strings.Index(strip, "step 01"); newest < 0 || newest > fold || oldest < fold {
		t.Fatalf("the newest show and the oldest fold: %d %d %d", newest, fold, oldest)
	}
}

// What was not read says so rather than reading as none: a batch answered
// from its record alone has no count of wakes or pull requests to give.
func TestWhatWasNotReadIsNotNone(t *testing.T) {
	r := newRig(t, "webhook")
	r.d.Cfg.Webhook = config.Webhook{URL: "http://127.0.0.1:1/none"}
	c := r.w.Add("K")
	done(c, "onboard", "analysis")
	if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "review"}, drive.SendOptions{Label: "K"}); err != nil {
		t.Fatal(err)
	}
	ov, err := r.d.Overview(false)
	if err != nil {
		t.Fatal(err)
	}
	page := string(ov.HTML(r.w.Clock.Now()))
	for _, want := range []string{"<dt>Wakes</dt><dd>Not read", "<dt>Pull requests</dt><dd>Not read", "Not read live: only the batch&#39;s record was read"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page says %q", want)
		}
	}
	if ov, err = r.d.Overview(true); err != nil {
		t.Fatal(err)
	}
	if page := string(ov.HTML(r.w.Clock.Now())); !strings.Contains(page, "<dt>Wakes</dt><dd>1 of 1 armed") || !strings.Contains(page, "<dt>Pull requests</dt><dd>None yet") {
		t.Errorf("read live, it counts them:\n%s", page)
	}
}

// The driver's last action is a step, an open, a park or a close; a watch's
// own events, such as one that ended without a wake, never count, so a
// driver that has gone reads as gone.
func TestTheLastDriverActionIsNeverAWatchs(t *testing.T) {
	r, _ := pageBatch(t)
	closed := clockOf(t, r, batch.KindClose, "D")
	r.w.Clock.Sleep(5 * time.Minute)
	r.d.WatchFailed("E", errors.New("no webhook configured"))
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	if e := ov.Acted; e == nil || e.Kind != batch.KindClose || e.Session != "D" {
		t.Fatalf("the newest thing a driver did is D's close: %+v", e)
	}
	if page := string(ov.HTML(r.w.Clock.Now())); !strings.Contains(page, "<dt>Last driver action</dt><dd>"+strings.TrimPrefix(closed, "at ")+"<small>5m ago, D closed</small>") {
		t.Errorf("the page says when and what:\n%s", page[strings.Index(page, `<dl class="health">`):][:300])
	}
}

// All lists every session waiting on its driver, from every batch: one on
// a prompt as well as one whose turn ended, each linking to its strip.
func TestAllListsEverySessionWaitingOnItsDriver(t *testing.T) {
	r, _ := pageBatch(t)
	newDriverBatch(t, r, "itell-2026-10-28", "itell")
	adopted(t, r, "P")
	r.d.DriverName = ""
	srv := httptest.NewServer(drive.NewPage(r.d))
	defer srv.Close()
	_, _, body := get(t, srv.URL)
	all := body[strings.Index(body, `<section id="all" data-view>`):strings.Index(body, `<section class="batch"`)]
	for _, want := range []string{`<a class="go st-next" href="#b1/Q">`, `<a class="go st-next" href="#b1/E">`, `<a class="go st-alert" href="#b1/L">`} {
		if !strings.Contains(all, want) {
			t.Errorf("All lists %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "#b1/W") || strings.Contains(all, " id=\"b1/") {
		t.Error("All lists only sessions waiting on a driver, and links to them rather than repeating them")
	}
}
