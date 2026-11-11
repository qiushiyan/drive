package drive

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
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

// Ask records a question put to the owner. One that replaces an open ask
// records that ask moot, replaced by this one, in the same write, so the
// owner is never left two questions where one stands.
func (d *Driver) Ask(session, question, recommendation, ifUnanswered, replaces string) (string, error) {
	if question == "" || ifUnanswered == "" {
		return "", &Refused{Reason: "an ask needs --question and --if-unanswered (what happens while nobody answers)"}
	}
	b, err := d.Batch()
	if err != nil {
		return "", err
	}
	var id string
	err = b.Locked(func(evs []batch.Event) error {
		id = batch.NextID(evs, batch.KindAsk, "A")
		ask := batch.Event{At: d.now(), Kind: batch.KindAsk, ID: id, Session: session, Ref: replaces,
			Question: question, Recommendation: recommendation, IfUnanswered: ifUnanswered}
		if replaces == "" {
			return b.Append(ask)
		}
		if refused := d.stillOpen(b, evs, replaces); refused != nil {
			refused.Next = "record it without --replaces, or name the open ask it replaces; " + d.run("status") + " lists the open asks"
			return refused
		}
		return b.Append(ask, batch.Event{At: d.now(), Kind: batch.KindAnswer, Ref: replaces, Took: "moot", Source: "replaced by " + id})
	})
	return id, err
}

// Restate records what an open ask waits on now, once its session closed or
// its change merged and the ask still holds something, such as a production
// write or a message: status and the owner's page then read it as standing
// again from now, in these words.
func (d *Driver) Restate(ask, ifUnanswered string) error {
	if strings.TrimSpace(ifUnanswered) == "" {
		return &Refused{Reason: "a restate needs --if-unanswered: what the ask waits on now, as the owner reads it"}
	}
	b, err := d.Batch()
	if err != nil {
		return err
	}
	return b.Locked(func(evs []batch.Event) error {
		if refused := d.stillOpen(b, evs, ask); refused != nil {
			return refused
		}
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindRestate, Ref: ask, IfUnanswered: ifUnanswered})
	})
}

