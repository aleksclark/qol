package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/qol/internal/auth"
	"github.com/aleksclark/qol/internal/store"
	"github.com/aleksclark/qol/internal/store/memory"
)

func TestBootstrapLoginSessionAndTicket(t *testing.T) {
	ctx := context.Background()
	service := auth.New(memory.New(), time.Hour, time.Minute)
	if err := service.Bootstrap(ctx, " Admin ", "secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Login(ctx, "admin", "wrong"); !errors.Is(err, auth.ErrCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	token, user, err := service.Login(ctx, "ADMIN", "secret")
	if err != nil || !user.Admin {
		t.Fatalf("login failed: %v", err)
	}
	current, err := service.Current(ctx, token)
	if err != nil || current.Username != "admin" {
		t.Fatalf("current user failed: %v", err)
	}
	ticket, _, err := service.CreateTicket(ctx, user.Username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeTicket(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeTicket(ctx, ticket); !errors.Is(err, auth.ErrCredentials) {
		t.Fatal("ticket was reusable")
	}
	if err := service.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Current(ctx, token); !errors.Is(err, auth.ErrCredentials) {
		t.Fatal("logout left session active")
	}
}

func TestBootstrapIsAtomic(t *testing.T) {
	ctx := context.Background()
	service := auth.New(memory.New(), time.Hour, time.Minute)
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- service.Bootstrap(ctx, "admin", "secret")
		}()
	}
	wait.Wait()
	close(results)
	var succeeded, existed int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrExists):
			existed++
		}
	}
	if succeeded != 1 || existed != 1 {
		t.Fatalf("got %d successes and %d conflicts", succeeded, existed)
	}
}
