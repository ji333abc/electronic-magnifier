package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestPlaybackCacheIndependentReadersAndCapacity(t *testing.T) {
	server := &Server{}
	configurePlaybackTestCache(t, server)
	cache := &server.playbackCache
	loads := 0
	load := func() (string, error) {
		loads++
		f, err := os.CreateTemp(cache.dir, "clip-*")
		if err != nil {
			return "", err
		}
		_, err = f.WriteString("0123456789")
		f.Close()
		return f.Name(), err
	}
	first, releaseFirst, err := cache.acquire(context.Background(), "first", load)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	second, releaseSecond, err := cache.acquire(context.Background(), "first", load)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	_, _ = first.Seek(8, io.SeekStart)
	data, err := io.ReadAll(second)
	if err != nil || string(data) != "0123456789" || loads != 1 {
		t.Fatalf("readers share an offset or regenerated the clip: %q %v, loads=%d", data, err, loads)
	}
	other, releaseOther, err := cache.acquire(context.Background(), "other", load)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cache.acquire(context.Background(), "over capacity", load)
	var statusErr *recordingPlaybackError
	if !errors.As(err, &statusErr) || statusErr.status != http.StatusServiceUnavailable || loads != 2 {
		t.Fatalf("active clip limit was not enforced: %v, loads=%d", err, loads)
	}
	oldPath := other.Name()
	releaseOther()
	_, releaseReplacement, err := cache.acquire(context.Background(), "replacement", load)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseReplacement()
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("eviction did not remove idle temporary file: %v", err)
	}
}

func TestPlaybackCacheCanceledWaiterAndIdleCleanup(t *testing.T) {
	server := &Server{}
	configurePlaybackTestCache(t, server)
	cache := &server.playbackCache
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, release, err := cache.acquire(context.Background(), "clip", func() (string, error) {
			close(started)
			<-finish
			f, err := os.CreateTemp(cache.dir, "clip-*")
			if err != nil {
				return "", err
			}
			f.Close()
			return f.Name(), nil
		})
		if err == nil {
			release()
		}
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := cache.acquire(ctx, "clip", func() (string, error) {
		t.Error("duplicate preparation")
		return "", errors.New("unexpected load")
	})
	close(finish)
	if loadErr := <-done; loadErr != nil {
		t.Fatal(loadErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter did not respect cancellation: %v", err)
	}
	cache.mu.Lock()
	entry := cache.entries["clip"]
	path := entry.path
	entry.timer.Reset(0)
	cache.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		remaining := len(cache.entries)
		cache.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle clip was not cleaned up")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("idle cleanup left temporary file: %v", err)
	}
}
