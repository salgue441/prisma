package wasm

import "sync"

// maxQueueItems bounds a shared queue; enqueues beyond it fail rather than
// growing without limit when nothing dequeues.
const maxQueueItems = 10000

// sharedQueues are plugin-wide FIFO queues (proxy_register_shared_queue).
// Every instance that registers a queue becomes one of its owners; each
// enqueue notifies one owner's root context with proxy_on_queue_ready.
type sharedQueues struct {
	mu     sync.Mutex
	byName map[string]uint32
	queues map[uint32]*queue
}

type queue struct {
	items  [][]byte
	owners []*instance
	next   int
}

func (s *sharedQueues) register(name string, owner *instance) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byName == nil {
		s.byName, s.queues = map[string]uint32{}, map[uint32]*queue{}
	}
	id, ok := s.byName[name]
	if !ok {
		id = uint32(len(s.byName) + 1)
		s.byName[name] = id
		s.queues[id] = &queue{}
	}
	q := s.queues[id]
	for _, o := range q.owners {
		if o == owner {
			return id
		}
	}
	q.owners = append(q.owners, owner)
	return id
}

func (s *sharedQueues) resolve(name string) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[name]
	return id, ok
}

// enqueue appends value and returns the owner to notify (nil if none is
// healthy; the item stays queued for the next notification).
func (s *sharedQueues) enqueue(id uint32, value []byte) (status, *instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok {
		return statusNotFound, nil
	}
	if len(q.items) >= maxQueueItems {
		return statusInternalFailure, nil
	}
	q.items = append(q.items, value)
	// Owners replaced after a crash re-register; skip the broken ones.
	live := q.owners[:0]
	for _, o := range q.owners {
		if !o.broken.Load() {
			live = append(live, o)
		}
	}
	q.owners = live
	if len(live) == 0 {
		return statusOK, nil
	}
	q.next = (q.next + 1) % len(live)
	return statusOK, live[q.next]
}

func (s *sharedQueues) dequeue(id uint32) ([]byte, status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[id]
	if !ok {
		return nil, statusNotFound
	}
	if len(q.items) == 0 {
		return nil, statusEmpty
	}
	v := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return v, statusOK
}
