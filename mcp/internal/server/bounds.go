// SPDX-License-Identifier: Apache-2.0

package server

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// What the server keeps is bounded: the derived documents it remembers (lru), the explain sections with no target
// (staticCache), each principal's open long-polls (startPoll), the rate limiter's buckets, and, through Sweep, what the
// state store holds past its expiry (ARCHITECTURE.md 5.10).

// lru maps keys to values, remembering at most n: adding to a full one forgets the entry least recently read or
// written. It is safe for concurrent use.
type lru struct {
	mu    sync.Mutex
	n     int
	order *list.List // of *lruEntry, most recent first
	items map[string]*list.Element
}

type lruEntry struct{ key, val string }

func newLRU(n int) *lru {
	return &lru{n: n, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(e)
	return e.Value.(*lruEntry).val, true
}

func (c *lru) put(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value.(*lruEntry).val = val
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key, val})
	for c.order.Len() > c.n {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}

func (c *lru) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// values is the set of the values remembered.
func (c *lru) values() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.items))
	for _, e := range c.items {
		out[e.Value.(*lruEntry).val] = true
	}
	return out
}

// staticCache holds texts by key (staticView): pinned ones for the life of the process, and the last maxStatic of the
// others. Requests of one key that arrive while it is computed wait for that computation, so a cold cache flooded with
// one key starts one engine. A failed computation is not remembered; the next request computes again. It is safe for
// concurrent use.
type staticCache struct {
	mu      sync.Mutex
	pinned  map[string][]byte
	recent  *lru
	flights map[string]*flight
}

// flight is a computation that requests of its key wait for.
type flight struct {
	done chan struct{}
	val  []byte
	err  error
}

func newStaticCache(n int) *staticCache {
	return &staticCache{pinned: map[string][]byte{}, recent: newLRU(n), flights: map[string]*flight{}}
}

// get returns the text of key: remembered, from a computation already running, or from compute, which it then
// remembers (pinned when pin is true). A request that ends while it waits gets ctx's error.
func (c *staticCache) get(ctx context.Context, key string, pin bool, compute func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if v, ok := c.pinned[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	if v, ok := c.recent.get(key); ok {
		c.mu.Unlock()
		return []byte(v), nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.val, f.err
		case <-ctx.Done():
			return nil, internalError("the request ended while the section was computed")
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	f.val, f.err = compute()
	c.mu.Lock()
	delete(c.flights, key)
	if f.err == nil && pin {
		c.pinned[key] = f.val
	} else if f.err == nil {
		c.recent.put(key, string(f.val))
	}
	c.mu.Unlock()
	close(f.done)
	return f.val, f.err
}

// startPoll counts a long-poll of p, unless p already holds MaxPolls. done must be called once when it ends.
func (s *Server) startPoll(p state.Principal) (done func(), ok bool) {
	s.pollsMu.Lock()
	defer s.pollsMu.Unlock()
	if s.polls[p] >= s.opt.MaxPolls {
		return nil, false
	}
	s.polls[p]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.pollsMu.Lock()
			defer s.pollsMu.Unlock()
			if s.polls[p]--; s.polls[p] <= 0 {
				delete(s.polls, p)
			}
		})
	}, true
}

// Sweep removes from the state store what has expired, when the store can (state.Sweeper): handles past their expiry,
// jobs past their retention, preview logs with no row left, and the artifacts nothing live names. An artifact is kept
// for the longest handle lifetime after it was last stored, so a document stored for a handle about to be minted, or
// derived from a prediction that may still be explained, stays; one the server still remembers as derived stays too.
// It returns how many entries it removed from the store. It also forgets the rate limiter's buckets that have refilled
// (ratelimit.Limiter.Prune), whatever the store.
func (s *Server) Sweep(ctx context.Context) (int, error) {
	s.opt.Limits.Prune()
	sw, ok := s.opt.Store.(state.Sweeper)
	if !ok {
		return 0, nil
	}
	return sw.Sweep(ctx, max(s.opt.FitTTL, s.opt.PredTTL), s.derived.values())
}

// RunSweeper sweeps at once, then every interval (DefaultSweepEvery when 0), until ctx ends. The stdio and HTTP modes
// run it beside the server; a failed sweep is tried again at the next interval.
func (s *Server) RunSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultSweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		_, _ = s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
