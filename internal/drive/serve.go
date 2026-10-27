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
// time and is reused for ReuseFor after it ends, since each costs a lookup
// per session. A visit while a read runs waits for what is left of that
// read's Deadline, then is answered from the record alone, so a stuck lookup
// never stalls the page; a read still running after GiveUpAfter is left to
// finish on its own, and the next visit starts another.
type Page struct {
	ReuseFor    time.Duration
	Deadline    time.Duration
	GiveUpAfter time.Duration

	d     *Driver
	mu    sync.Mutex
	reads map[string]*pageReads // by batch
}

// pageReads is a batch's newest read that ended well, and the read under
// way, if any.
type pageReads struct {
	last, running *pageRead
}

// pageRead is one read of a batch.
type pageRead struct {
	began time.Time     // on the wall clock, which the deadline counts on
	done  chan struct{} // closed when it ends
	ov    *Overview
	err   error
	ended time.Time // on the driver's clock, which reuse counts from
}

// NewPage is the page of d's batch: the current one at each visit, or the
// one --batch pins.
func NewPage(d *Driver) *Page {
	return &Page{ReuseFor: 30 * time.Second, Deadline: 20 * time.Second, GiveUpAfter: 2 * time.Minute, d: d, reads: map[string]*pageReads{}}
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
	w.Header().Set("Cache-Control", "no-store") // a reload asks again, and is answered within ReuseFor
	_, _ = w.Write(ov.HTML(p.d.now()))
}

// overview is the batch's last read if it is fresh, else what a read
// gives within its deadline, else what the record holds. The batch is
// resolved once per visit, and each read reads only that batch.
func (p *Page) overview() (*Overview, error) {
	b, err := p.d.Batch()
	if errors.Is(err, batch.ErrNoBatch) {
		return p.d.Overview(false)
	}
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	rs := p.reads[b.Meta.Name]
	if rs == nil {
		rs = &pageReads{}
		p.reads[b.Meta.Name] = rs
	}
	if l := rs.last; l != nil && p.d.now().Sub(l.ended) < p.ReuseFor {
		p.mu.Unlock()
		return l.ov, nil
	}
	r := rs.running
	if r == nil || time.Since(r.began) > p.GiveUpAfter {
		r = &pageRead{began: time.Now(), done: make(chan struct{})}
		rs.running = r
		go p.read(b, rs, r)
	}
	p.mu.Unlock()

	wait := time.NewTimer(max(p.Deadline-time.Since(r.began), 0))
	defer wait.Stop()
	select {
	case <-r.done:
		return r.ov, r.err
	case <-wait.C:
	}
	ov, err := p.d.overviewOf(b, false)
	if err != nil {
		return nil, err
	}
	ov.Live = "the sessions are still being read, since " + r.began.In(ov.Zone).Format("15:04:05")
	return ov, nil
}

// read reads b live into r. A read that ends after a newer one does not
// replace it.
func (p *Page) read(b *batch.Batch, rs *pageReads, r *pageRead) {
	ov, err := p.d.overviewOf(b, true)
	p.mu.Lock()
	r.ov, r.err, r.ended = ov, err, p.d.now()
	if rs.running == r {
		rs.running = nil
	}
	if err == nil && (rs.last == nil || r.began.After(rs.last.began)) {
		rs.last = r
	}
	p.mu.Unlock()
	close(r.done)
}
