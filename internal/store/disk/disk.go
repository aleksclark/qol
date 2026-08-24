package disk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aleksclark/qol/internal/store"
)

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("data directory is required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) EnsureBucket(name string, _ time.Duration) error {
	return os.MkdirAll(s.bucketDir(name), 0o750)
}

func (s *Store) Create(_ context.Context, bucket, key string, data []byte) error {
	path := s.recordPath(bucket, key)
	if _, err := os.Stat(path); err == nil {
		return store.ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomic(path, encode(1, data))
}

func (s *Store) Get(_ context.Context, bucket, key string) (store.Value, error) {
	payload, err := os.ReadFile(s.recordPath(bucket, key))
	if errors.Is(err, os.ErrNotExist) {
		return store.Value{}, store.ErrNotFound
	}
	if err != nil {
		return store.Value{}, err
	}
	revision, data, err := decode(payload)
	if err != nil {
		return store.Value{}, err
	}
	return store.Value{Data: data, Revision: revision}, nil
}

func (s *Store) Put(_ context.Context, bucket, key string, data []byte) error {
	path := s.recordPath(bucket, key)
	revision := uint64(1)
	if payload, err := os.ReadFile(path); err == nil {
		current, _, err := decode(payload)
		if err != nil {
			return err
		}
		revision = current + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomic(path, encode(revision, data))
}

func (s *Store) Delete(_ context.Context, bucket, key string, revision uint64) error {
	path := s.recordPath(bucket, key)
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	current, _, err := decode(payload)
	if err != nil {
		return err
	}
	if revision != 0 && current != revision {
		return store.ErrConflict
	}
	return os.Remove(path)
}

func (s *Store) bucketDir(name string) string {
	return filepath.Join(s.root, filepath.Base(name))
}

func (s *Store) recordPath(bucket, key string) string {
	return filepath.Join(s.bucketDir(bucket), filepath.Base(key))
}

func encode(revision uint64, data []byte) []byte {
	payload := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(payload, revision)
	copy(payload[8:], data)
	return payload
}

func decode(payload []byte) (uint64, []byte, error) {
	if len(payload) < 8 {
		return 0, nil, fmt.Errorf("record is truncated")
	}
	return binary.BigEndian.Uint64(payload[:8]), append([]byte(nil), payload[8:]...), nil
}

func writeAtomic(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "record-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	return os.Rename(temporaryPath, path)
}
