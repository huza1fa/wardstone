package google

import (
	"context"
	"fmt"
	"sync"
)

type FakeClient struct {
	mu     sync.RWMutex
	Users  map[string]User
	Groups map[string][]Group
	Err    error
}

func (f *FakeClient) GetUser(ctx context.Context, email string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.Err != nil {
		return User{}, f.Err
	}
	user, ok := f.Users[email]
	if !ok {
		return User{}, fmt.Errorf("user %q not found", email)
	}
	return user, nil
}

func (f *FakeClient) ListGroups(ctx context.Context, email string) ([]Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]Group(nil), f.Groups[email]...), nil
}
