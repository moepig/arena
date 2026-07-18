package controller

import "sync"

// workQueue is a deduplicating work queue with the kubernetes workqueue
// semantics: an item added while being processed is re-queued when Done is
// called, and an item is never handed to two workers at once. This is what
// serializes all processing for one fleet while fleets stay parallel.
type workQueue struct {
	mu   sync.Mutex
	cond *sync.Cond

	order      []string
	queued     map[string]bool // in order, waiting for a worker
	processing map[string]bool
	dirty      map[string]bool // re-add arrived while processing
	shutdown   bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{
		queued:     map[string]bool{},
		processing: map[string]bool{},
		dirty:      map[string]bool{},
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add enqueues an item, deduplicating against queued and in-flight work.
func (q *workQueue) Add(item string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown || q.queued[item] {
		return
	}
	if q.processing[item] {
		q.dirty[item] = true
		return
	}
	q.queued[item] = true
	q.order = append(q.order, item)
	q.cond.Signal()
}

// Get blocks until an item is available (ok=true) or the queue shuts down
// (ok=false). The caller must call Done(item) afterwards.
func (q *workQueue) Get() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if len(q.order) == 0 {
		return "", false
	}
	item := q.order[0]
	q.order = q.order[1:]
	delete(q.queued, item)
	q.processing[item] = true
	return item, true
}

// Done releases an item; if it was re-added during processing it goes back
// on the queue.
func (q *workQueue) Done(item string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, item)
	if q.dirty[item] && !q.shutdown {
		delete(q.dirty, item)
		q.queued[item] = true
		q.order = append(q.order, item)
		q.cond.Signal()
	}
}

// ShutDown wakes all waiting workers with ok=false.
func (q *workQueue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	q.cond.Broadcast()
}
