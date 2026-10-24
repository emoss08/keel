package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/cache"
)

// failing is a Store whose every call fails.
type failing struct{}

func (failing) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("down")
}

func (failing) Set(context.Context, string, []byte, time.Duration) error { return errors.New("down") }

func TestLoaderSharesConcurrentMisses(t *testing.T) {
	ctx := context.Background()
	store := cache.NewMemory(cache.MemoryOptions{})
	l := cache.NewLoader(store, cache.LoaderOptions{})
	var calls atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("report"), nil
	}
	var wg sync.WaitGroup
	results := make([]string, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, hit, err := l.Load(ctx, "k", time.Minute, load)
			if err != nil || hit {
				t.Errorf("Load = %v %v", hit, err)
			}
			results[i] = string(v)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("load ran %d times, want 1", n)
	}
	for _, r := range results {
		if r != "report" {
			t.Fatalf("a waiter got %q", r)
		}
	}
	v, hit, err := l.Load(ctx, "k", time.Minute, load)
	if err != nil || !hit || string(v) != "report" {
		t.Fatalf("second Load = %q %v %v, want a hit", v, hit, err)
	}
}

func TestLoaderErrorsAreNotCached(t *testing.T) {
	ctx := context.Background()
	l := cache.NewLoader(cache.NewMemory(cache.MemoryOptions{}), cache.LoaderOptions{})
	boom := errors.New("boom")
	if _, _, err := l.Load(ctx, "k", time.Minute, func(context.Context) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	v, hit, err := l.Load(ctx, "k", time.Minute, func(context.Context) ([]byte, error) { return []byte("ok"), nil })
	if err != nil || hit || string(v) != "ok" {
		t.Fatalf("Load after a failure = %q %v %v", v, hit, err)
	}
}

func TestLoaderPanicWakesWaiters(t *testing.T) {
	l := cache.NewLoader(cache.NewMemory(cache.MemoryOptions{}), cache.LoaderOptions{})
	_, _, err := l.Load(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) {
		panic("bad report")
	})
	if err == nil || err.Error() != "cache: load panicked: bad report" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoaderCancelsWhenEveryWaiterLeaves(t *testing.T) {
	l := cache.NewLoader(cache.NewMemory(cache.MemoryOptions{}), cache.LoaderOptions{})
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := l.Load(ctx, "k", time.Minute, func(lctx context.Context) ([]byte, error) {
		<-lctx.Done()
		close(cancelled)
		return nil, lctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the load was not cancelled after its only waiter left")
	}
	v, _, err := l.Load(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("fresh"), nil
	})
	if err != nil || string(v) != "fresh" {
		t.Fatalf("a caller after the cancellation got %q %v", v, err)
	}
}

func TestLoaderKeepsLoadingForRemainingWaiters(t *testing.T) {
	l := cache.NewLoader(cache.NewMemory(cache.MemoryOptions{}), cache.LoaderOptions{})
	release := make(chan struct{})
	load := func(lctx context.Context) ([]byte, error) {
		select {
		case <-release:
			return []byte("done"), nil
		case <-lctx.Done():
			return nil, lctx.Err()
		}
	}
	quitter, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := l.Load(quitter, "k", time.Minute, load)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	done := make(chan string, 1)
	go func() {
		v, _, _ := l.Load(context.Background(), "k", time.Minute, load)
		done <- string(v)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("the quitter got %v", err)
	}
	close(release)
	if v := <-done; v != "done" {
		t.Fatalf("the remaining waiter got %q", v)
	}
}

func TestLoaderSurvivesStoreFailures(t *testing.T) {
	var reported atomic.Int32
	l := cache.NewLoader(failing{}, cache.LoaderOptions{OnError: func(error) { reported.Add(1) }})
	v, hit, err := l.Load(context.Background(), "k", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("v"), nil
	})
	if err != nil || hit || string(v) != "v" {
		t.Fatalf("Load = %q %v %v", v, hit, err)
	}
	if n := reported.Load(); n != 2 {
		t.Fatalf("reported %d errors, want the get and the set", n)
	}
}
