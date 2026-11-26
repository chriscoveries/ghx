package governor

import "sync"

// admissionQueue orders requests waiting for the gate without interrupting
// the active request. Each class is FIFO and M3 can overtake once between
// regular turns.
type admissionQueue struct {
	mu      sync.Mutex
	active  bool
	lastM3  bool
	m3      []chan struct{}
	regular []chan struct{}
}

func (q *admissionQueue) acquire(m3 bool) {
	q.mu.Lock()
	if !q.active {
		q.active = true
		q.lastM3 = m3
		q.mu.Unlock()
		return
	}

	ready := make(chan struct{})
	if m3 {
		q.m3 = append(q.m3, ready)
	} else {
		q.regular = append(q.regular, ready)
	}
	q.mu.Unlock()
	<-ready
}

func (q *admissionQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()

	var waiters *[]chan struct{}
	switch {
	case len(q.m3) != 0 && (!q.lastM3 || len(q.regular) == 0):
		waiters = &q.m3
		q.lastM3 = true
	case len(q.regular) != 0:
		waiters = &q.regular
		q.lastM3 = false
	default:
		q.active = false
		return
	}
	ready := (*waiters)[0]
	(*waiters)[0] = nil
	*waiters = (*waiters)[1:]
	close(ready)
}