// stillOpen refuses a record about an ask that is not open: one never asked,
// or one already answered, which waits on nothing and is replaced by nothing.
func (d *Driver) stillOpen(b *batch.Batch, evs []batch.Event, ask string) *Refused {
	_, asks, _ := batch.Records(evs)
	for _, a := range asks {
		if a.ID != ask {
			continue
		}
		if a.Answer == nil {
			return nil
		}
		at := a.Answer.At.In(b.Zone()).Format("15:04")
		if a.Answer.Took == "moot" {
			return &Refused{Reason: fmt.Sprintf("%s was recorded moot at %s (%s), so it is no longer open", ask, at, clip(a.Answer.Source, 120))}
		}
		return &Refused{Reason: fmt.Sprintf("%s was answered at %s (%s), so it is no longer open", ask, at, a.Answer.Took)}
	}
	return &Refused{Reason: fmt.Sprintf("no ask %s in batch %s", ask, b.Meta.Name), Next: d.run("status") + " lists the open asks"}
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
		return "", err
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

// AnswerAndSend records the owner's answer to an ask and sends the session the
// ask is about the step that answer leads to: the one command it takes, the
// session's current view read for it. The answer goes on record with the step's
// intent, in one write under the batch's lock, so no command cut short can leave
// a step sent with its ask still open, and no second run can send it again.
func (d *Driver) AnswerAndSend(ask, source, took string, explained bool, step Step, opt SendOptions) (*Sent, string, error) {
	if !tookWords[took] {
		return nil, "", &Refused{Reason: fmt.Sprintf("--took %q: one of recommended, amended, delegated, moot", took)}
	}
	b, evs, members, err := d.members()
	if err != nil {
		return nil, "", err
	}
	if b == nil {
		return nil, "", &Refused{Reason: batch.ErrNoBatch.Error(), cause: batch.ErrNoBatch}
	}
	a, refused := d.openAsk(b, evs, ask)
	if refused != nil {
		return nil, "", refused
	}
	answer := &AskAnswer{Ask: ask, Source: source, Took: took, Explained: explained}
	if a.Session == "" {
		return nil, "", &Refused{Reason: ask + " is about the batch, so it names no session to send a step to",
			Next: "record the answer alone (" + d.answerCommand(answer) + "), then send the step to the session it is for"}
	}
	o, err := d.observe(b, members, a.Session)
	if err != nil {
		return nil, "", d.unanswered(err, answer)
	}
	opt.Answer = answer
	sent, err := d.Send(o.View.String(), step, opt)
	evs, rerr := b.Events()
	if rerr != nil {
		return sent, "", err
	}
	_, asks, _ := batch.Records(evs)
	answered := slices.ContainsFunc(asks, func(x *batch.Ask) bool { return x.ID == ask && x.Answer != nil })
	if !answered {
		return sent, "", d.unanswered(err, answer)
	}
	if r, ok := errors.AsType[*Refused](err); ok {
		// Admitted and then refused, so the answer stands though the step did not go.
		return sent, ask, &Failed{What: fmt.Sprintf("recorded the answer to %s, but the step did not go: %s", ask, r.Reason), Next: r.Next}
	}
	return sent, ask, err
}

// unanswered is a refusal of an answer's step, before the answer went on
// record with it: whatever the driver runs instead, a send its next gives
// included, sends the step alone, so it says how to record the answer too.
func (d *Driver) unanswered(err error, a *AskAnswer) error {
	r, ok := errors.AsType[*Refused](err)
	if !ok {
		return err
	}
	out := *r
	out.Reason = fmt.Sprintf("%s; so the answer to %s is not recorded either", r.Reason, a.Ask)
	record := "record the answer to " + a.Ask + " as well, since a step sent another way goes alone: " + d.answerCommand(a)
	if r.Next == "" {
		record = "record the answer to " + a.Ask + " alone: " + d.answerCommand(a)
	}
	out.Next = strings.TrimPrefix(r.Next+"; "+record, "; ")
	return &out
}

// answerCommand is the drive record answer that records a, with no step.
func (d *Driver) answerCommand(a *AskAnswer) string {
	words := []any{"record", "answer", a.Ask, "--took", a.Took}
	if a.Source != "" {
		words = append(words, "--source="+a.Source) // one word, so words starting with a dash stay the flag's value
	}
	if a.Explained {
		words = append(words, "--explained")
	}
	return d.run(words...)
}

// openAsk finds an ask no one has answered, or refuses saying why it cannot be
// answered.
func (d *Driver) openAsk(b *batch.Batch, evs []batch.Event, ask string) (*batch.Ask, *Refused) {
	_, asks, _ := batch.Records(evs)
	for _, a := range asks {
		if a.ID != ask {
			continue
		}
		if a.Answer != nil {
			r := &Refused{Reason: fmt.Sprintf("%s was answered at %s, and %s", ask, a.Answer.At.In(b.Zone()).Format("15:04"), stepOfAnswer(evs, a.Answer)),
				Next: "read the session before sending anything more: " + d.run("read", cmp.Or(a.Session, "SESSION"))}
			if a.Answer.Intent != "" && a.Session != "" {
				r.Next = "see whether the step arrived, and send it again only if it did not: " + d.run("read", a.Session, "--user")
			}
			return nil, r
		}
		return a, nil
	}
	return nil, &Refused{Reason: fmt.Sprintf("no ask %s in batch %s", ask, b.Meta.Name), Next: d.run("status") + " lists the open asks"}
}

// stepOfAnswer says what the record knows of the step an answer went with, which
// is whether it went only once its send was settled.
func stepOfAnswer(evs []batch.Event, answer *batch.Event) string {
	if answer.Intent == "" {
		return "no step went with it"
	}
	for _, e := range evs {
		if e.Intent != answer.Intent {
			continue
		}
		switch e.Kind {
		case batch.KindSend:
			return "its step " + e.Outcome
		case batch.KindFailure:
			return "its step failed: " + clip(e.Reason, 160)
		}
	}
	return "nothing says yet whether its step reached the session"
}

// Seen notes that a driver read the batch, which is how a run that changes
// nothing is heard: a wake that leaves no trace reads as one no run answered.
// One note a minute is plenty.
func (d *Driver) Seen() {
	b, err := d.Batch()
	if err != nil || b.Meta.Wake != "webhook" {
		return
	}
	evs, err := b.Events()
	if err != nil {
		return
	}
	if h := batch.Hearings(evs, d.now()); !h.Heard.IsZero() && d.now().Sub(h.Heard) < time.Minute {
		return
	}
	_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindSeen})
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
		return err
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
// space, line breaks included, made one space. A goal with no words, such
// as a placeholder copied as it was printed, one with a control character,
// or one past maxGoal bytes is refused.
func goalText(s string) (string, error) {
	goal := strings.Join(strings.Fields(s), " ")
	switch {
	case !strings.ContainsFunc(goal, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }):
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
		return "", err
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
		return nil, nil, err
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

// Overtaken is what happened to an open ask's session after the ask last
// stood, as asked or restated: the session closed, or its newest pull
// request merged. Either is a reason to judge the ask again and never an
// answer, since an ask outlives both while it still holds a production write
// or a message, and only its driver knows whether the owner answered it in
// chat meanwhile.
type Overtaken struct {
	Closed   time.Time       // when its session closed; zero unless after the ask stood
	PR       *progress.PRRef // its newest pull request, when that merged after the ask stood
	MergedAt time.Time
	// Next is what a driver runs about it, as status says: record it moot,
	// or restate what it still waits on.
	Next string
}

// None says nothing overtook the ask.
func (o Overtaken) None() bool { return o.Closed.IsZero() && o.PR == nil }

// overtakenOf is what overtook a, read from its session's member and, when
// read, its newest pull request and that pull request's state.
func overtakenOf(a *batch.Ask, m *batch.Member, ref *progress.PRRef, pr *progress.PR) Overtaken {
	var o Overtaken
	stood := a.Stood()
	if m != nil && m.Closed && m.ClosedAt.After(stood) {
		o.Closed = m.ClosedAt
	}
	if ref != nil && pr != nil && pr.State == "MERGED" && pr.MergedAt.After(stood) {
		o.PR, o.MergedAt = ref, pr.MergedAt
	}
	return o
}

// rejudge is what a driver runs about an ask something overtook: an answer
// the owner gave meanwhile is theirs, one that waits on nothing now is moot,
// and one that still holds something is restated.
func (d *Driver) rejudge(ask string) string {
	return fmt.Sprintf("if the owner answered it meanwhile, record their answer; if it waits on nothing now, %s; if it still holds something, such as a production write or a message, %s",
		d.run("record", "answer", ask, "--took", "moot", "--source", "…"), d.run("record", "restate", ask, "--if-unanswered", "…"))
}

// stoodWord is how an ask came to stand as it does: asked, or restated.
func stoodWord(a *batch.Ask) string {
	if a.Restated != nil {
		return "restated"
	}
	return "asked"
}

// overtakenWords is what overtook an ask about session, for a driver or the
// owner to read: "S1 closed at 02:00 and #11154 merged at 01:30". at prints
// a time.
func overtakenWords(o Overtaken, session string, at func(time.Time) string) string {
	var parts []string
	if !o.Closed.IsZero() {
		parts = append(parts, session+" closed "+at(o.Closed))
	}
	if o.PR != nil {
		parts = append(parts, fmt.Sprintf("#%d merged %s", o.PR.Number, at(o.MergedAt)))
	}
	return strings.Join(parts, " and ")
}
