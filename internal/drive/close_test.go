package drive_test

import (
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// On 11-11 sr-search-repair finished with a stray "h" in its input box. The
// owner answered the ask "close the tab", and the close was refused all the
// same, its next naming only the ask and the park, so the bot told the owner drive
// had no way past it and parked the session overnight. The clear it needed
// was there: the refusal now names it, and the close goes on the same view.
func TestALineTheOwnerLetsGoIsClearedAndTheSessionClosed(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	done(c, "closeout", "closed out")
	woken(r, c)
	c.Edit(func(c *testkit.Claude) { c.Input = "h" })
	v := r.view("T1")
	next := nextOf(r.d.Close(v))
	clear, closeIt := "drive --batch b1 respond "+v+" clear", "drive --batch b1 close "+v
	if !strings.Contains(next, clear+", then "+closeIt) {
		t.Fatalf("the refusal names the clear and the close that follow the owner's word: %q", next)
	}
	if _, err := r.d.Respond(v, "clear"); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Close(v); err != nil {
		t.Fatalf("the close goes on the view it was refused on: %v", err)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 0 {
		t.Fatalf("its block is closed: %+v", blocks)
	}
}

// Then the owner closed its tab by hand, and drive could not put that on
// record: a gone session has no view, every report of it named only the open
// that brings it back, and its park stood. As on 11-09, when a parked tab
// the owner closed came back. A gone session's next now names the close
// first, and the close by its label ends its park and its wake, takes no
// view, and says how to bring it back.
func TestATabTheOwnerClosedIsClosedOnRecord(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	if err := r.d.Park(r.view("T1"), "finished; an unsent line keeps its tab open", r.later()); err != nil {
		t.Fatal(err)
	}
	if err := r.w.Close(c.Block); err != nil { // the owner closes the tab
		t.Fatal(err)
	}

	end := "drive --batch b1 close T1"
	rd, err := r.d.Read("T1", drive.ReadLast, 10)
	if err != nil || rd.State != "gone" || !strings.HasPrefix(rd.Next, "its block no longer exists; if it was closed to end the session, put that on record: "+end+"; if not, bring it back") {
		t.Fatalf("a read of it names the close before the open: %+v %v", rd, err)
	}
	if _, later, err := r.d.Due(); err != nil || len(later) != 1 || later[0].Gone != rd.Next {
		t.Fatalf("so does the watchdog: %+v %v", later, err)
	}
	if err := r.d.Close("T1"); !isRefused(err) || !strings.Contains(err.Error(), "is a session, not a view") {
		t.Fatalf("a close by label on a view still asks for a view: %v", err)
	}

	out, err := r.d.CloseGone("T1")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Text(); !strings.HasPrefix(got, "closed T1, which was gone: its block no longer exists\nif that was not its end, bring it back under its label") ||
		!strings.Contains(got, "--resume "+c.SID) {
		t.Fatalf("the close says what became of the block and how to bring the session back: %q", got)
	}
	evs, _ := r.b.Events()
	m := batch.Find(batch.Members(evs), "T1")
	if m == nil || !m.Closed {
		t.Fatalf("it is closed on record: %+v", m)
	}
	if closes := r.events(batch.KindClose); len(closes) != 1 || closes[0].Note != "it was gone: its block no longer exists" {
		t.Fatalf("the log says it was gone: %+v", closes)
	}
	if r.watches.Alive(watchOf(c)) {
		t.Fatal("its wake is stopped")
	}
	st, err := r.d.Status(false)
	if err != nil || strings.Contains(st.Text(r.w.Clock.Now()), "is gone") {
		t.Fatalf("status no longer reports it gone: %v\n%s", err, st.Text(r.w.Clock.Now()))
	}
	if _, later, err := r.d.Due(); err != nil || len(later) != 0 {
		t.Fatalf("nor does the watchdog, nor its park: %+v %v", later, err)
	}
	if rd, err := r.d.Read("T1", drive.ReadLast, 10); err != nil || rd.State != "closed" {
		t.Fatalf("a read says closed: %+v %v", rd, err)
	}
	if _, err := r.d.CloseGone("T1"); !isRefused(err) || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("a second close says it is closed: %v", err)
	}
}

// A restart of Rex's server leaves a shell in each block where Claude Code
// ran. Its close takes that shell along, while a block where another
// program now runs is left as it is; a session that runs is closed on a view.
func TestAGoneSessionsBlockIsClosedOnlyWhenOnlyAShellIsLeft(t *testing.T) {
	r := newRig(t, "webhook")
	adopted(t, r, "T1")
	other := adopted(t, r, "T2")
	live := adopted(t, r, "T3")
	r.w.Restore()
	r.w.BackInBlock(live.Block)
	other.Edit(func(c *testkit.Claude) { c.Front = "vim" })

	if rd, err := r.d.Read("T1", drive.ReadLast, 10); err != nil || !strings.HasPrefix(rd.Next, "its Claude Code no longer runs in its block; if its work is done, close it: drive --batch b1 close T1; if not, bring it back") {
		t.Fatalf("a shell left in its block reads as Claude Code ended: %+v %v", rd, err)
	}
	out, err := r.d.CloseGone("T1")
	if err != nil || out.Block != "the shell left in its block is closed too" {
		t.Fatalf("the shell goes with it: %+v %v", out, err)
	}
	if out, err := r.d.CloseGone("T2"); err != nil || out.Block != "its block is left open, as vim runs in front there" {
		t.Fatalf("another program keeps its block: %+v %v", out, err)
	}
	blocks, _ := r.w.Blocks("")
	if len(blocks) != 2 || blocks[0].ID != other.Block || blocks[1].ID != live.Block {
		t.Fatalf("only the shell's block closed: %+v", blocks)
	}
	if _, err := r.d.CloseGone("T3"); !isRefused(err) || !strings.Contains(err.Error(), "T3 still runs") || !strings.Contains(err.Error(), "drive --batch b1 read T3") {
		t.Fatalf("a session that runs is closed on what is read of it: %v", err)
	}
	if len(r.events(batch.KindClose)) != 2 {
		t.Fatalf("two closes on record: %+v", r.events(batch.KindClose))
	}
}
