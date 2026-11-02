package drive

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
)

// Cleaned is what became of one branch a closed session left, or of a closed
// session that left none (Branch "").
type Cleaned struct {
	Batch, Session, Branch string
	Outcome                string // removed, unmerged, left, unread or none
	Reason                 string
}

// Text is the line cleanup prints for it, which says what stands and asks
// nothing: nobody decides anything about a branch gwt will not remove.
func (c Cleaned) Text() string {
	switch c.Outcome {
	case "removed":
		return fmt.Sprintf("%s: removed branch %s and its worktree: it is merged\n", c.Session, c.Branch)
	case "unmerged":
		return fmt.Sprintf("%s: branch %s is not merged, so it stays: gwt remove refuses an unmerged branch\n", c.Session, c.Branch)
	case "left":
		return fmt.Sprintf("%s: branch %s stays: %s\n", c.Session, c.Branch, c.Reason)
	case "unread":
		return fmt.Sprintf("%s: not checked: %s; the next cleanup checks it again\n", c.Session, c.Reason)
	}
	return fmt.Sprintf("%s: left nothing to clean up\n", c.Session)
}

// CleanedText is what cleanup prints.
func CleanedText(cs []Cleaned) string {
	if len(cs) == 0 {
		return "nothing to clean up: every closed session is resolved\n"
	}
	var b strings.Builder
	for _, c := range cs {
		if c.Outcome != "none" {
			b.WriteString(c.Text())
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("nothing to clean up: %d closed sessions left nothing\n", len(cs))
	}
	return b.String()
}

// resolved says the record has the last word on a closed session: it left
// nothing, or everything it left has been removed. What stays is judged again
// each time, since a pull request can merge after its session closed.
func resolved(cs []batch.Cleanup, session string) bool {
	seen := false
	for _, c := range cs {
		if c.Session != session {
			continue
		}
		seen = true
		if c.Outcome != "none" && c.Outcome != "removed" {
			return false
		}
	}
	return seen
}

// drivenBatches are the batches of the driver b belongs to, oldest first: a
// branch a closed session left outlives the batch it was closed in.
func (d *Driver) drivenBatches(b *batch.Batch) []*batch.Batch {
	metas, _ := d.Store.List()
	var out []*batch.Batch
	for _, m := range metas {
		if batch.DriverOf(m) != batch.DriverOf(b.Meta) {
			continue
		}
		if o, err := d.Store.Get(m.Name); err == nil {
			out = append(out, o)
		}
	}
	return out
}

// Cleanup resolves what closed sessions left, in every batch of this driver's,
// or only ref's in the batch it acts in: a merged branch goes with its
// worktree, and one gwt will not remove stays, on the record with why. Status
// then lists it once, as a fact, and a driver's reports need not repeat it.
func (d *Driver) Cleanup(ref string) ([]Cleaned, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	if d.Progress == nil {
		return nil, &Refused{Reason: "this driver reads no progress, so it cannot judge what merged"}
	}
	var out []Cleaned
	batches := d.drivenBatches(b)
	if ref != "" {
		evs, err := b.Events()
		if err != nil {
			return nil, err
		}
		m := batch.Find(batch.Members(evs), ref)
		switch {
		case m == nil:
			return nil, &Refused{Reason: fmt.Sprintf("%s has never been in batch %s", ref, b.Meta.Name), Next: d.run("status") + " lists the batch's sessions by label"}
		case !m.Closed:
			return nil, &Refused{Reason: fmt.Sprintf("%s is still open: its branch may be in use", m.Label),
				Next: "close it first, on the view it stands at: " + d.run("read", m.Label)}
		}
		ref, batches = m.Label, []*batch.Batch{b}
	}
	inUse := d.branchesInUse()
	for _, o := range batches {
		evs, err := o.Events()
		if err != nil {
			return out, err
		}
		done := batch.Cleanups(evs)
		for _, m := range batch.Members(evs) {
			if !m.Closed || ref != "" && m.Label != ref || ref == "" && resolved(done, m.Label) {
				continue
			}
			found, judged := d.cleanSession(o, m, inUse)
			// Judged, a branch that stayed last time and has no verdict now is
			// gone, removed by hand or by its own closeout.
			for _, c := range done {
				if judged && c.Session == m.Label && (c.Outcome == "unmerged" || c.Outcome == "left" || c.Outcome == "unread") &&
					!slices.ContainsFunc(found, func(f Cleaned) bool { return f.Branch == c.Branch }) {
					found = append(found, Cleaned{Batch: o.Meta.Name, Session: m.Label, Branch: c.Branch, Outcome: "none"})
				}
			}
			for _, c := range found {
				if err := o.Append(batch.Event{At: d.now(), Kind: batch.KindCleanup, Session: c.Session, Branch: c.Branch, Outcome: c.Outcome, Reason: c.Reason}); err != nil {
					return append(out, found...), &Failed{What: fmt.Sprintf("cleaned up %s, but the record was not written: %v", c.Session, err), Next: "run it again: " + d.again()}
				}
			}
			out = append(out, found...)
		}
	}
	return out, nil
}

// cleanSession judges and removes what one closed session left. judged is false
// when it could not be read at all, which says nothing of what it left, so the
// record keeps it open to be read again. Nothing left is recorded only once that
// is known: its checkout is gone, it named no branch or commit to judge, or its
// transcript is gone, when nothing further can say.
func (d *Driver) cleanSession(o *batch.Batch, m *batch.Member, inUse map[string]string) (out []Cleaned, judged bool) {
	none := []Cleaned{{Batch: o.Meta.Name, Session: m.Label, Outcome: "none"}}
	if m.Dir == "" {
		return none, true
	}
	if _, err := os.Stat(m.Dir); errors.Is(err, os.ErrNotExist) {
		return none, true // its checkout went with its own closeout
	}
	l, err := d.judged(m)
	switch {
	case errors.Is(err, progress.ErrNothingToJudge):
		return none, true
	case err != nil:
		return []Cleaned{{Batch: o.Meta.Name, Session: m.Label, Outcome: "unread", Reason: firstLine(err.Error())}}, false
	}
	for _, branch := range l.Branches {
		if branch == l.Trunk || slices.ContainsFunc(out, func(c Cleaned) bool { return c.Branch == branch }) {
			continue
		}
		c := Cleaned{Batch: o.Meta.Name, Session: m.Label, Branch: branch}
		merged, verdict := l.Merged[branch]
		why, failed := l.Errors[branch]
		who, used := inUse[branch]
		switch {
		case failed:
			c.Outcome, c.Reason = "unread", why
		case !verdict: // gwt found no such branch
			continue
		case !merged:
			c.Outcome = "unmerged"
		case used:
			c.Outcome, c.Reason = "left", who+" still works on it"
		default:
			if err := d.Progress.Remove(m.Dir, branch); err != nil {
				c.Outcome, c.Reason = "left", firstLine(err.Error())
			} else {
				c.Outcome = "removed"
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return none, true
	}
	return out, true
}

// branchesInUse names the branch each Claude session running in Rex is on,
// whichever driver drives it or none, to who runs it: no cleanup takes a
// merged branch from under a session still standing in it.
func (d *Driver) branchesInUse() map[string]string {
	out := map[string]string{}
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return out
	}
	for _, bl := range blocks {
		s, err := session.Locate(d.Term, d.Host, bl.ID)
		if err != nil || s.ID == "" {
			continue
		}
		if on := d.branchesOf(s.ID); len(on) > 0 {
			out[on[len(on)-1]] = cmp.Or(bl.Label, rex.Short(bl.ID))
		}
	}
	return out
}

// leftoverLines lists what closed sessions of this driver's batches left, from
// the record alone: what stays and why, then how many sessions nothing has
// judged yet. It costs reading logs, so status can afford it.
func (d *Driver) leftoverLines(b *batch.Batch) []string {
	var lines []string
	unchecked := 0
	for _, o := range d.drivenBatches(b) {
		evs, err := o.Events()
		if err != nil {
			continue
		}
		done := batch.Cleanups(evs)
		for _, c := range done {
			where := c.Session
			if o.Meta.Name != b.Meta.Name {
				where += " (batch " + o.Meta.Name + ")"
			}
			switch c.Outcome {
			case "unmerged":
				lines = append(lines, fmt.Sprintf("left by %s: branch %s stays: it is not merged, and gwt remove refuses an unmerged branch", where, c.Branch))
			case "left":
				lines = append(lines, fmt.Sprintf("left by %s: branch %s stays: %s", where, c.Branch, c.Reason))
			case "unread":
				lines = append(lines, fmt.Sprintf("what %s left could not be checked: %s; to check it again: %s", where, c.Reason, d.runIn(b.Meta.Name, "cleanup")))
			}
		}
		for _, m := range batch.Members(evs) {
			if m.Closed && !slices.ContainsFunc(done, func(c batch.Cleanup) bool { return c.Session == m.Label }) {
				unchecked++
			}
		}
	}
	if unchecked > 0 {
		lines = append(lines, fmt.Sprintf("%d closed sessions not yet checked for merged branches and worktrees; removes what merged, and says plainly what stays: %s", unchecked, d.runIn(b.Meta.Name, "cleanup")))
	}
	return lines
}
