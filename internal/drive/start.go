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
	"github.com/qiushiyan/drive/internal/rex"
)

// Started reports a batch that started.
type Started struct {
	Batch      *batch.Batch
	RexStarted bool         // the Rex session its sessions open in was started for it
	Prev       *batch.Batch // the batch of the same driver's it replaced
	Stopped    int          // that batch's watches stopped
	StopErr    error        // why the rest of them were not
	NoWebhook  *noWebhook   // its driver's webhook, which holds no URL yet
	ThenArm    string       // what arms its wakes once the webhook is written
}

var webhookName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BatchOptions is a batch as `drive batch new` names it. A zone or Rex
// session it leaves out is the configuration's.
type BatchOptions struct {
	Name       string
	Wake       string // webhook or none
	Webhook    string // the chat driver's webhook file, for a batch that wakes one
	Driver     string // the driver that starts it, as --driver named it
	RexSession string
	TZ         string
	AfterWake  []string
}

// batchCommand is the command that starts the batch opt names, for a
// refusal to repeat. It acts in no batch, so it names none.
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
	if opt.Driver != "" && opt.Driver != opt.Webhook {
		words = append([]any{"--driver", opt.Driver}, words...)
	}
	return d.runIn("", words...)
}

// StartBatch starts a batch and makes it its driver's current one. A batch
// belongs to one driver for good: the one its webhook wakes, or the one
// --driver names for a driver that waits itself. It replaces only that
// driver's batch, whose watches it stops, since they would wake the driver
// about sessions it now drives in this one; another driver's batch is never
// touched. A webhook that holds no URL yet does not stop the start, since
// the driver is known by its name: the owner writes it, and each wake that
// cannot start until then says so. The Rex session its sessions open in runs
// once it starts.
func (d *Driver) StartBatch(opt BatchOptions) (*Started, error) {
	again := d.batchCommand(opt)
	if taken, err := d.Store.Get(opt.Name); err == nil {
		if owner, mine := batch.DriverOf(taken.Meta), cmp.Or(opt.Webhook, opt.Driver); owner != mine {
			// Batch names are shared by every driver here, so one that names
			// itself keeps clear of the others' names.
			r := &Refused{Reason: fmt.Sprintf("batch %s is %s; every driver here shares one set of batch names", opt.Name, driverWords(owner)),
				Next: "choose another name in: " + again}
			if mine != "" {
				named := opt
				named.Name = mine + "-" + opt.Name
				r.Next = "name yours for you: " + d.batchCommand(named)
			}
			return nil, r
		}
	}
	if err := d.Store.Free(opt.Name); err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if err := d.watchNamesFree(opt.Name); err != nil {
		return nil, err
	}
	m := batch.Meta{Name: opt.Name, Started: d.now(), Wake: opt.Wake, Webhook: opt.Webhook, Driver: opt.Driver,
		RexSession: cmp.Or(opt.RexSession, d.Cfg.RexSession), TZ: cmp.Or(opt.TZ, d.Cfg.TZ), AfterWake: opt.AfterWake}
	out := &Started{}
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
		if m.Driver != "" && m.Driver != m.Webhook {
			return nil, &Refused{Reason: fmt.Sprintf("a chat driver is named by its webhook: --driver %s and --webhook %s name two", m.Driver, m.Webhook),
				Next: "name one driver: " + d.batchCommand(BatchOptions{Name: opt.Name, Wake: opt.Wake, Webhook: opt.Webhook, RexSession: opt.RexSession, TZ: opt.TZ, AfterWake: opt.AfterWake})}
		}
		m.Driver = m.Webhook
		if err := d.canPost(&batch.Batch{Meta: m}); err != nil {
			out.NoWebhook, _ = errors.AsType[*noWebhook](err)
		}
	}
	if m.RexSession != "" {
		// Recorded by its label: an id, as $REX_SESSION gives it, dies with
		// Rex's server, and a batch's sessions reopen under the label after
		// a restart.
		label, started, err := d.Term.EnsureSession(m.RexSession)
		switch {
		case errors.Is(err, rex.ErrNoServer):
			return nil, serverDown(again)
		case err != nil:
			return nil, &Refused{Reason: fmt.Sprintf("Rex session %s could not be started: %v", m.RexSession, err),
				Next: "try it again: " + again}
		}
		m.RexSession, out.RexStarted = label, started
	}
	b, prev, err := d.Store.New(m)
	if err != nil {
		// Free found the name unused, so this is the store failing midway.
		what := fmt.Sprintf("batch %s did not start: %v", m.Name, err)
		if out.RexStarted {
			what += fmt.Sprintf("; Rex session %s was started for it and runs", m.RexSession)
		}
		return nil, &Failed{What: what, Next: "look at " + filepath.Join(d.Store.Dir, "batches", m.Name) + ", then try it again: " + again}
	}
	d.rebind()
	out.Batch, out.Prev = b, prev
	if out.NoWebhook != nil {
		out.ThenArm = d.runIn(b.Meta.Name, "watch", "--due")
	}
	if prev != nil {
		out.Stopped, out.StopErr = d.StopWatches(prev)
	}
	return out, nil
}

// watchNamesFree refuses a batch name whose watches would be named as an
// existing batch's are, such as a.b beside a-b: stopping one batch's
// watches would stop the other's.
func (d *Driver) watchNamesFree(name string) error {
	metas, err := d.Store.List()
	if err != nil {
		return err
	}
	for _, m := range metas {
		if m.Name != name && safeName(m.Name) == safeName(name) {
			return &Refused{Reason: fmt.Sprintf("batch name %q is too like batch %s's: their watches would share names, and ending one would stop the other's; choose a name that differs in a letter or digit", name, m.Name)}
		}
	}
	return nil
}

// UseBatch makes a batch its driver's current one again. Another driver's
// name on the command refuses it, as it would any action in that batch.
func (d *Driver) UseBatch(name string) (*batch.Batch, error) {
	b, err := d.Store.Get(name)
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if driver := batch.DriverOf(b.Meta); d.DriverName != "" && driver != d.DriverName {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s is %s, not driver %s's", name, driverWords(driver), d.DriverName)}
	}
	if _, err := d.Store.Use(name, d.now()); err != nil {
		return nil, err
	}
	d.rebind()
	return b, nil
}

// Ended reports a batch that ended.
type Ended struct {
	Batch   *batch.Batch
	Stopped int
}

// EndBatch ends the batch the command acts in, once its driver's drive is
// over: its watches and timers stop, and it is no longer its driver's, so
// a command that names no driver no longer needs to name one for it.
func (d *Driver) EndBatch() (*Ended, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	if err := d.Store.End(b.Meta.Name, d.now()); err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	d.rebind()
	out := &Ended{Batch: b}
	if out.Stopped, err = d.StopWatches(b); err != nil {
		return out, &Failed{What: fmt.Sprintf("batch %s ended, and stopped %d of its watches, then: %v", b.Meta.Name, out.Stopped, err),
			Next: "look at the watches still running: ls " + filepath.Join(d.Store.Dir, "watches")}
	}
	return out, nil
}

// Batches lists every batch, with which batch each driver drives now.
func (d *Driver) Batches() ([]batch.Meta, batch.Drivers, error) {
	metas, err := d.Store.List()
	if err != nil {
		return nil, batch.Drivers{}, err
	}
	drivers, err := d.Store.Drivers()
	return metas, drivers, err
}
