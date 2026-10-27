// Package store keeps the member API's records: sessions, sign-up and
// sign-in flows, the roster and rate-limit counters. Records are small
// JSON documents addressed by a kind and an id, with an optional expiry
// (DynamoDB in a deployment, Mem in tests and local development).
package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrExists is returned by Put with ifAbsent when the record exists.
var ErrExists = errors.New("store: record exists")

// Item is one record of a List.
type Item struct {
	ID   string
	Data []byte
}

// Store is the member API's storage. An expired record reads as absent.
type Store interface {
	// Get returns a record's data.
	Get(ctx context.Context, kind, id string) (data []byte, ok bool, err error)
	// Put writes a record (exp zero: kept until deleted). With ifAbsent it
	// fails with ErrExists when a live record exists.
	Put(ctx context.Context, kind, id string, data []byte, exp time.Time, ifAbsent bool) error
	// Delete removes a record; a missing one is no error.
	Delete(ctx context.Context, kind, id string) error
	// List returns the live records of a kind, by id.
	List(ctx context.Context, kind string) ([]Item, error)
	// Incr adds one to a counter and returns the new value. A counter
	// that does not exist or has expired starts again at 1 and lives
	// until exp.
	Incr(ctx context.Context, kind, id string, exp time.Time) (int, error)
	// Count returns a live counter's value (0 when absent or expired).
	Count(ctx context.Context, kind, id string) (int, error)
}

type memRecord struct {
	data []byte
	n    int
	exp  time.Time
}

// Mem is an in-memory Store.
type Mem struct {
	mu  sync.Mutex
	m   map[string]map[string]*memRecord
	Now func() time.Time
}

// NewMem returns an empty Mem.
func NewMem() *Mem { return &Mem{m: map[string]map[string]*memRecord{}, Now: time.Now} }

func (s *Mem) live(r *memRecord) bool { return r != nil && (r.exp.IsZero() || s.Now().Before(r.exp)) }

func (s *Mem) kind(k string) map[string]*memRecord {
	if s.m[k] == nil {
		s.m[k] = map[string]*memRecord{}
	}
	return s.m[k]
}

// Get implements Store.
func (s *Mem) Get(_ context.Context, kind, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[kind][id]
	if !s.live(r) {
		return nil, false, nil
	}
	return append([]byte(nil), r.data...), true, nil
}

// Put implements Store.
func (s *Mem) Put(_ context.Context, kind, id string, data []byte, exp time.Time, ifAbsent bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.kind(kind)
	if ifAbsent && s.live(k[id]) {
		return ErrExists
	}
	k[id] = &memRecord{data: append([]byte(nil), data...), exp: exp}
	return nil
}

// Delete implements Store.
func (s *Mem) Delete(_ context.Context, kind, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[kind], id)
	return nil
}

// List implements Store.
func (s *Mem) List(_ context.Context, kind string) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Item
	for id, r := range s.m[kind] {
		if s.live(r) {
			out = append(out, Item{ID: id, Data: append([]byte(nil), r.data...)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Incr implements Store.
func (s *Mem) Incr(_ context.Context, kind, id string, exp time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.kind(kind)
	r := k[id]
	if !s.live(r) {
		r = &memRecord{exp: exp}
		k[id] = r
	}
	r.n++
	return r.n, nil
}

// Count implements Store.
func (s *Mem) Count(_ context.Context, kind, id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[kind][id]
	if !s.live(r) {
		return 0, nil
	}
	return r.n, nil
}
