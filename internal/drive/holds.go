package drive

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

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
func (d *Driver) holdNext(b *batch.Batch, h heldBy, label, view, retry string, open bool) string {
	var next string
	if b.Meta.Wake != "webhook" {
		wait := fmt.Sprintf("wait for %s's turn to end (%s)", h.Session, d.run("wait", h.Session))
		switch {
		case h.prompt:
			wait = fmt.Sprintf("answer %s's prompt (%s), %s", h.Session, d.run("read", h.Session, "--pending"), wait)
		case h.job != nil:
			wait = fmt.Sprintf("wait for %s, which usually goes on once its job ends (%s)", h.Session, d.run("wait", h.Session))
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
			next = fmt.Sprintf("%s waits on a prompt, so answer it first (%s); its wake comes when its turn then ends and %s", h.Session, d.run("read", h.Session, "--pending"), names)
		case h.job != nil && open:
			next = fmt.Sprintf("nothing to run now: %s usually goes on once its job ends, and the wake of that turn, which %s arms, %s", h.Session, d.run("watch", "--due"), names)
		case h.job != nil:
			next, others = h.Session+" usually goes on once its job ends", "nothing wakes you"
		default:
			next = fmt.Sprintf("%s's wake comes when its turn ends and %s", h.Session, names)
		}
		if view != "" {
			next += fmt.Sprintf("; %s for %s, so park it meanwhile: %s; %s wakes it once %s is free",
				others, label, d.run("park", view, text("waits for "+h.Name+", held by "+h.Session)), d.run("watch", "--due"), h.Name)
		}
	}
	if h.job != nil {
		// A job can outlast what the hold was taken for by hours, as an eval
		// started beside a release did: only the driver can tell the two apart.
		next += fmt.Sprintf("; if that job is not what %s was taken for, free the hold, saying why: %s; if it should have ended, have %s stop it: %s",
			h.Name, d.run("free", h.Name, text("…")), h.Session, d.stopJob(h.view, *h.job))
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
				fmt.Fprintf(&out, "waiting for %s, which %s held: %s; open it once %s is free (%s): %s\n",
					h.Name, label, w.Session, h.Name, d.run("status"), w.Step)
				continue
			}
			view := w.Session + "@VIEW"
			if o, err := d.observe(b, members, w.Session); err == nil {
				view = o.View.String()
			}
			fmt.Fprintf(&out, "waiting for %s, which %s held: %s; send it once %s is free (%s): %s\n",
				h.Name, label, w.Session, h.Name, d.run("status"), d.sendCommand(view, refusedStep(w), SendOptions{Hold: h.Name}))
		}
	}
	return out.String()
}

