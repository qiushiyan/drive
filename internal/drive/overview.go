package drive

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
)

// Overview is the batch as its owner reads it on the page drive page serves:
// what each session sets out to do, where it stands, the pull requests it
// opened or named and their state, and what waits on the owner. What was
// read live and what the record holds stay apart, so a park says when the
// driver parked the session and why, beside what has happened since.
type Overview struct {
	Batch  *batch.Meta // nil when there is no current batch
	Zone   *time.Location
	At     time.Time     // when the reads began
	Took   time.Duration // how long they took
	Live   string        // why the sessions were not read live; "" when they were
	Wakes  bool          // the batch wakes its driver, so a session with no wake armed goes unheard
	Asks   []*batch.Ask  // questions put to the owner with no answer yet
	Groups []Group       // the sessions, by where each stands, in the order the page shows them
	Holds  []string
}

// Group is sessions that stand alike. Closed is a group of its own, apart
// from any claim that a session's work is done, which no record makes.
type Group struct {
	Title string
	Cards []Card
}

// The groups, in the order the page shows them.
const (
	GroupPrompt  = "Waiting on a prompt"
	GroupNext    = "Waiting for its next step"
	GroupParked  = "Parked"
	GroupWorking = "Working"
	GroupUnread  = "Not read live"
	GroupClosed  = "Closed"
)

var groupOrder = []string{GroupPrompt, GroupNext, GroupParked, GroupWorking, GroupUnread, GroupClosed}

// Card is one session of the batch.
type Card struct {
	Member  *batch.Member
	Row     *Row // as read live; nil for a closed session, or one not read live
	Stage   string
	StageAt time.Time
	PRs     []PRView
	Asks    []string // the open asks that name it
	Unread  string   // what of it could not be read
}

// PRView is a pull request a session opened or named, with its state as read.
type PRView struct {
	progress.PRRef
	PR  *progress.PR // nil when it could not be read
	Err string
}

// Overview reads the current batch, or the one --batch pins, for its owner.
func (d *Driver) Overview(live bool) (*Overview, error) {
	b, err := d.Batch()
	if errors.Is(err, batch.ErrNoBatch) {
		return &Overview{Zone: time.Local, At: d.now()}, nil
	}
	if err != nil {
		return nil, err
	}
	return d.overviewOf(b, live)
}

// overviewOf reads b's record once, then, when live is set, every open
// session, the stage and pull requests of every closed one, and the state
// of every pull request named, each once. A live read that fails leaves
// what the record holds and says what could not be read; without live,
// nothing but the record is read.
func (d *Driver) overviewOf(b *batch.Batch, live bool) (*Overview, error) {
	ov := &Overview{Batch: &b.Meta, Zone: zoneOf(b), At: d.now(), Wakes: b.Meta.Wake == "webhook"}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	members := batch.Members(evs)
	_, asks, _ := batch.Records(evs)
	for _, a := range asks {
		if a.Answer == nil {
			ov.Asks = append(ov.Asks, a)
		}
	}
	rows := map[string]*Row{}
	if live {
		// A Rex session that ended, as on a server restart, leaves its
		// sessions to read as gone.
		switch _, err := d.Term.Blocks(b.Meta.RexSession); {
		case errors.Is(err, rex.ErrNoServer):
			ov.Live = "the sessions could not be read: Rex's server is not running"
		case err != nil && !errors.Is(err, rex.ErrNoSession):
			ov.Live = "the sessions could not be read: " + firstLine(err.Error())
		default:
			holds := d.holdsNow(evs, members)
			ov.Holds = holdLines(holds, ov.Zone)
			read := d.memberRows(b, members, nil, holds, ov.Zone, true)
			for i := range read {
				rows[read[i].Label] = &read[i]
			}
		}
	}

	cards := make([]Card, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		c := Card{Member: m, Row: rows[m.Label]}
		for _, a := range ov.Asks {
			if a.Session == m.Label {
				c.Asks = append(c.Asks, a.ID)
			}
		}
		if r := c.Row; r != nil {
			c.Stage, c.StageAt, c.PRs = r.Stage, r.StageAt, views(r.PRs)
			switch {
			case r.Error != "":
				c.Unread = r.Error
			case r.StepsErr != "":
				c.Unread = "its stage and pull requests (" + r.StepsErr + ")"
			}
		}
		cards[i] = c
		if m.Closed && live && d.Progress != nil && m.SID != "" {
			// A closed session's transcript outlives it, and with it what it did.
			wg.Go(func() {
				if s, err := d.Progress.Steps(m.SID); err != nil {
					cards[i].Unread = "its stage and pull requests (" + firstLine(err.Error()) + ")"
				} else {
					cards[i].Stage, cards[i].StageAt, cards[i].PRs = s.Stage, s.StageAt, views(s.PRs)
				}
			})
		}
	}
	wg.Wait()
	if live {
		var refs []progress.PRRef
		for _, c := range cards {
			for _, p := range c.PRs {
				refs = append(refs, p.PRRef)
			}
		}
		states := d.prStates(refs)
		for i := range cards {
			for j, p := range cards[i].PRs {
				if s, ok := states[keyOf(p.PRRef)]; ok {
					cards[i].PRs[j].PR, cards[i].PRs[j].Err = s.read()
				}
			}
		}
	}
	ov.Groups = grouped(cards)
	ov.Took = d.now().Sub(ov.At)
	return ov, nil
}

func views(refs []progress.PRRef) []PRView {
	var out []PRView
	for _, ref := range refs {
		out = append(out, PRView{PRRef: ref})
	}
	return out
}

// grouped puts each card in the group of where it stands: open sessions in
// the order they joined, closed ones newest first.
func grouped(cards []Card) []Group {
	by := map[string][]Card{}
	for _, c := range cards {
		by[groupOf(c)] = append(by[groupOf(c)], c)
	}
	slices.SortStableFunc(by[GroupClosed], func(a, b Card) int { return b.Member.ClosedAt.Compare(a.Member.ClosedAt) })
	var out []Group
	for _, title := range groupOrder {
		if len(by[title]) > 0 {
			out = append(out, Group{Title: title, Cards: by[title]})
		}
	}
	return out
}

// groupOf is where a card stands. A park is the record's, so it holds
// whether or not the session was read live.
func groupOf(c Card) string {
	r := c.Row
	read := r != nil && r.Error == ""
	switch {
	case c.Member.Closed:
		return GroupClosed
	case read && r.State.onPrompt():
		return GroupPrompt
	case c.Member.Parked != "":
		return GroupParked
	case !read:
		return GroupUnread
	case r.State == "working":
		return GroupWorking
	}
	return GroupNext
}
