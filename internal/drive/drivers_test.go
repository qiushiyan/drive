package drive_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
)

// newDriverBatch starts a batch for driver, as its own batch new would,
// and has r act as that driver.
func newDriverBatch(t *testing.T, r *rig, name, driver string) *batch.Batch {
	t.Helper()
	// A batch with no webhook named wakes through the configuration's, which the rig sets.
	b, _, err := r.d.Store.New(batch.Meta{Name: name, Started: r.w.Clock.Now(), Wake: "webhook", Driver: driver})
	if err != nil {
		t.Fatal(err)
	}
	r.d.DriverName = driver
	return b
}

// A session belongs to the live batch that brought it in: another driver
// reads it and is told whose it is, and every action of theirs on it is
// refused before it touches the session. The driver that brought it in
// carries it over into its own next batch.
func TestASessionAnotherDriverDrivesIsTheirs(t *testing.T) {
	r := newRig(t, "webhook")
	newDriverBatch(t, r, "2026-10-27", "session-driver")
	on := adopted(t, r, "ON")
	block := on.Block[len(on.Block)-6:]
	newDriverBatch(t, r, "itell-2026-10-28", "itell")

	rd, err := r.d.Read(block, drive.ReadLast, 10)
	if err != nil || !strings.Contains(rd.DrivenBy, "ON in batch 2026-10-27, driver session-driver") || rd.Next != "" {
		t.Fatalf("a read names the driver that drives it, and gives nothing to run: %+v %v", rd, err)
	}
	v := rd.View.String()
	typed, _ := on.Seen()
	for what, act := range map[string]func() error{
		"send": func() error {
			_, err := r.d.Send(v, drive.Step{Line: "go"}, drive.SendOptions{Label: "I1"})
			return err
		},
		"respond": func() error { _, err := r.d.Respond(v, "interrupt"); return err },
		"park":    func() error { return r.d.Park(v, "waits") },
		"close":   func() error { return r.d.Close(v) },
		"watch":   func() error { _, err := r.d.Watch(block); return err },
	} {
		if err := act(); !isRefused(err) || !strings.Contains(err.Error(), "another driver drives it") {
			t.Errorf("%s on another driver's session is refused: %v", what, err)
		}
	}
	if now, _ := on.Seen(); len(now) != len(typed) || r.watches.Alive("drive-itell-2026-10-28-"+block) {
		t.Fatalf("nothing reached the session, and no wake of itell's watches it: %q", now)
	}
	if st, err := r.d.Status(false); err != nil || len(st.Others) != 1 || !strings.Contains(st.Others[0].DrivenBy, "2026-10-27") {
		t.Fatalf("status lists it as another driver's: %+v %v", st, err)
	}

	newDriverBatch(t, r, "2026-10-28", "session-driver")
	if _, err := r.d.Send(r.view(block), drive.Step{Line: "go on"}, drive.SendOptions{Label: "ON"}); err != nil {
		t.Fatalf("its driver brings it into its next batch, the last one no longer live: %v", err)
	}
	r.d.DriverName = "itell"
	if _, err := r.d.Send(r.view(block), drive.Step{Line: "go"}, drive.SendOptions{Label: "I1"}); !isRefused(err) ||
		!strings.Contains(err.Error(), "batch 2026-10-28") {
		t.Fatalf("and it is that batch's now: %v", err)
	}
}

// An open whose driver ended before Rex opened its block is settled from a
// block under its label; one another driver's batch opened under the same
// label is not it.
func TestASettledOpenTakesNoOtherDriversBlock(t *testing.T) {
	r := newRig(t, "webhook")
	work := filepath.Join(r.w.Root, "work")
	newDriverBatch(t, r, "2026-10-27", "session-driver")
	theirs, err := r.d.Open("S3", drive.Step{}, drive.OpenOptions{Dir: work})
	if err != nil {
		t.Fatal(err)
	}
	it := newDriverBatch(t, r, "itell-2026-10-28", "itell")
	_ = it.Append(batch.Event{At: r.w.Clock.Now(), Kind: batch.KindOpenIntent, Session: "S3", Intent: "gone", Dir: work})
	mine, err := r.d.Open("S3", drive.Step{}, drive.OpenOptions{Dir: work})
	if err != nil || mine.Block == theirs.Block {
		t.Fatalf("itell's S3 opens a block of its own: %+v %v", mine, err)
	}
	evs, _ := it.Events()
	for _, e := range evs {
		if e.Intent == "gone" && e.Kind == batch.KindOpen {
			t.Fatalf("the open settled as session-driver's block: %+v", e)
		}
	}
}
