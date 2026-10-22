package turn

// queue is what Claude Code holds back while a turn runs, as its
// queue-operation rows record it: lines typed meanwhile, task notifications,
// other sessions' messages. Its log is incomplete (a task notification can
// be removed under text it was never enqueued with), so the model keeps to
// what a reader needs: a typed line waits until it is taken, popped back
// into the input box, or lost with the process, and never longer.
type queue struct {
	items []waiting
	// taking counts items that left to start or join a turn whose row has
	// not come yet.
	taking int
}

// waiting is one queued item.
type waiting struct {
	Prompt
	content *string // as enqueued; nil when Claude Code recorded none
	prompt  bool    // someone typed it: a task notification is not a prompt
}

func (q *queue) enqueue(e row) {
	// A slash command typed while busy is enqueued without its text.
	w := waiting{Prompt: Prompt{ID: "queued:" + e.Timestamp, At: e.at(), Queued: true}, prompt: true}
	if c, ok := e.text(); ok {
		w.content, w.Text, w.Known, w.prompt = &c, c, true, typed(c)
	}
	q.items = append(q.items, w)
}

// dequeue takes the item the next turn starts on: the oldest typed line,
// which Claude Code runs before task notifications queued ahead of it, else
// the oldest item.
func (q *queue) dequeue() {
	i := q.index(func(w waiting) bool { return w.prompt })
	if i < 0 && len(q.items) > 0 {
		i = 0
	}
	q.drop(i)
	q.taking++
}

// remove takes out the item a running turn absorbed, or Claude Code
// delivered to a subagent, by its text.
func (q *queue) remove(content string, absorbed bool) {
	q.drop(q.index(func(w waiting) bool { return w.content != nil && *w.content == content }))
	if absorbed {
		q.taking++ // its row, a queued_command attachment, follows
	}
}

// popAll takes every typed line back into the input box, as Escape does
// while lines wait, or Up to edit them; task notifications stay.
func (q *queue) popAll() {
	q.keep(func(w waiting) bool { return !w.prompt })
}

// started counts off a row a queued item started or joined a turn with.
func (q *queue) started() bool {
	if q.taking == 0 {
		return false
	}
	q.taking--
	return true
}

// taken is a prompt the session took. One that nothing left the queue for
// was typed while the session was idle, when nothing waits: Claude Code
// keeps its queue in memory, so what a process that exited held is gone.
func (q *queue) taken() {
	if !q.started() {
		q.items = nil
	}
}

// newest is the newest typed line still waiting.
func (q *queue) newest() *Prompt {
	for i := len(q.items) - 1; i >= 0; i-- {
		if q.items[i].prompt {
			p := q.items[i].Prompt
			return &p
		}
	}
	return nil
}

func (q *queue) index(match func(waiting) bool) int {
	for i, w := range q.items {
		if match(w) {
			return i
		}
	}
	return -1
}

func (q *queue) drop(i int) {
	if i >= 0 {
		q.items = append(q.items[:i:i], q.items[i+1:]...)
	}
}

func (q *queue) keep(match func(waiting) bool) {
	var kept []waiting
	for _, w := range q.items {
		if match(w) {
			kept = append(kept, w)
		}
	}
	q.items = kept
}
