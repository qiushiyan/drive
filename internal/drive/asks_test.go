package drive_test

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/progress"
)

// A question that replaces another settles it in the same write. On
// 2026-11-11 A41's own words said it replaced A4, and the owner's list
// still carried A4 a day later.
func TestAnAskThatReplacesAnotherRecordsItMoot(t *testing.T) {
	r := newRig(t, "none")
	a1, err := r.d.Ask("", "Finish the reply before web search?", "yes", "it builds that", "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := r.d.Ask("", "Stop the reply at once for web search?", "yes", "it builds that", a1)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := r.b.Events()
	_, asks, _ := batch.Records(evs)
	if len(asks) != 2 || asks[0].Answer == nil || asks[0].Answer.Took != "moot" || asks[0].Answer.Source != "replaced by "+a2 || asks[1].Answer != nil {
		t.Fatalf("%s is not recorded moot, replaced by %s: %+v %+v", a1, a2, asks[0].Answer, asks[1].Answer)
	}
	if asks[1].Ref != a1 {
		t.Errorf("%s does not say it replaces %s: %+v", a2, a1, asks[1].Event)
	}

	// An ask no longer open, or never asked, is replaced by nothing: refused, and nothing recorded.
	for _, ref := range []string{a1, "A9"} {
		if _, err := r.d.Ask("", "Again?", "", "it waits", ref); !isRefused(err) {
			t.Errorf("replacing %s: got %v, want a refusal", ref, err)
		}
	}
	if after, _ := r.b.Events(); len(after) != len(evs) {
		t.Errorf("a refused replace recorded %d events", len(after)-len(evs))
	}
}

// An ask outlives its session's close and its pull request's merge while it
// still holds something, as A1, A2 and A16 did on 2026-11-11 with a
// production repair and a message, and goes stale once it holds nothing, as
// A36 had: the record alone told the owner neither. Status says what
// overtook each so its driver judges it again, a restatement stands it
// again from then, and the owner's page sets the same beside it.
func TestAnAskOvertakenByACloseOrAMergeIsJudgedAgain(t *testing.T) {
	r := newRig(t, "none")
	prog := &pageProgress{steps: map[string]progress.Steps{}, prs: map[int]progress.PR{
		7: {Number: 7, State: "OPEN"},
		8: {Number: 8, State: "OPEN"},
	}}
	r.d.Progress = prog
	ref := func(n int) *progress.PRRef {
		return &progress.PRRef{Repo: "acme/app", Number: n, URL: "https://github.com/acme/app/pull/1", Opened: true}
	}
	for label, n := range map[string]int{"S1": 7, "S2": 8} {
		c := r.w.Add(label)
		done(c, "x", "y")
		if _, err := r.d.Send(r.view(c.Block[len(c.Block)-6:]), drive.Step{Line: "onboard"}, drive.SendOptions{Label: label}); err != nil {
			t.Fatal(err)
		}
		prog.steps[c.SID] = progress.Steps{Stage: "build", StageAt: r.w.Clock.Now(), PR: ref(n), PRs: []progress.PRRef{*ref(n)}}
	}
	a1, _ := r.d.Ask("S1", "Repair the 24 pages in production?", "yes", "the merge waits", "")
	a2, _ := r.d.Ask("S2", "Merge #8?", "yes", "the merge waits", "")
	status := func() *drive.Status {
		t.Helper()
		st, err := r.d.Status(true)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status(); len(st.Overtaken) != 0 {
		t.Fatalf("nothing happened since the asks, yet: %+v", st.Overtaken)
	}

	r.w.Clock.Sleep(time.Minute)
	prog.prs[7] = progress.PR{Number: 7, State: "MERGED", MergedAt: r.w.Clock.Now()}
	st := status()
	if o, ok := st.Overtaken[a1]; !ok || o.PR == nil || o.PR.Number != 7 || !o.Closed.IsZero() {
		t.Fatalf("%s's pull request merged after it, yet status says %+v", a1, st.Overtaken)
	}
	if _, ok := st.Overtaken[a2]; ok {
		t.Errorf("%s was overtaken by nothing: %+v", a2, st.Overtaken[a2])
	}
	text := st.Text(r.w.Clock.Now())
	for _, want := range []string{"since " + a1 + " was asked: #7 merged at ", "record answer " + a1 + " --took moot", "record restate " + a1 + " --if-unanswered"} {
		if !strings.Contains(text, want) {
			t.Errorf("status does not say %q:\n%s", want, text)
		}
	}
	ov, err := r.d.Overview(true)
	if err != nil {
		t.Fatal(err)
	}
	if page := string(ov.HTML(r.w.Clock.Now())); !strings.Contains(page, `<span class="since">Since it was asked: app#7 merged at `) {
		t.Errorf("the page sets no merge beside %s:\n%s", a1, page)
	}

	// Still holding the production repair, it is restated, and stands again from then.
	r.w.Clock.Sleep(time.Minute)
	if err := r.d.Restate(a1, "The repair script is built; the production run waits for your word."); err != nil {
		t.Fatal(err)
	}
	st = status()
	if _, ok := st.Overtaken[a1]; ok {
		t.Errorf("%s was restated after its merge, yet status still flags it: %+v", a1, st.Overtaken[a1])
	}
	text = st.Text(r.w.Clock.Now())
	if !strings.Contains(text, "restated <1m") || !strings.Contains(text, "unanswered: The repair script is built") {
		t.Errorf("status does not show %s as restated:\n%s", a1, text)
	}

	// A close after the ask overtakes it as well, read from the record alone.
	r.w.Clock.Sleep(time.Minute)
	if err := r.d.Close(r.view("S2")); err != nil {
		t.Fatal(err)
	}
	st, err = r.d.Status(false)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := st.Overtaken[a2]; !ok || o.Closed.IsZero() || o.PR != nil {
		t.Errorf("S2 closed after %s, yet status says %+v", a2, st.Overtaken)
	}
	if !strings.Contains(st.Text(r.w.Clock.Now()), "since "+a2+" was asked: S2 closed at ") {
		t.Errorf("status does not name S2's close:\n%s", st.Text(r.w.Clock.Now()))
	}

	// A restatement, like an answer, is for an open ask only.
	if err := r.d.Restate("A9", "it waits"); !isRefused(err) {
		t.Errorf("restating an ask never asked: got %v, want a refusal", err)
	}
	if err := r.d.Restate(a2, " "); !isRefused(err) {
		t.Errorf("restating with no words: got %v, want a refusal", err)
	}
}
