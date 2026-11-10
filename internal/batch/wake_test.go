package batch

import (
	"testing"
	"time"
)

// The wake's readers, on event sequences the real logs seldom or never hold:
// results that land late, posts that overlap, records an older drive wrote.

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// seq stamps events a minute apart, in order, so a test reads as its log would.
func seq(evs ...Event) []Event {
	for i := range evs {
		if evs[i].At.IsZero() {
			evs[i].At = t0.Add(time.Duration(i) * time.Minute)
		}
	}
	return evs
}

func open(label, block string) Event {
	return Event{Kind: KindOpen, Session: label, Block: block, SID: "sid-" + label}
}
func watch(label string) Event { return Event{Kind: KindWatch, Session: label} }
func posting(label, view string) Event {
	return Event{Kind: KindPosting, Session: label, View: view, Why: "turn-ended"}
}
func wake(label, view string, http int) Event {
	return Event{Kind: KindWake, Session: label, View: view, HTTP: http, Why: "turn-ended"}
}
func sent(label string) Event { return Event{Kind: KindSend, Session: label, Outcome: "arrived"} }
func seen() Event             { return Event{Kind: KindSeen} }

func standing(t *testing.T, evs []Event, label string, now time.Time) WakeStanding {
	t.Helper()
	m := Find(Members(evs), label)
	if m == nil {
		t.Fatalf("no member %s", label)
	}
	s, _ := m.Wake.Standing(now)
	return s
}

func TestHowAWakeStands(t *testing.T) {
	late := t0.Add(time.Hour)
	for _, tc := range []struct {
		name string
		evs  []Event
		now  time.Time
		want WakeStanding
	}{
		{"never watched", seq(open("A", "b1")), late, WakeNever},
		{"armed, nothing since", seq(open("A", "b1"), watch("A")), late, WakeNoResult},
		{"a post under way", seq(open("A", "b1"), watch("A"), posting("A", "v1")), t0.Add(5 * time.Minute), WakePosting},
		// Its watch died posting: retries end within PostLimit, so the post is no longer under way.
		{"a post that never returned", seq(open("A", "b1"), watch("A"), posting("A", "v1")), t0.Add(2*time.Minute + PostLimit), WakeNoResult},
		{"a post that failed", seq(open("A", "b1"), watch("A"), posting("A", "v1"), wake("A", "v1", 0)), late, WakeNoResult},
		{"posted, nothing done", seq(open("A", "b1"), watch("A"), posting("A", "v1"), wake("A", "v1", 204)), late, WakeUnhandled},
		{"posted, then a step", seq(open("A", "b1"), watch("A"), posting("A", "v1"), wake("A", "v1", 204), sent("A")), late, WakeActed},
		{"a step, then its watch", seq(open("A", "b1"), watch("A"), posting("A", "v1"), wake("A", "v1", 204), sent("A"), watch("A")), late, WakeNoResult},
		{"stopped", seq(open("A", "b1"), watch("A"), Event{Kind: KindWakeStop, Session: "A"}), late, WakeStopped},
		{"stopped, then armed again", seq(open("A", "b1"), watch("A"), Event{Kind: KindWakeStop, Session: "A"}, watch("A")), late, WakeNoResult},
		{"killed while it posted", seq(open("A", "b1"), watch("A"), posting("A", "v1"), WatchEnded(time.Time{}, "A", "killed by terminated")), t0.Add(4 * time.Minute), WakeNoResult},
		// A watch retires before it posts, so a step can arm the next one before
		// the post returns: that result is the old watch's, and the new one still waits.
		{"a result that lands after a newer watch", seq(open("A", "b1"), watch("A"), posting("A", "v1"), sent("A"), watch("A"), wake("A", "v1", 204)), late, WakeNoResult},
		// A watch that waits on a job posts and runs on: its second post failing
		// leaves the first as what the driver was told.
		{"a second post fails", seq(open("A", "b1"), watch("A"), posting("A", "v1"), wake("A", "v1", 204), posting("A", "v1"), wake("A", "v1", 0)), late, WakeUnhandled},
		{"an older drive's wake, with no posting", seq(open("A", "b1"), watch("A"), wake("A", "v1", 204)), late, WakeUnhandled},
		// The label was reopened in a new block: a result from the old block's post is not its.
		{"a result from the block before", seq(open("A", "b1"), watch("A"), Event{Kind: KindPosting, Session: "A", Block: "b1", View: "v1"},
			open("A", "b2"), Event{Kind: KindWake, Session: "A", Block: "b1", View: "v1", HTTP: 204}), late, WakeNever},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := standing(t, tc.evs, "A", tc.now); got != tc.want {
				t.Fatalf("stands %d, want %d", got, tc.want)
			}
		})
	}
}

