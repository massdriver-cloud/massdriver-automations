package scorecards

import (
	"context"
	"sync"
)

// queue holds the work to do, by job key, such as instance/billing-dev-app. A key waits in the queue once, however
// many events name it. A key that is named while its job runs runs again after it, so the last run reads the
// newest state.
type queue struct {
	mu      sync.Mutex
	wake    chan struct{}
	order   []string
	waiting map[string]bool
	running map[string]bool
	again   map[string]bool
}

func newQueue() *queue {
	return &queue{wake: make(chan struct{}, 1), waiting: map[string]bool{}, running: map[string]bool{},
		again: map[string]bool{}}
}

func (q *queue) add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.running[key]:
		q.again[key] = true
		return
	case q.waiting[key]:
		return
	}
	q.waiting[key] = true
	q.order = append(q.order, key)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns the next key that is not running, or false when there is none.
func (q *queue) take() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, key := range q.order {
		if q.running[key] {
			continue
		}
		q.order = append(q.order[:i], q.order[i+1:]...)
		delete(q.waiting, key)
		q.running[key] = true
		return key, true
	}
	return "", false
}

func (q *queue) done(key string) {
	q.mu.Lock()
	delete(q.running, key)
	rerun := q.again[key]
	delete(q.again, key)
	q.mu.Unlock()
	if rerun {
		q.add(key)
	}
}

// work runs jobs until ctx is done.
func (q *queue) work(ctx context.Context, run func(context.Context, string)) {
	for {
		for {
			key, ok := q.take()
			if !ok {
				break
			}
			run(ctx, key)
			q.done(key)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
	}
}

// drain runs jobs, one at a time, until the queue is empty.
func (q *queue) drain(ctx context.Context, run func(context.Context, string)) {
	for {
		key, ok := q.take()
		if !ok {
			return
		}
		run(ctx, key)
		q.done(key)
	}
}
