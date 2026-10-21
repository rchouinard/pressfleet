package repository

import (
	"context"
	"sync"
)

// initLocks makes repository initialization single-flight per repository
// location in this process. It is package-level, not per Manager, because
// several Managers (a job handler's, a CLI command's) may open the same
// repository.
var initLocks = &keyedLock{held: map[string]*heldLock{}}

// keyedLock is a set of mutexes by key. Waiting honors the context, which
// sync.Mutex can't, and an entry exists only while somebody holds or waits
// for its key.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]*heldLock
}

type heldLock struct {
	// ch holds one token while the key is free.
	ch   chan struct{}
	refs int
}

// lock waits for key and returns the function that releases it, or the
// context's error.
func (k *keyedLock) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	h := k.held[key]
	if h == nil {
		h = &heldLock{ch: make(chan struct{}, 1)}
		h.ch <- struct{}{}
		k.held[key] = h
	}
	h.refs++
	k.mu.Unlock()

	release := func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if h.refs--; h.refs == 0 {
			delete(k.held, key)
		}
	}
	select {
	case <-h.ch:
		return func() {
			h.ch <- struct{}{}
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