// A wake is answered by the first trace a driver leaves from when its post
// began until the session's next post begins: a run can act before the post
// that started it returns, and one run answers the newest wake, not every wake
// that went unheard before it.
func TestWhichWakeARunAnswers(t *testing.T) {
	told := func(evs []Event) Told {
		t.Helper()
		got, ok := Find(Members(evs), "A").Wake.Told()
		if !ok {
			t.Fatal("no wake reached the driver")
		}
		return got
	}
	base := []Event{open("A", "b1"), watch("A")}
	with := func(more ...Event) []Event { return seq(append(append([]Event{}, base...), more...)...) }

	if got := told(with(posting("A", "v1"), wake("A", "v1", 204))); got.Heard || got.Times != 1 || got.Acked != 0 {
		t.Fatalf("no run yet: %+v", got)
	}
	if got := told(with(posting("A", "v1"), seen(), wake("A", "v1", 204))); !got.Heard || got.Acked != 1 {
		t.Fatalf("a run that read the batch before the post returned heard it: %+v", got)
	}
	if got := told(with(seen(), posting("A", "v1"), wake("A", "v1", 204))); got.Heard {
		t.Fatalf("a trace from before the post answers nothing: %+v", got)
	}
	six := with(posting("A", "v1"), wake("A", "v1", 204), posting("A", "v1"), wake("A", "v1", 204), posting("A", "v1"), wake("A", "v1", 204), seen())
	if got := told(six); got.Times != 3 || got.Acked != 1 || !got.Heard {
		t.Fatalf("one run answers the newest of three wakes for a view: %+v", got)
	}
	if got := told(append(six, seq(posting("A", "v2"), wake("A", "v2", 204))...)); got.Times != 1 || got.Acked != 0 || got.View != "v2" {
		t.Fatalf("a new view counts afresh: %+v", got)
	}
	// A newer post that failed leaves the wake before it as what the driver was
	// told, and unanswered: the trace that followed the failure answered nothing.
	if got := told(with(posting("A", "v1"), wake("A", "v1", 204), posting("A", "v1"), wake("A", "v1", 0), seen())); got.Heard || got.Times != 1 {
		t.Fatalf("a failed post was not heard: %+v", got)
	}
}

// Two timers can ring at once, both under no session: each wake ends the
// oldest post still open, so a run that answered both leaves no strike.
func TestOverlappingPostsPairInOrder(t *testing.T) {
	evs := seq(posting("", ""), posting("", ""), seen(), wake("", "", 204), wake("", "", 204))
	posts := Posts(evs)
	if len(posts) != 2 || posts[0].Pending() || posts[1].Pending() || !posts[0].Ended.Before(posts[1].Ended) || posts[1].Heard.IsZero() {
		t.Fatalf("each wake ends the oldest open post: %+v", posts)
	}
	if h := Hearings(evs, t0.Add(time.Hour)); h.Strikes != 0 {
		t.Fatalf("both were answered before they returned: %+v", h)
	}
	// With the second still open, it is the post under way.
	if posts := Posts(evs[:4]); posts[0].Pending() || !posts[1].Pending() {
		t.Fatalf("the second timer still posts: %+v", posts)
	}
}

// An ending a post carried goes nowhere when that post fails, and a session's
// next post overtakes one that never returned: neither stays under way.
func TestAPostWithNoResultIsOver(t *testing.T) {
	carried := Event{Kind: KindPosting, Session: "B", View: "v1", Via: "A"}
	posts := Posts(seq(posting("A", "v1"), carried, wake("A", "v1", 0)))
	if len(posts) != 2 || posts[1].Pending() || !posts[1].Lost() || posts[1].Delivered() {
		t.Fatalf("a failed carrier ends what it carried: %+v", posts)
	}
	posts = Posts(seq(posting("A", "v1"), posting("A", "v2"), wake("A", "v2", 204)))
	if len(posts) != 2 || !posts[0].Lost() || posts[1].View != "v2" || !posts[1].Delivered() {
		t.Fatalf("the newer post takes the result: %+v", posts)
	}
}

// An open is two events, the block and then the session Claude Code started
// in it: one member, whose record of what was done to it stands across both,
// however often the second is said.
func TestAnOpenIsOneMember(t *testing.T) {
	block := Event{Kind: KindOpen, Session: "A", Block: "b1", Intent: "i1", Outcome: "starting"}
	started := Event{Kind: KindOpen, Session: "A", Block: "b1", SID: "sid-A", Account: "acct"}
	evs := seq(Event{Kind: KindOpenIntent, Session: "A", Intent: "i1", Mode: "line", Text: "go"}, block)
	m := Find(Members(evs), "A")
	if !m.Starting || m.Opening == nil || m.Opening.Text != "go" || m.SID != "" {
		t.Fatalf("its block is on record with what was asked, and no session: %+v", m)
	}
	evs = seq(append(evs, started, watch("A"), posting("A", "v1"), wake("A", "v1", 204), started)...)
	ms := Members(evs)
	m = Find(ms, "A")
	if len(ms) != 1 || m.Starting || m.SID != "sid-A" || m.Account != "acct" || !m.Joined.Equal(evs[1].At) {
		t.Fatalf("started, it is the same member, open: %+v", m)
	}
	if s, _ := m.Wake.Standing(t0.Add(time.Hour)); s != WakeUnhandled {
		t.Fatalf("saying it started a second time takes nothing from its record: %d", s)
	}
}
