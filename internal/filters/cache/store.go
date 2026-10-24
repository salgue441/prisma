// Package cache implements the gateway's shared HTTP response cache.
package cache

import (
	"container/list"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Status   int
	Header   http.Header
	Body     []byte
	StoredAt time.Time
	Expires  time.Time
	// Vary holds the request header values this variant was stored for.
	Vary map[string]string
}

func (e *Entry) size(key string) int64 {
	n := int64(len(key) + len(e.Body) + 64)
	for k, vs := range e.Header {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	return n
}

// Store is an LRU bounded by total bytes. It is shared by every route and
// survives config reloads.
type Store struct {
	mu      sync.Mutex
	max     int64
	size    int64
	ll      *list.List
	entries map[string]*list.Element
}

type item struct {
	key   string
	entry *Entry
	size  int64
}

func NewStore(maxBytes int64) *Store {
	return &Store{max: maxBytes, ll: list.New(), entries: map[string]*list.Element{}}
}

// SetMax changes the budget, evicting if needed.
func (s *Store) SetMax(maxBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max = maxBytes
	s.evictLocked()
}

func (s *Store) Get(key string, now time.Time) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[key]
	if !ok {
		return nil
	}
	it := el.Value.(*item)
	if !now.Before(it.entry.Expires) {
		s.removeLocked(el)
		return nil
	}
	s.ll.MoveToFront(el)
	return it.entry
}

func (s *Store) Put(key string, e *Entry) {
	sz := e.size(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sz > s.max {
		return
	}
	if el, ok := s.entries[key]; ok {
		s.removeLocked(el)
	}
	s.entries[key] = s.ll.PushFront(&item{key: key, entry: e, size: sz})
	s.size += sz
	s.evictLocked()
}

// Purge removes every key with the given prefix ("" purges everything) and
// returns how many entries were removed.
func (s *Store) Purge(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, el := range s.entries {
		if strings.HasPrefix(k, prefix) {
			s.removeLocked(el)
			n++
		}
	}
	return n
}

// Stats reports entry count and bytes used.
func (s *Store) Stats() (entries int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.size
}

func (s *Store) evictLocked() {
	for s.size > s.max {
		el := s.ll.Back()
		if el == nil {
			return
		}
		s.removeLocked(el)
	}
}

func (s *Store) removeLocked(el *list.Element) {
	it := el.Value.(*item)
	s.ll.Remove(el)
	delete(s.entries, it.key)
	s.size -= it.size
}
