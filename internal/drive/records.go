package drive

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/qiushiyan/drive/internal/batch"
)

// Grant records authority the owner handed over, in their words and with
// what it covers. It lives as long as the batch: a new batch starts with none.
func (d *Driver) Grant(source, scope string) (string, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(scope) == "" {
		return "", &Refused{Reason: "a grant needs --source (the owner's words and where they said them) and --scope (what it covers)"}
	}
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindGrant, ID: batch.NextID(evs, batch.KindGrant, "G"), Source: source, Scope: scope}
	})
}

// Ask records a question put to the owner.
func (d *Driver) Ask(session, question, recommendation, ifUnanswered string) (string, error) {
	if question == "" || ifUnanswered == "" {
		return "", &Refused{Reason: "an ask needs --question and --if-unanswered (what happens while nobody answers)"}
	}
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindAsk, ID: batch.NextID(evs, batch.KindAsk, "A"), Session: session,
			Question: question, Recommendation: recommendation, IfUnanswered: ifUnanswered}
	})
}

var tookWords = map[string]bool{"recommended": true, "amended": true, "delegated": true, "moot": true}

// Answer records the owner's answer to an ask: whether they took the
// recommendation, amended it, handed it to someone (a fresh model), or it
// stopped mattering; and separately whether they needed it explained first.
func (d *Driver) Answer(ask, source, took string, explained bool) (string, error) {
	if !tookWords[took] {
		return "", &Refused{Reason: fmt.Sprintf("--took %q: one of recommended, amended, delegated, moot", took)}
	}
	b, err := d.Batch()
	if err != nil {
		return "", &Refused{Reason: err.Error()}
	}
	err = b.Locked(func(evs []batch.Event) error {
		for _, e := range evs {
			if e.Kind == batch.KindAsk && e.ID == ask {
				return b.Append(batch.Event{At: d.now(), Kind: batch.KindAnswer, Ref: ask, Source: source, Took: took, Explained: explained})
			}
		}
		return &Refused{Reason: fmt.Sprintf("no ask %s in batch %s", ask, b.Meta.Name), Next: d.run("status") + " lists the open asks"}
	})
	return ask, err
}

// Note records something the driver wants the batch to remember.
func (d *Driver) Note(session, text string) (string, error) {
	return d.record(func(evs []batch.Event) batch.Event {
		return batch.Event{Kind: batch.KindNote, ID: batch.NextID(evs, batch.KindNote, "N"), Session: session, Note: text}
	})
}

// Goal records what a session sets out to do, in plain terms for the owner,
// replacing the goal its label had: for a session drive did not open, or
// once its purpose or scope changed. A closed session takes one too, so a
// batch's record can be completed after the fact.
func (d *Driver) Goal(session, text string) error {
	goal, err := goalText(text)
	if err != nil {
		return err
	}
	b, err := d.Batch()
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	return b.Locked(func(evs []batch.Event) error {
		m := batch.Find(batch.Members(evs), session)
		if m == nil {
			return &Refused{Reason: fmt.Sprintf("%s has never been in batch %s", session, b.Meta.Name),
				Next: d.run("status") + " lists the batch's sessions by label"}
		}
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindGoal, Session: m.Label, Goal: goal})
	})
}

// maxGoal bounds a goal: a short paragraph the owner reads on a phone.
const maxGoal = 600

// goalText is a goal as it is recorded: one paragraph, its runs of white
// space, line breaks included, made one space. An empty goal, one with a
// control character, or one past maxGoal bytes is refused.
func goalText(s string) (string, error) {
	goal := strings.Join(strings.Fields(s), " ")
	switch {
	case goal == "":
		return "", &Refused{Reason: "a goal needs words: what the session sets out to do, in plain terms"}
	case strings.ContainsFunc(goal, unicode.IsControl):
		return "", &Refused{Reason: "a goal is plain text: it has a control character"}
	case len(goal) > maxGoal:
		return "", &Refused{Reason: fmt.Sprintf("a goal is one short paragraph, at most %d bytes; this one is %d", maxGoal, len(goal))}
	}
	return goal, nil
}

func (d *Driver) record(make func([]batch.Event) batch.Event) (string, error) {
	b, err := d.Batch()
	if err != nil {
		return "", &Refused{Reason: err.Error()}
	}
	var id string
	err = b.Locked(func(evs []batch.Event) error {
		e := make(evs)
		e.At = d.now()
		id = e.ID
		return b.Append(e)
	})
	return id, err
}

// Log is the batch's events since a time.
func (d *Driver) Log(since time.Time) (*batch.Batch, []batch.Event, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, nil, &Refused{Reason: err.Error()}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, nil, err
	}
	var out []batch.Event
	for _, e := range evs {
		if !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return b, out, nil
}
