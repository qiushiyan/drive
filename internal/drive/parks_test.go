package drive_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
)

// statusOf reads the batch and returns label's row and the lines status
// prints about parks under the table.
func statusOf(t *testing.T, r *rig, label string) (drive.Row, []string) {
	t.Helper()
	st, err := r.d.Status(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range st.Rows {
		if row.Label == label {
			return row, st.Parks
		}
	}
	t.Fatalf("no row for %s", label)
	return drive.Row{}, nil
}

// A park nothing on record ends is forgotten by a driver that keeps nothing
// between runs, so it is refused before it stops the wake, saying the three
// ways to park it and close.
func TestAParkNothingEndsIsRefused(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "S5")
	err := r.d.Park(r.view("S5"), "the build waits until the prompt PRs land", time.Time{})
	var ref *drive.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("refused: %v", err)
	}
	for _, want := range []string{"drive --batch b1 record ask S5", "--until WHEN", "drive --batch b1 close S5@"} {
		if !strings.Contains(ref.Next, want) {
			t.Fatalf("next names %q: %s", want, ref.Next)
		}
	}
	if len(r.events(batch.KindPark)) != 0 {
		t.Fatal("nothing is recorded")
	}
	if !r.watches.Alive(watchOf(c)) {
		t.Fatal("its wake still stands: a refusal did nothing")
	}
}

// An open ask about the session ends its park; so does a time, which must
// be to come.
func TestAParkEndsOnAnAskOrATime(t *testing.T) {
	r := newRig(t, "webhook")
	adopted(t, r, "S5")
	if _, err := r.d.Ask("S5", "Start the build without the two prompt PRs?", "yes", "S5 stays parked"); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Park(r.view("S5"), "waits for your call on the prompt PRs", time.Time{}); err != nil {
		t.Fatal(err)
	}
	row, parks := statusOf(t, r, "S5")
	if !strings.HasPrefix(row.Watch, "parked on A1: waits for your call") || len(parks) != 0 {
		t.Fatalf("%q %q", row.Watch, parks)
	}

	adopted(t, r, "S6")
	if err := r.d.Park(r.view("S6"), "the fleet read", r.w.Clock.Now().Add(-time.Minute)); !isRefused(err) {
		t.Fatalf("a time gone by is refused: %v", err)
	}
	until := r.w.Clock.Now().Add(3 * time.Hour).Truncate(time.Minute)
	if err := r.d.Park(r.view("S6"), "the fleet read", until); err != nil {
		t.Fatal(err)
	}
	if row, _ := statusOf(t, r, "S6"); !strings.HasPrefix(row.Watch, "parked until "+until.In(time.Local).Format("01-02 15:04")+": the fleet read") {
		t.Fatalf("%q", row.Watch)
	}
	if e := r.events(batch.KindPark); !e[len(e)-1].Due.Equal(until) {
		t.Fatalf("the time is on record: %+v", e[len(e)-1])
	}
}

// A park's time coming is told once by the watchdog, again only while no run
// heard it, and listed under status's table; parking again with a later time
// makes it a new park, told afresh when that time comes.
func TestAParkTimeComingIsToldOnce(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	adopted(t, r, "S6")
	if err := r.d.Park(r.view("S6"), "the fleet read", r.w.Clock.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	due := func() {
		t.Helper()
		if _, err := r.d.DueAll(); err != nil {
			t.Fatal(err)
		}
	}
	if due(); l.count() != 0 {
		t.Fatalf("not yet: %d posts", l.count())
	}
	r.w.Clock.Sleep(2*time.Hour + time.Minute)
	row, parks := statusOf(t, r, "S6")
	if !strings.Contains(row.Watch, "which has come") || len(parks) != 1 || !strings.HasPrefix(parks[0], "park of S6 until ") {
		t.Fatalf("%q %q", row.Watch, parks)
	}
	if due(); l.count() != 1 || !strings.Contains(l.output(0), "park of S6 until") || l.posts[0]["kind"] != "watchdog" {
		t.Fatalf("%d posts", l.count())
	}
	r.w.Clock.Sleep(5 * time.Minute)
	if due(); l.count() != 1 {
		t.Fatalf("not again before a run has had the time: %d", l.count())
	}
	r.d.Seen()
	r.w.Clock.Sleep(time.Hour)
	if due(); l.count() != 1 {
		t.Fatalf("heard, it goes no more: %d", l.count())
	}
	// Parked again for later: quiet until then, then told again.
	if err := r.d.Park(r.view("S6"), "the fleet read, after the 14:00 run", r.w.Clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, parks := statusOf(t, r, "S6"); len(parks) != 0 {
		t.Fatalf("%q", parks)
	}
	r.w.Clock.Sleep(time.Hour + time.Minute)
	if due(); l.count() != 2 {
		t.Fatalf("the new time is told: %d", l.count())
	}
}

// An ask answered with no step leaves its session parked with nothing to end
// it: the answer still records, and says what to do; status lists the park,
// and the watchdog tells of it once.
func TestAnAnswerWithNoStepLeavesAParkToSettle(t *testing.T) {
	r := newRig(t, "webhook")
	l := hook(t, r)
	adopted(t, r, "S6")
	id, err := r.d.Ask("S6", "Re-trigger the stuck Codex review?", "yes", "the review stays stuck")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.Park(r.view("S6"), "waits for your call on the review", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if next := r.d.LeftParked(id); next != "" {
		t.Fatalf("open, the ask ends the park: %q", next)
	}
	if _, err := r.d.Answer(id, "moot: Codex finished on its own", "moot", false); err != nil {
		t.Fatal(err)
	}
	next := r.d.LeftParked(id)
	if !strings.Contains(next, "S6 stays parked with nothing on record to end it now that A1 is answered") || !strings.Contains(next, "--until WHEN") {
		t.Fatalf("%q", next)
	}
	if _, parks := statusOf(t, r, "S6"); len(parks) != 1 || !strings.HasPrefix(parks[0], "park of S6 has nothing on record to end it") {
		t.Fatalf("%q", parks)
	}
	r.w.Clock.Sleep(2 * time.Minute) // past the run that answered
	if _, err := r.d.DueAll(); err != nil || l.count() != 1 || !strings.Contains(l.output(0), "park of S6 has nothing") {
		t.Fatalf("%d posts: %v", l.count(), err)
	}
}

// A step sent ends the park and its time with it.
func TestASendEndsTheParkAndItsTime(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "S6")
	if err := r.d.Park(r.view("S6"), "the fleet read", r.w.Clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	done(c, "start", "ready")
	if _, err := r.d.Send(r.view("S6"), drive.Step{Line: "take the read"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	r.w.Clock.Sleep(2 * time.Hour)
	evs, err := r.b.Events()
	if err != nil {
		t.Fatal(err)
	}
	m := batch.Find(batch.Members(evs), "S6")
	if m.Parked != "" || !m.ParkedUntil.IsZero() {
		t.Fatalf("%+v", m)
	}
	if _, parks := statusOf(t, r, "S6"); len(parks) != 0 {
		t.Fatalf("%q", parks)
	}
}
