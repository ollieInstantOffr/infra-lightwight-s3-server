package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const testLockKey int64 = 0x5333_44_FF

// The property the alert engine depends on: while one holder is inside fn, a
// second caller is refused rather than queued. Queueing would run the same
// cycle moments later, which is the duplicate the lock exists to prevent.
func TestASecondCallerIsRefusedRatherThanQueued(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	inside := make(chan struct{})
	release := make(chan struct{})
	var firstRan bool

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ran, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("first WithSingleton: %v", err)
		}
		firstRan = ran
	}()

	<-inside // The first holder is now inside fn.

	secondRan, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error {
		t.Error("the second caller ran fn while the first was still inside it")
		return nil
	})
	if err != nil {
		t.Fatalf("second WithSingleton: %v", err)
	}
	if secondRan {
		t.Error("the second caller reported that it ran, so two processes would both notify")
	}

	close(release)
	wg.Wait()
	if !firstRan {
		t.Error("the first caller did not report running")
	}
}

// Once the holder returns, the lock must be available again — otherwise the
// first evaluation would be the only one that ever happens.
func TestTheLockIsReleasedWhenTheWorkFinishes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for attempt := 1; attempt <= 3; attempt++ {
		ran, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error { return nil })
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if !ran {
			t.Fatalf("attempt %d did not run, so the lock was not released", attempt)
		}
	}
}

// A cycle that fails must not leave the lock held, or one transient database
// error would stop alerting until the process restarted.
func TestTheLockIsReleasedWhenTheWorkFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sentinel := errors.New("the work failed")

	ran, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error { return sentinel })
	if !ran {
		t.Fatal("the work did not run")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithSingleton = %v, want the work's own error", err)
	}

	ran, err = WithSingleton(ctx, pool, testLockKey, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("after a failure: %v", err)
	}
	if !ran {
		t.Error("the lock survived a failing cycle, so alerting would stop until a restart")
	}
}

// Panicking is not a case the engine produces, but a lock that outlived one
// would wedge every other process until this one was restarted.
func TestTheLockIsReleasedOnPanic(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	func() {
		defer func() { _ = recover() }()
		_, _ = WithSingleton(ctx, pool, testLockKey, func(context.Context) error {
			panic("something went wrong inside the cycle")
		})
	}()

	ran, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("after a panic: %v", err)
	}
	if !ran {
		t.Error("a panic left the lock held")
	}
}

// Different keys must not exclude each other, or adding a second singleton
// later would silently serialise it against the alert engine.
func TestDifferentKeysDoNotBlockEachOther(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	inside := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = WithSingleton(ctx, pool, testLockKey, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()

	<-inside
	ran, err := WithSingleton(ctx, pool, testLockKey+1, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("other key: %v", err)
	}
	if !ran {
		t.Error("a different key was blocked by an unrelated lock")
	}

	close(release)
	wg.Wait()
}

// Only one of many concurrent callers may run, which is the multi-process case
// the split actually produces during a restart.
func TestExactlyOneOfManyConcurrentCallersRuns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const callers = 8
	var (
		mu       sync.Mutex
		running  int
		maxSeen  int
		ranCount int
	)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ran, err := WithSingleton(ctx, pool, testLockKey, func(context.Context) error {
				mu.Lock()
				running++
				if running > maxSeen {
					maxSeen = running
				}
				mu.Unlock()

				// Held long enough that a second entrant would overlap
				// observably rather than by luck of scheduling.
				time.Sleep(50 * time.Millisecond)

				mu.Lock()
				running--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithSingleton: %v", err)
			}
			if ran {
				mu.Lock()
				ranCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if maxSeen > 1 {
		t.Errorf("%d callers were inside the work at once, want at most 1", maxSeen)
	}
	if ranCount == 0 {
		t.Error("no caller ran at all")
	}
}
