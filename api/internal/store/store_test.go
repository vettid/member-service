package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Mem: expiry hides records, ifAbsent ignores an expired one, counters
// start again after their window.
func TestMem(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := NewMem()
	s.Now = func() time.Time { return now }
	if err := s.Put(ctx, "k", "a", []byte("1"), now.Add(time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "k", "a", []byte("2"), time.Time{}, true); !errors.Is(err, ErrExists) {
		t.Fatalf("ifAbsent over a live record: %v", err)
	}
	if b, ok, _ := s.Get(ctx, "k", "a"); !ok || string(b) != "1" {
		t.Fatal("get")
	}
	now = now.Add(time.Minute)
	if _, ok, _ := s.Get(ctx, "k", "a"); ok {
		t.Fatal("expired record read")
	}
	if err := s.Put(ctx, "k", "a", []byte("3"), time.Time{}, true); err != nil {
		t.Fatalf("ifAbsent over an expired record: %v", err)
	}
	_ = s.Put(ctx, "k", "b", []byte("4"), time.Time{}, false)
	if items, _ := s.List(ctx, "k"); len(items) != 2 || items[0].ID != "a" || items[1].ID != "b" {
		t.Fatalf("list %+v", items)
	}
	_ = s.Delete(ctx, "k", "a")
	if items, _ := s.List(ctx, "k"); len(items) != 1 {
		t.Fatal("delete")
	}
	for want := 1; want <= 3; want++ {
		if n, _ := s.Incr(ctx, "rate", "x", now.Add(time.Hour)); n != want {
			t.Fatalf("incr %d", n)
		}
	}
	if n, _ := s.Count(ctx, "rate", "x"); n != 3 {
		t.Fatalf("count %d", n)
	}
	now = now.Add(time.Hour)
	if n, _ := s.Count(ctx, "rate", "x"); n != 0 {
		t.Fatalf("count after the window %d", n)
	}
	if n, _ := s.Incr(ctx, "rate", "x", now.Add(time.Hour)); n != 1 {
		t.Fatalf("after the window %d", n)
	}
}
