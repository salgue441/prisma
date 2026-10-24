package cache

import (
	"strings"
	"testing"
	"time"
)

func TestStoreLRUAndExpiry(t *testing.T) {
	now := time.Now()
	body := []byte(strings.Repeat("x", 100))
	entry := func(ttl time.Duration) *Entry {
		return &Entry{Status: 200, Body: body, StoredAt: now, Expires: now.Add(ttl)}
	}
	one := entry(time.Hour).size("r|a")
	s := NewStore(one*2 + 10)
	s.Put("r|a", entry(time.Hour))
	s.Put("r|b", entry(time.Hour))
	s.Get("r|a", now) // a is now most recently used
	s.Put("r|c", entry(time.Hour))
	if s.Get("r|b", now) != nil {
		t.Error("least recently used entry should have been evicted")
	}
	if s.Get("r|a", now) == nil || s.Get("r|c", now) == nil {
		t.Error("recent entries were evicted")
	}
	if s.Get("r|a", now.Add(2*time.Hour)) != nil {
		t.Error("expired entry was served")
	}
	if n, _ := s.Stats(); n != 1 {
		t.Errorf("expired entry should be removed on access, %d entries left", n)
	}
	s.Put("other|x", entry(time.Hour))
	if purged := s.Purge("r|"); purged != 1 {
		t.Errorf("purged %d, want 1", purged)
	}
	s.Put("huge", &Entry{Body: make([]byte, 1<<20), Expires: now.Add(time.Hour)})
	if s.Get("huge", now) != nil {
		t.Error("entries larger than the whole budget must be rejected")
	}
}

func TestParseCacheControl(t *testing.T) {
	cc := parseCacheControl(`public, max-age=60, s-maxage="120"`)
	if cc.maxAge != 60 || cc.sMaxAge != 120 || cc.private || cc.noStore {
		t.Errorf("%+v", cc)
	}
	cc = parseCacheControl("Private, No-Store")
	if !cc.private || !cc.noStore || cc.maxAge != -1 {
		t.Errorf("%+v", cc)
	}
}
