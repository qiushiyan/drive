package drive

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Sampler is the machine's sampler, which records what the machine spends
// its CPU and memory on while batches run: a process that outlives the
// command that started it, as a watch does.
type Sampler interface {
	// Running says whether the process Start ran still runs.
	Running() bool
	Start(argv []string) error
	// Stop ends the process Start ran and returns once it has exited, so a
	// sampler started next never finds the last one still running.
	Stop() error
}

// KeepSampler runs the configured sampler while any batch is live,
// whichever driver's, and stops it once none is. Every command calls it
// once it has run, so a batch going live starts it, the last one ending
// stops it, and one that died runs again after the next command, the
// watchdog's every five minutes among them. It decides under the lock
// batches start and end under, so a batch going live meanwhile is never
// left without it. Nothing of the batches reaches the sampler: a reader
// lines its samples up with the batch logs by time.
func (d *Driver) KeepSampler() error {
	if d.Sampler == nil || (len(d.Cfg.Sampler) == 0 && !d.Sampler.Running()) {
		return nil
	}
	return d.Store.WithLive(func(live []string) error {
		want := len(live) > 0 && len(d.Cfg.Sampler) > 0
		switch running := d.Sampler.Running(); {
		case want && !running:
			return d.Sampler.Start(d.Cfg.Sampler)
		case !want && running:
			return d.Sampler.Stop()
		}
		return nil
	})
}

// DetachedSampler runs the sampler as Detached runs a watch: in a session of
// its own, under no terminal, named by a pidfile. What it prints goes to
// Log, beside a line for each start and stop, since its failures fail no
// command.
type DetachedSampler struct {
	Pidfile, Log string
	Dir          string        // where it runs, a directory that outlives any worktree
	Grace        time.Duration // how long Stop waits after SIGTERM before SIGKILL; 10 s when zero
}

func (s DetachedSampler) Running() bool {
	p, ok := readPidfile(s.Pidfile)
	return ok && p.running()
}

func (s DetachedSampler) Start(argv []string) error {
	if err := os.MkdirAll(filepath.Dir(s.Pidfile), 0o755); err != nil {
		return err
	}
	// The sampler is no watch, though a watch's own command may start it.
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "DRIVE_WATCH=") })
	p, err := detach(argv, env, s.Dir, s.Log)
	if err != nil {
		s.note("%s did not start: %v", strings.Join(argv, " "), err)
		return fmt.Errorf("starting the sampler: %w", err)
	}
	if err := writePidfile(s.Pidfile, p); err != nil {
		// Off the books, no batch ending would stop it.
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		s.note("stopped pid %d at once, as its pidfile could not be written: %v", p.pid, err)
		return err
	}
	s.note("started pid %d: %s", p.pid, p.command)
	return nil
}

// Stop signals the sampler's process group, as a watch's is, and keeps it on
// the books until it has exited, so a stop that fails is tried again.
func (s DetachedSampler) Stop() error {
	p, ok := readPidfile(s.Pidfile)
	if !ok {
		return nil
	}
	if p.running() {
		if err := s.end(p); err != nil {
			s.note("pid %d did not stop: %v", p.pid, err)
			return err
		}
		s.note("stopped pid %d", p.pid)
	}
	if err := os.Remove(s.Pidfile); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s DetachedSampler) end(p proc) error {
	grace := cmp.Or(s.Grace, 10*time.Second)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(-p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stopping the sampler (pid %d): %w", p.pid, err)
		}
		for deadline := time.Now().Add(grace); p.running(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				break
			}
		}
		if !p.running() {
			return nil
		}
	}
	return fmt.Errorf("the sampler (pid %d) still runs after SIGKILL", p.pid)
}

// note adds a dated line to the log.
func (s DetachedSampler) note(format string, args ...any) {
	f, err := os.OpenFile(s.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s drive: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