// heldBy is a hold as it stands: who holds it and why, whether the holder
// waits on a prompt, and, where a job it started is what holds it, that job
// and the holder's view.
type heldBy struct {
	batch.Hold
	why     string
	prompt  bool
	job     *session.Shell
	view    string
	sending bool // the send that took it is under way
	unread  bool // its holder cannot be read, so nothing says where its turn stands
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
			hb.why, hb.sending = "whose send is still under way", true
			return hb, true
		}
		s, err := session.Locate(d.Term, d.Host, m.Block)
		if errors.Is(err, session.ErrNoClaude) || err != nil && !d.blockExists(m.Block) {
			return hb, false
		}
		if err != nil {
			hb.why, hb.unread = "whose session cannot be read", true
			return hb, true
		}
		f, err := s.Frontier()
		if err != nil {
			hb.why, hb.unread = "whose session cannot be read", true
			return hb, true
		}
		t := d.turnOf(s, f, h.Since)
		switch {
		case t.State.onPrompt():
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

// holdNow is a hold as status lists it: whether it is held, by whom and
// why, and the sessions waiting for it with the step each was refused.
type holdNow struct {
	Name    string
	held    bool
	by      heldBy
	waiting []batch.Hold
}

// holdsNow reads every hold the batch has taken or refused a step for, in
// name order.
func (d *Driver) holdsNow(evs []batch.Event, members []*batch.Member) []holdNow {
	names := map[string]bool{}
	for _, h := range batch.Holds(evs) {
		names[h.Name] = true
	}
	waiting := batch.Waiting(evs)
	for _, w := range waiting {
		names[w.Name] = true
	}
	var out []holdNow
	for _, name := range sortedKeys(names) {
		h := holdNow{Name: name}
		h.by, h.held = d.holder(evs, members, name, "")
		for _, w := range waiting {
			if w.Name == name {
				h.waiting = append(h.waiting, w)
			}
		}
		out = append(out, h)
	}
	return out
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

// refusedStep is the step a session waiting for a hold was refused.
func refusedStep(w batch.Hold) Step {
	switch {
	case w.Key != "":
		return Step{Key: w.Key, Text: w.Step}
	case w.Path != "":
		return Step{File: w.Path}
	}
	return Step{Line: w.Step}
}

// Freed reports a free: the hold a driver handed back, or one that nothing
// held.
type Freed struct {
	Name  string
	By    string    // the session it was freed from; "" when nothing held it
	Since time.Time // when that session took it
	Job   string    // the background job that kept it
	// Unnamed says no step in the batch has named the hold, and Holds the
	// ones its steps have: a mistyped name would otherwise read as free.
	Unnamed bool
	Holds   []string
	// Next is the send or open of each session that waits for the hold.
	Next string
	zone *time.Location
}

// Free hands back a hold its holder's turn no longer needs: the turn has
// ended and only a background job it started keeps the hold, as when a step
// sent with --hold release also started an eval that ran nine hours, and
// nine sends were refused on a release long done (2026-11-09). Only the
// driver can say the job is not the held work, so it says why, and the log
// keeps that. While the holder's turn runs, waits on a prompt, or its send
// is under way, the hold is the turn's and the free is refused; a hold
// nothing holds is reported free. Freed, it is no one's until a later send
// or open takes it.
func (d *Driver) Free(name, reason string) (*Freed, error) {
	reason = strings.Join(strings.Fields(reason), " ")
	if !strings.ContainsFunc(reason, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return nil, &Refused{Reason: "a free says why, in words: what shows its holder no longer needs the hold"}
	}
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	again := d.run("free", name, text(reason))
	out := &Freed{Name: name, zone: b.Zone()}
	holder := ""
	err = b.Locked(func(evs []batch.Event) error {
		members := batch.Members(evs)
		h, held := d.holder(evs, members, name, "")
		switch {
		case !held:
			out.Holds = holdNames(evs)
			out.Unnamed = !slices.Contains(out.Holds, name)
		case h.job == nil:
			holder = h.Session
			return d.unfreed(b, members, h, again)
		default:
			out.By, out.Since, out.Job = h.Session, h.Since, h.job.Command
			if err := b.Append(batch.Event{At: d.now(), Kind: batch.KindFree, Session: h.Session, Hold: name, Intent: h.Intent, Reason: reason}); err != nil {
				return err
			}
		}
		out.Next = d.waitersNext(b, evs, members, name)
		return nil
	})
	if err != nil {
		return nil, d.refuse(b, holder, unsent(err, again))
	}
	return out, nil
}

// unfreed refuses the free of a hold its holder's turn still needs, saying
// what keeps it and what to run from there. again is the free itself.
func (d *Driver) unfreed(b *batch.Batch, members []*batch.Member, h heldBy, again string) *Refused {
	r := &Refused{Reason: fmt.Sprintf("%s stays held by %s since %s, %s: a hold is freed only once its holder's turn has ended",
		h.Name, h.Session, h.Since.In(b.Zone()).Format("15:04"), h.why)}
	ends := fmt.Sprintf("%s's wake comes when its turn ends", h.Session)
	if b.Meta.Wake != "webhook" {
		ends = fmt.Sprintf("wait for %s's turn to end (%s)", h.Session, d.run("wait", h.Session))
	}
	switch {
	case h.sending:
		r.Next = fmt.Sprintf("let that send finish; %s then shows where %s stands", d.run("read", h.Session), h.Session)
	case h.unread:
		r.Next = "nothing was done; try it again: " + again
	case h.prompt:
		answer := d.run("read", h.Session, "--pending")
		if o, err := d.observe(b, members, h.Session); err == nil {
			answer = d.answerNext(h.Session, o.View.String(), o.State())
		}
		r.Next = fmt.Sprintf("answer %s's prompt first: %s", h.Session, answer)
	default:
		r.Next = fmt.Sprintf("%s; if a background job still keeps %s then, and it is not what %s was taken for, run this again: %s", ends, h.Name, h.Name, again)
	}
	return r
}

// waitersNext is what to run for each session that waits for the hold name,
// now that it is free: the send or open it was refused; "" when none waits.
func (d *Driver) waitersNext(b *batch.Batch, evs []batch.Event, members []*batch.Member, name string) string {
	var who, runs []string
	for _, w := range batch.Waiting(evs) {
		if w.Name != name {
			continue
		}
		run := "open it now: " + w.Step // a refused open, whole
		if !w.Open {
			run = fmt.Sprintf("read it, then send its step with --hold %s: %s", name, d.run("read", w.Session))
			if o, err := d.observe(b, members, w.Session); err == nil {
				run = "send its step now: " + d.sendCommand(o.View.String(), refusedStep(w), SendOptions{Hold: name})
			}
		}
		who, runs = append(who, w.Session), append(runs, run)
	}
	switch len(who) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s waits for %s; %s", who[0], name, runs[0])
	}
	for i := range runs {
		runs[i] = who[i] + ", " + runs[i]
	}
	return fmt.Sprintf("%s wait for %s, and the first step sent takes it: %s", andList(who), name, strings.Join(runs, "; "))
}

// holdNames are the holds the batch's steps have named, taken or refused,
// in name order.
func holdNames(evs []batch.Event) []string {
	names := map[string]bool{}
	for _, e := range evs {
		if e.Hold != "" {
			names[e.Hold] = true
		}
	}
	return sortedKeys(names)
}
