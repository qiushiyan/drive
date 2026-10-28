package drive

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// settle gives an outcome to the sends an earlier driver of this session
// admitted and never finished. The caller owns the session, so that driver
// has gone; whether its line arrived is read from the transcript. A session
// that cannot be read for now settles nothing, since a step it took and
// still runs would read as one that never reached it and free its hold; a
// session gone or closed holds nothing, and settles from what it shows.
func (d *Driver) settle(b *batch.Batch, label, block string) error {
	evs, err := b.Events()
	if err != nil {
		return err
	}
	var orphans []batch.Event
	for _, e := range batch.Unresolved(evs, label) {
		if e.Kind == batch.KindSendIntent {
			orphans = append(orphans, e)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	var f turn.Frontier
	s, err := session.Locate(d.Term, d.Host, block)
	if err == nil {
		f, err = s.Frontier()
	}
	if err != nil && !errors.Is(err, session.ErrNoClaude) && d.blockExists(block) {
		return fmt.Errorf("%s cannot be read to settle the step an earlier driver left it: %v", label, err)
	}
	for _, p := range orphans {
		e := batch.Event{At: d.now(), Session: label, Intent: p.Intent, Hold: p.Hold, View: p.View, Key: p.Key,
			Text: p.Text, Path: p.Path, Mode: p.Mode, Notice: p.Notice}
		switch outcome, instead := arrival(f, p); {
		case outcome != "":
			e.Kind, e.Outcome, e.Note = batch.KindSend, outcome, "settled: its driver ended before confirming it"
		case instead != "":
			e.Kind, e.Reason = batch.KindFailure, fmt.Sprintf("settled: its driver ended before the step reached the session, which received %q instead", clip(instead, 120))
		default:
			e.Kind, e.Reason = batch.KindFailure, "settled: its driver ended before the step reached the session"
		}
		if err := b.Append(e); err != nil {
			return err
		}
	}
	return nil
}

// arrival looks for an intent's line among the prompts the session received
// since the intent was recorded: arrived, queued, or garbled when one is the
// line; else the newest prompt that came instead, such as the owner's own
// words.
func arrival(f turn.Frontier, intent batch.Event) (outcome, instead string) {
	ps := f.Prompts
	if f.Queued != nil {
		ps = append(append([]turn.Prompt{}, ps...), *f.Queued)
	}
	for _, p := range ps {
		if p.At.Before(intent.At) {
			continue
		}
		switch got := received(intent.Text, p); got {
		case "":
		case "other":
			instead = strings.TrimSpace(p.Text)
		default:
			return got, ""
		}
	}
	return "", instead
}

// settleOpen gives an outcome to the opens of label an earlier driver
// admitted and never finished. The caller owns the label, so that driver has
// gone; a block Rex opened under the label is the one it opened.
func (d *Driver) settleOpen(b *batch.Batch, evs []batch.Event, label string) error {
	members := batch.Members(evs)
	for _, e := range batch.Unresolved(evs, label) {
		if e.Kind != batch.KindOpenIntent {
			continue
		}
		ev := batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Intent: e.Intent,
			Reason: "settled: its driver ended before Rex opened a block"}
		if blocks, err := d.Term.Blocks(b.Meta.RexSession); err == nil {
			for _, bl := range blocks {
				if bl.Label != label || batch.Find(members, bl.ID) != nil {
					continue
				}
				// Another driver's batch may have opened a block under the same label.
				if other, err := d.drivenElsewhere(b, bl.ID); err != nil || other != nil {
					continue
				}
				ev = batch.Event{At: d.now(), Kind: batch.KindOpen, Session: label, Intent: e.Intent, Block: bl.ID, Dir: e.Dir,
					Outcome: "settled", Note: "its driver ended before Claude Code started"}
				break
			}
		}
		if err := b.Append(ev); err != nil {
			return err
		}
	}
	return nil
}
