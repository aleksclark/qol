package memory

import (
	"context"
	"sync"

	"github.com/aleksclark/qol/internal/store"
)

type record struct {
	data     []byte
	revision uint64
}

type Store struct {
	mu      sync.Mutex
	buckets map[string]map[string]record
}

func New() *Store {
	return &Store{buckets: make(map[string]map[string]record)}
}

func (s *Store) Create(_ context.Context, bucket, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := s.bucket(bucket)
	if _, exists := values[key]; exists {
		return store.ErrExists
	}
	values[key] = record{data: append([]byte(nil), data...), revision: 1}
	return nil
}

func (s *Store) Get(_ context.Context, bucket, key string) (store.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.bucket(bucket)[key]
	if !exists {
		return store.Value{}, store.ErrNotFound
	}
	return store.Value{Data: append([]byte(nil), value.data...), Revision: value.revision}, nil
}

func (s *Store) Put(_ context.Context, bucket, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := s.bucket(bucket)
	value := values[key]
	value.revision++
	value.data = append([]byte(nil), data...)
	values[key] = value
	return nil
}

func (s *Store) Delete(_ context.Context, bucket, key string, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := s.bucket(bucket)
	value, exists := values[key]
	if !exists {
		return store.ErrNotFound
	}
	if revision != 0 && value.revision != revision {
		return store.ErrConflict
	}
	delete(values, key)
	return nil
}

func (s *Store) bucket(name string) map[string]record {
	if s.buckets[name] == nil {
		s.buckets[name] = make(map[string]record)
	}
	return s.buckets[name]
}
