package drive

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
)

// Started reports a batch that started.
type Started struct {
	Batch      *batch.Batch
	RexStarted bool         // the Rex session its sessions open in was started for it
	Prev       *batch.Batch // the batch that was current
	Stopped    int          // that batch's watches stopped
}

var webhookName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BatchOptions is a batch as `drive batch new` names it. A zone or Rex
// session it leaves out is the configuration's.
type BatchOptions struct {
	Name       string
	Wake       string // webhook or none
	Webhook    string // the chat driver's webhook file, for a batch that wakes one
	RexSession string
	TZ         string
	AfterWake  []string
}

// batchCommand is the command that starts the batch opt names, for a
// refusal to repeat.
func (d *Driver) batchCommand(opt BatchOptions) string {
	words := []any{"batch", "new", opt.Name}
	for _, f := range [][2]string{{"--webhook", opt.Webhook}, {"--rex-session", opt.RexSession}, {"--tz", opt.TZ}} {
		if f[1] != "" {
			words = append(words, f[0], f[1])
		}
	}
	for _, c := range opt.AfterWake {
		words = append(words, "--after-wake", c)
	}
	return d.run(words...)
}

// StartBatch starts a batch and makes it current. A batch that wakes a chat
// driver names that driver's webhook, which must hold a URL before the batch
// starts, since every wake of it would otherwise fail unseen; the Rex
// session its sessions open in runs once it starts; the batch before it
// stops its watches, which would wake the driver about sessions it now drives
// in this one.
func (d *Driver) StartBatch(opt BatchOptions) (*Started, error) {
	again := d.batchCommand(opt)
	if err := d.Store.Free(opt.Name); err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	m := batch.Meta{Name: opt.Name, Started: d.now(), Wake: opt.Wake, Webhook: opt.Webhook,
		RexSession: cmp.Or(opt.RexSession, d.Cfg.RexSession), TZ: cmp.Or(opt.TZ, d.Cfg.TZ), AfterWake: opt.AfterWake}
	if m.Wake == "webhook" {
		dir := filepath.Join(d.Cfg.ConfigDir, "webhooks")
		switch base := filepath.Base(m.Webhook); {
		case m.Webhook == "":
			return nil, &Refused{Reason: "a batch that wakes a chat driver names that driver's webhook",
				Next: fmt.Sprintf("add --webhook and the name of your own wake routine's file in %s to: %s", dir, again)}
		case webhookName.MatchString(m.Webhook):
		case filepath.Dir(config.Expand(d.Cfg.Home, m.Webhook)) == dir && webhookName.MatchString(base):
			return nil, &Refused{Reason: fmt.Sprintf("--webhook takes the file's name in %s, not its path", dir),
				Next: strings.Replace(again, m.Webhook, base, 1)}
		default:
			return nil, &Refused{Reason: fmt.Sprintf("--webhook %q is not the name of a file in %s", m.Webhook, dir),
				Next: fmt.Sprintf("name your own wake routine's file there instead, which the owner writes, in: %s", again)}
		}
		if err := d.canPost(&batch.Batch{Meta: m}); err != nil {
			var nw *noWebhook
			errors.As(err, &nw)
			return nil, &Refused{Reason: err.Error(), Next: nw.next(again)}
		}
	}
	out := &Started{}
	if m.RexSession != "" {
		started, err := d.Term.EnsureSession(m.RexSession)
		if err != nil {
			return nil, &Refused{Reason: fmt.Sprintf("Rex session %s could not be started: %v", m.RexSession, err),
				Next: "try it again: " + again}
		}
		out.RexStarted = started
	}
	// The batch that stops is the current one, whichever batch this run
	// was pointed at.
	out.Prev, _ = d.Store.Current()
	b, err := d.Store.New(m)
	if err != nil {
		// Free found the name unused, so this is the store failing midway.
		what := fmt.Sprintf("batch %s did not start: %v", m.Name, err)
		if out.RexStarted {
			what += fmt.Sprintf("; Rex session %s was started for it and runs", m.RexSession)
		}
		return nil, &Failed{What: what, Next: "look at " + filepath.Join(d.Store.Dir, "batches", m.Name) + ", then try it again: " + again}
	}
	out.Batch = b
	if out.Prev != nil {
		out.Stopped, _ = d.StopWatches(out.Prev)
	}
	return out, nil
}

// UseBatch makes a batch current.
func (d *Driver) UseBatch(name string) error {
	if err := d.Store.Use(name); err != nil {
		return &Refused{Reason: err.Error()}
	}
	return nil
}

// Batches lists every batch, and names the current one.
func (d *Driver) Batches() (metas []batch.Meta, current string, err error) {
	if metas, err = d.Store.List(); err != nil {
		return nil, "", err
	}
	if cur, _ := d.Store.Current(); cur != nil {
		current = cur.Meta.Name
	}
	return metas, current, nil
}
