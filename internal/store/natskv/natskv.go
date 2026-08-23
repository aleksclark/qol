package natskv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aleksclark/qol/internal/store"
	"github.com/nats-io/nats.go"
)

type Store struct {
	jetstream nats.JetStreamContext
	mu        sync.Mutex
	buckets   map[string]nats.KeyValue
}

func New(connection *nats.Conn) (*Store, error) {
	jetstream, err := connection.JetStream()
	if err != nil {
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	return &Store{jetstream: jetstream, buckets: make(map[string]nats.KeyValue)}, nil
}

func (s *Store) EnsureBucket(name string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.buckets[name]; exists {
		return nil
	}
	bucket, err := s.jetstream.KeyValue(name)
	if errors.Is(err, nats.ErrBucketNotFound) {
		bucket, err = s.jetstream.CreateKeyValue(&nats.KeyValueConfig{Bucket: name, TTL: ttl, Storage: nats.FileStorage})
	}
	if err != nil {
		return fmt.Errorf("ensure bucket %s: %w", name, err)
	}
	s.buckets[name] = bucket
	return nil
}

func (s *Store) Create(_ context.Context, bucket, key string, data []byte) error {
	_, err := s.buckets[bucket].Create(key, data)
	if errors.Is(err, nats.ErrKeyExists) {
		return store.ErrExists
	}
	return err
}

func (s *Store) Get(_ context.Context, bucket, key string) (store.Value, error) {
	entry, err := s.buckets[bucket].Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, nats.ErrKeyDeleted) {
		return store.Value{}, store.ErrNotFound
	}
	if err != nil {
		return store.Value{}, err
	}
	return store.Value{Data: entry.Value(), Revision: entry.Revision()}, nil
}

func (s *Store) Put(_ context.Context, bucket, key string, data []byte) error {
	_, err := s.buckets[bucket].Put(key, data)
	return err
}

func (s *Store) Delete(_ context.Context, bucket, key string, revision uint64) error {
	err := s.buckets[bucket].Delete(key, nats.LastRevision(revision))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return store.ErrNotFound
	}
	if errors.Is(err, nats.ErrKeyExists) {
		return store.ErrConflict
	}
	return err
}
