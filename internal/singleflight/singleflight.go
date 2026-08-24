// Package singleflight provides duplicate-function-call suppression. When
// multiple goroutines request the same work key concurrently, only one runs
// the function; the rest wait and receive the same result.
package singleflight

import "sync"

// call is an in-flight or completed unit of work.
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// Group represents a class of work and forms a namespace in which units of
// work can be executed with duplicate suppression.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do executes and returns the results of the given function, ensuring that
// only one execution is in-flight for a given key at a time. A duplicate
// caller waits for the original to complete and receives the same results.
// The boolean return reports whether the result was shared with a caller
// that arrived while the function was already running.
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error, bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err, false
}
