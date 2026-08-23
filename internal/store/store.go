package store

import (
	"context"
	"errors"
)

var (
	ErrNotFound = errors.New("record not found")
	ErrExists   = errors.New("record already exists")
	ErrConflict = errors.New("record revision conflict")
)

type Value struct {
	Data     []byte
	Revision uint64
}

type Store interface {
	Create(context.Context, string, string, []byte) error
	Get(context.Context, string, string) (Value, error)
	Put(context.Context, string, string, []byte) error
	Delete(context.Context, string, string, uint64) error
}
