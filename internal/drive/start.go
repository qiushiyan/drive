package drive

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/qiushiyan/drive/internal/batch"
)

// Started reports a batch that started.
type Started struct {
	Batch      *batch.Batch
	RexStarted bool         // the Rex session its sessions open in was started for it
	Prev       *batch.Batch // the batch that was current
	Stopped    int          // that batch's watches stopped
}

var webhookName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// StartBatch starts a batch and makes it current. A batch that wakes a chat
// driver names that driver's webhook, which must hold a URL before the batch
// starts, since every wake of it would otherwise fail unseen; the Rex
// session its sessions open in runs once it starts; the batch before it
// stops its watches, which would wake the driver about sessions it now drives
// in this one. again is the command that starts it, for a refusal to repeat.
func (d *Driver) StartBatch(m batch.Meta, again string) (*Started, error) {
	if m.Wake == "webhook" {
		if !webhookName.MatchString(m.Webhook) {
			return nil, &Refused{Reason: "a batch that wakes a chat driver names that driver's webhook",
				Next: fmt.Sprintf("add --webhook with the name of your own wake routine's file under %s, which holds its URL= and AUTH= lines, to: %s",
					filepath.Join(d.Cfg.ConfigDir, "webhooks"), again)}
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
	out.Prev, _ = d.Batch()
	b, err := d.Store.New(m)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	out.Batch = b
	if out.Prev != nil {
		out.Stopped, _ = d.StopWatches(out.Prev)
	}
	return out, nil
}
