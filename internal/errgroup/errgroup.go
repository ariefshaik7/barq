// Package errgroup provides the small subset of golang.org/x/sync/errgroup
// that Barq needs.
//
// Barq has a hard zero dependency constraint, so the group is
// implemented here. The semantics match the upstream package: the
// first error cancels the group, Wait returns the first error, and a
// nil error is returned when every function succeeded.
//
// The zero value is not usable; call Group or WithContext.
package errgroup

import (
	"context"
	"fmt"
	"sync"
)

// Group runs a set of functions concurrently.
//
// The zero value is not usable. Create a Group with the Group
// function, or use WithContext.
type Group struct {
	cancel func(error)

	wg sync.WaitGroup

	mu sync.Mutex
	// errOnce ensures that only the first error is recorded and that
	// the cancellation function is called exactly once.
	errOnce sync.Once
	err     error
	// limit and sem implement the optional concurrency limit.
	limit   int
	started int
	sem     chan struct{}
}

// WithContext returns a new Group and an associated Context derived
// from ctx.
//
// The derived context is canceled the first time a function passed to
// Go returns a non-nil error, or the first time Wait returns. If the
// function passed to Go returns a nil error, the derived Context is
// not canceled.
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	return &Group{cancel: cancel}, ctx
}

// New returns a new Group with an associated Context derived from
// context.Background.
//
// The derived Context is canceled the first time a function passed to
// Go returns a non-nil error, or the first time Wait returns.
func New() (*Group, context.Context) {
	return WithContext(context.Background())
}

// Go calls the given function in a new goroutine.
//
// The first call to return a non-nil error cancels the group's
// context. The error is returned by Wait.
//
// Go must not be called after Wait has returned.
func (g *Group) Go(f func() error) {
	if f == nil {
		panic("errgroup: Go called with a nil function")
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				if g.cancel != nil {
					g.cancel(err)
				}
			}
			g.mu.Unlock()
		}
	}()
}

// Wait blocks until all the function calls from Go have returned, and
// then returns the first non-nil error, if any.
//
// It is safe to call Wait from multiple goroutines.
func (g *Group) Wait() error {
	g.wg.Wait()
	g.mu.Lock()
	err := g.err
	g.mu.Unlock()
	if err == nil && g.cancel != nil {
		// A nil error must still cancel the derived context, because
		// the upstream contract says the context is canceled when Wait
		// returns.
		g.cancel(nil)
	}
	return err
}

// SetLimit limits the number of active goroutines in this group to at
// most n.
//
// A negative value indicates no limit. A zero value prevents any new
// goroutine from being started.
//
// Calling SetLimit after Go has been called is a programming error.
func (g *Group) SetLimit(n int) {
	if n < 0 {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.limit = 0
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started > n {
		panic("errgroup: modify limit after Go")
	}
	g.limit = n
	g.sem = make(chan struct{}, n)
}

// TryGo runs the given function in a new goroutine only if the group
// is below its configured limit.
//
// It returns false if the limit would be exceeded.
func (g *Group) TryGo(f func() error) bool {
	g.mu.Lock()
	if g.sem != nil {
		select {
		case g.sem <- struct{}{}:
		default:
			g.mu.Unlock()
			return false
		}
	}
	g.started++
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer func() {
			if g.sem != nil {
				<-g.sem
			}
			g.wg.Done()
		}()
		if err := f(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				if g.cancel != nil {
					g.cancel(err)
				}
			}
			g.mu.Unlock()
		}
	}()
	return true
}

// Errorf returns an error with a formatted message. It exists so that
// a caller does not need to import fmt just to build an error.
func Errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
