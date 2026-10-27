package drive

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
)

// Page serves the batch's overview as a web page its owner opens when they
// choose, read on demand and never pushed. One read of a batch runs at a
// time and is reused for ReuseFor, since each costs a lookup per session; a
// read that takes longer than Deadline is answered from the record alone
// while it finishes, so one stuck lookup never stalls the page.
type Page struct {
	ReuseFor time.Duration
	Deadline time.Duration

	d       *Driver
	mu      sync.Mutex
	last    map[string]*Overview     // each batch's newest read
	failed  map[string]string        // why each batch's newest read failed
	reading map[string]chan struct{} // a read under way, closed when it ends
	since   map[string]time.Time     // when that read began
}

// NewPage is the page of d's batch: the current one at each request, or
// the one --batch pins.
func NewPage(d *Driver) *Page {
	return &Page{ReuseFor: 30 * time.Second, Deadline: 20 * time.Second, d: d,
		last: map[string]*Overview{}, failed: map[string]string{}, reading: map[string]chan struct{}{}, since: map[string]time.Time{}}
}

func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the page only reads", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ov, err := p.overview()
	if err != nil {
		http.Error(w, "the batch could not be read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a reload reads again, within ReuseFor
	_, _ = w.Write(ov.HTML(p.d.now()))
}

// overview is the batch's newest read if it is fresh, else a new one; one
// that outlasts the deadline, or fails, leaves what the record holds. The
// batch is resolved once, and every read of it reads that batch, so a page
// is never two batches spliced.
func (p *Page) overview() (*Overview, error) {
	b, err := p.d.Batch()
	if errors.Is(err, batch.ErrNoBatch) {
		return p.d.Overview(false)
	}
	if err != nil {
		return nil, err
	}
	name := b.Meta.Name
	p.mu.Lock()
	if ov := p.last[name]; ov != nil && p.d.now().Sub(ov.At) < p.ReuseFor {
		p.mu.Unlock()
		return ov, nil
	}
	done, ok := p.reading[name]
	if !ok {
		done = make(chan struct{})
		p.reading[name], p.since[name] = done, p.d.now()
		go func() {
			ov, err := p.d.overviewOf(b, true)
			p.mu.Lock()
			if err != nil {
				p.failed[name] = err.Error()
			} else {
				p.last[name] = ov
				delete(p.failed, name)
			}
			delete(p.reading, name)
			p.mu.Unlock()
			close(done)
		}()
	}
	began := p.since[name]
	p.mu.Unlock()

	why := ""
	select {
	case <-done:
		p.mu.Lock()
		ov, failed := p.last[name], p.failed[name]
		p.mu.Unlock()
		if failed == "" && ov != nil {
			return ov, nil
		}
		why = "the read failed: " + failed
	case <-time.After(p.Deadline):
		why = "the sessions are still being read, since " + began.In(zoneOf(b)).Format("15:04:05") + "; reload in a moment"
	}
	ov, err := p.d.overviewOf(b, false)
	if err != nil {
		return nil, err
	}
	ov.Live = why
	return ov, nil
}
