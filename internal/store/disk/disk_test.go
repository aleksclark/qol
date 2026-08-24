package disk_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aleksclark/qol/internal/store"
	"github.com/aleksclark/qol/internal/store/disk"
)

func TestCreateGetPutDelete(t *testing.T) {
	ctx := context.Background()
	storage, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureBucket("users", 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, "users", "admin", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, "users", "admin", []byte("two")); !errors.Is(err, store.ErrExists) {
		t.Fatalf("got %v, want exists", err)
	}
	value, err := storage.Get(ctx, "users", "admin")
	if err != nil || string(value.Data) != "one" || value.Revision != 1 {
		t.Fatalf("unexpected value: %#v, %v", value, err)
	}
	if err := storage.Put(ctx, "users", "admin", []byte("two")); err != nil {
		t.Fatal(err)
	}
	value, err = storage.Get(ctx, "users", "admin")
	if err != nil || string(value.Data) != "two" || value.Revision != 2 {
		t.Fatalf("unexpected updated value: %#v, %v", value, err)
	}
	if err := storage.Delete(ctx, "users", "admin", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("got %v, want conflict", err)
	}
	if err := storage.Delete(ctx, "users", "admin", value.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Get(ctx, "users", "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want not found", err)
	}
}
