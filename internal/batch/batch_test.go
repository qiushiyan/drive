package batch

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewUseCurrent(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if _, err := s.Current(); err != ErrNoBatch {
		t.Fatalf("no batch yet: %v", err)
	}
	b, err := s.New(Meta{Name: "2026-10-22", Started: time.Now(), Wake: "webhook"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.New(Meta{Name: "2026-10-22"}); err == nil || !strings.Contains(err.Error(), "batch use") {
		t.Fatalf("a used name is refused: %v", err)
	}
	if _, err := s.New(Meta{Name: "../x"}); err == nil {
		t.Fatal("a name that leaves the store is refused")
	}
	cur, err := s.Current()
	if err != nil || cur.Meta.Name != b.Meta.Name || cur.Meta.Wake != "webhook" {
		t.Fatalf("current: %+v %v", cur, err)
	}
	if _, err := s.New(Meta{Name: "next", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Use("2026-10-22"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Current(); cur.Meta.Name != "2026-10-22" {
		t.Fatal("use switches the current batch")
	}
}

func TestSendsAreNeverOverwritten(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	b, _ := s.New(Meta{Name: "b", Started: time.Now()})
	p1, _ := b.SaveSend("T1", []byte("one"))
	p2, _ := b.SaveSend("T1", []byte("two"))
	if p1 == p2 {
		t.Fatal("each send gets its own file")
	}
	if got, _ := os.ReadFile(p1); string(got) != "one" {
		t.Fatalf("the first file keeps what was sent: %q", got)
	}
}

func TestDerivedViews(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	evs := []Event{
		{At: at(0), Kind: KindOpen, Session: "T1", Block: "block:aaaa-111111"},
		{At: at(1), Kind: KindSendIntent, Session: "T1", Intent: "i1", Hold: "release"},
		{At: at(2), Kind: KindSend, Session: "T1", Intent: "i1", Outcome: "arrived", Hold: "release"},
		{At: at(2), Kind: KindSendIntent, Session: "T1", Intent: "i0", Hold: "deploy"},
		{At: at(2), Kind: KindFailure, Session: "T1", Intent: "i0", Reason: "Type failed"},
		{At: at(3), Kind: KindSendIntent, Session: "T1", Intent: "i2", Text: "next"},
		{At: at(4), Kind: KindGrant, ID: "G1", Scope: "merge"},
		{At: at(5), Kind: KindAsk, ID: "A1", Question: "q"},
		{At: at(9), Kind: KindAnswer, Ref: "A1", Took: "recommended"},
		{At: at(10), Kind: KindPark, Session: "T1", Reason: "waits on Qiushi"},
	}
	ms := Members(evs)
	if len(ms) != 1 || ms[0].LastSend == nil || ms[0].Parked != "waits on Qiushi" {
		t.Fatalf("members: %+v", ms[0])
	}
	if Find(ms, "111111") != ms[0] || Find(ms, "T1") != ms[0] || Find(ms, "11") != nil {
		t.Fatal("find by label or six characters of the block")
	}
	if p := Unresolved(evs, "T1"); len(p) != 1 || p[0].Intent != "i2" {
		t.Fatalf("the intent with no outcome is unresolved: %+v", p)
	}
	// A hold is taken from admission; a send that failed before reaching its session took none.
	if h := Holds(evs); len(h) != 1 || h[0].Session != "T1" || h[0].Name != "release" || !h[0].Since.Equal(at(1)) || h[0].Pending {
		t.Fatalf("holds: %+v", h)
	}
	g, asks, _ := Records(evs)
	if len(g) != 1 || len(asks) != 1 || asks[0].Answer == nil || asks[0].Answer.Took != "recommended" {
		t.Fatalf("records: %+v %+v", g, asks)
	}
	if NextID(evs, KindAsk, "A") != "A2" {
		t.Fatal("next id")
	}
}

func TestOwnRefusesASecondDriver(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	release, err := s.Own("block:1", "sending T1 \"recap\"")
	if err != nil {
		t.Fatal(err)
	}
	if o := s.Busy("block:1"); o == nil || o.What != "sending T1 \"recap\"" {
		t.Fatalf("busy names the owner: %+v", o)
	}
	var busy *Busy
	if _, err := s.Own("block:1", "closing T1"); !errors.As(err, &busy) || busy.What != "sending T1 \"recap\"" {
		t.Fatalf("a second owner is refused with the first's action: %v", err)
	}
	if _, err := s.Own("block:2", "x"); err != nil {
		t.Fatalf("another session is free: %v", err)
	}
	release()
	if s.Busy("block:1") != nil {
		t.Fatal("released")
	}
	again, err := s.Own("block:1", "closing T1")
	if err != nil {
		t.Fatal(err)
	}
	again()
}

// A relaunch skips the notice only for a session that read it: one whose
// notice never reached it gets it with the step it is relaunched with.
func TestAResumedSessionHasReadTheNoticeOnlyIfItDid(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		resumed bool
	}{{"arrived", true}, {"unknown", false}} {
		evs := []Event{
			{Kind: KindAdopt, Session: "T4", Block: "block:a-000001", SID: "s1"},
			{Kind: KindSend, Session: "T4", Outcome: tc.outcome, Notice: true},
			{Kind: KindClose, Session: "T4"},
			{Kind: KindOpen, Session: "T4", Block: "block:a-000002", SID: "s1", Ref: "s1"},
		}
		if m := Find(Members(evs), "T4"); m.Resumed != tc.resumed {
			t.Fatalf("notice %s: resumed %v", tc.outcome, m.Resumed)
		}
	}
}
