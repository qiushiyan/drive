package drive

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// holdRefused is a send refused because another session holds what it needs.
type holdRefused struct {
	Refused
	hold string
	step Step
	open string // for a refused open, the drive open to run once the hold is free
}

func (h *holdRefused) Unwrap() error { return &h.Refused }

// holdNext is what the driver of a send or open refused on h does
// meanwhile, from what keeps the hold: a turn ends by itself, a prompt waits
// for its answer, and a job ends in its own time, its session usually going
// on after it. view is the refused session's when it can be parked, "" for an
// open or a session outside the batch; retry is the refused command.
func holdNext(b *batch.Batch, h heldBy, label, view, retry string, open bool) string {
	var next string
	if b.Meta.Wake != "webhook" {
		wait := fmt.Sprintf("wait for %s's turn to end (drive wait %s)", h.Session, h.Session)
		switch {
		case h.prompt:
			wait = fmt.Sprintf("answer %s's prompt (drive read %s --pending), %s", h.Session, h.Session, wait)
		case h.job != nil:
			wait = fmt.Sprintf("wait for %s, which usually goes on once its job ends (drive wait %s)", h.Session, h.Session)
		}
		again := "send this again: "
		if open {
			again = "open it: "
		}
		next = wait + ", then " + again + retry
	} else {
		names := fmt.Sprintf("names %s as waiting for %s", label, h.Name)
		if open {
			names = "prints this open: " + retry
		}
		others := "nothing else wakes you"
		switch {
		case h.prompt:
			next = fmt.Sprintf("%s waits on a prompt, so answer it first (drive read %s --pending); its wake comes when its turn then ends and %s", h.Session, h.Session, names)
		case h.job != nil && open:
			next = fmt.Sprintf("nothing to run now: %s usually goes on once its job ends, and the wake of that turn, which drive watch --due arms, %s", h.Session, names)
		case h.job != nil:
			next, others = h.Session+" usually goes on once its job ends", "nothing wakes you"
		default:
			next = fmt.Sprintf("%s's wake comes when its turn ends and %s", h.Session, names)
		}
		if view != "" {
			next += fmt.Sprintf("; %s for %s, so park it meanwhile: drive park %s %s; drive watch --due wakes it once %s is free",
				others, label, view, shellQuote("waits for "+h.Name+", held by "+h.Session), h.Name)
		}
	}
	if h.job != nil {
		next += fmt.Sprintf("; if that job should have ended, have %s stop it: %s", h.Session, stopJob(h.Session, h.view, *h.job))
	}
	return next
}

// waiterLines names, for a session whose turn has ended, each session
// waiting on a hold it held, with the send to run once the hold is free.
func (d *Driver) waiterLines(b *batch.Batch, label string) string {
	evs, err := b.Events()
	if err != nil {
		return ""
	}
	members := batch.Members(evs)
	var out strings.Builder
	for _, h := range batch.Holds(evs) {
		if h.Session != label {
			continue
		}
		for _, w := range batch.Waiting(evs) {
			if w.Name != h.Name {
				continue
			}
			if w.Open {
				fmt.Fprintf(&out, "waiting for %s, which %s held: %s; open it once %s is free (drive status): %s\n",
					h.Name, label, w.Session, h.Name, w.Command())
				continue
			}
			view := w.Session + "@VIEW"
			if o, err := d.observe(members, w.Session); err == nil {
				view = o.View.String()
			}
			fmt.Fprintf(&out, "waiting for %s, which %s held: %s; send it once %s is free (drive status): drive send %s %s --hold %s\n",
				h.Name, label, w.Session, h.Name, view, w.Command(), h.Name)
		}
	}
	return out.String()
}

// heldBy is a hold as it stands: who holds it and why, whether the holder
// waits on a prompt, and, where a job it started is what holds it, that job
// and the holder's view.
type heldBy struct {
	batch.Hold
	why    string
	prompt bool
	job    *session.Shell
	view   string
}

// holder says whether a hold is taken by a session other than label. It
// holds while the send that took it is under way, while its holder works,
// later steps included, since a follow-up to a closeout is still the
// closeout, while it waits on a prompt, and while background jobs it started
// since run. A holder that cannot be read holds; only one that is gone or
// never received the send does not.
func (d *Driver) holder(evs []batch.Event, members []*batch.Member, name, label string) (heldBy, bool) {
	for _, h := range batch.Holds(evs) {
		if h.Name != name || h.Session == label {
			continue
		}
		hb := heldBy{Hold: h, why: "whose turn is still running"}
		m := batch.Find(members, h.Session)
		if m == nil || m.Closed {
			return hb, false
		}
		if h.Pending && d.Store.Busy(m.Block) != nil {
			hb.why = "whose send is still under way"
			return hb, true
		}
		s, err := session.Locate(d.Term, d.Host, m.Block)
		if errors.Is(err, session.ErrNoClaude) || err != nil && !d.blockExists(m.Block) {
			return hb, false
		}
		if err != nil {
			hb.why = "whose session cannot be read"
			return hb, true
		}
		f, err := s.Frontier()
		if err != nil {
			hb.why = "whose session cannot be read"
			return hb, true
		}
		t := d.turnOf(s, f, h.Since)
		switch {
		case t.waiting():
			hb.why, hb.prompt = "who waits on a prompt", true
			return hb, true
		case t.State == "working" && sentSince(evs, h):
			hb.why = "who works on a later step"
			return hb, true
		case t.State == "working":
			return hb, true
		}
		if p := f.Newest(); p == nil || p.At.Before(h.Since) {
			return hb, false // the send never reached it
		}
		// A turn that ended while a job it started runs, such as a wait on
		// checks before a merge, resumes when the job ends.
		if t.Over && len(t.Jobs) > 0 {
			hb.job, hb.view = &t.Jobs[0], turn.ViewOf(m.Label, s.ID, f).String()
			hb.why = "whose turn ended, but a background job it started still runs: " + clip(t.Jobs[0].Command, 120)
		}
		return hb, !t.Over || t.Shells > 0
	}
	return heldBy{}, false
}

// sentSince says whether the holder was sent a step after the one that
// took the hold.
func sentSince(evs []batch.Event, h batch.Hold) bool {
	var took time.Time
	for _, e := range evs {
		switch {
		case e.Kind != batch.KindSend || e.Session != h.Session || e.Outcome == "":
		case e.Intent == h.Intent:
			took = e.At
		case !took.IsZero() && e.Mode != "notice" && e.At.After(took):
			return true
		}
	}
	return false
}

func (d *Driver) blockExists(block string) bool {
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return true
	}
	for _, b := range blocks {
		if b.ID == block {
			return true
		}
	}
	return false
}
