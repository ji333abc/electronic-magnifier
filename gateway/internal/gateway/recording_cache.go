package gateway

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

const (
	recordingPlaybackMaxBytes = 512 << 20
	recordingPlaybackMaxClips = 2
	recordingPlaybackIdleTTL  = 2 * time.Minute
)

type recordingPlaybackError struct {
	status int
}

func (e *recordingPlaybackError) Error() string {
	return fmt.Sprintf("recording playback: HTTP %d", e.status)
}

type recordingPlaybackEntry struct {
	ready chan struct{}
	path  string
	err   error
	refs  int
	timer *time.Timer
}

// Limit both preparing and in-use clips. Each request opens its own file so
// simultaneous byte-range requests never share a seek offset. Only idle files
// may be evicted; the same clip stays byte-for-byte stable while it is in use.
type recordingPlaybackCache struct {
	mu      sync.Mutex
	entries map[string]*recordingPlaybackEntry
	dir     string
}

func (c *recordingPlaybackCache) tempDir() string {
	if c.dir != "" {
		return c.dir
	}
	// Armbian may mount /tmp as a small RAM disk. /var/tmp is writable with
	// systemd PrivateTmp and keeps large clips out of the gateway's Go heap.
	if runtime.GOOS == "linux" {
		return "/var/tmp"
	}
	return ""
}

func (c *recordingPlaybackCache) acquire(ctx context.Context, key string, load func() (string, error)) (*os.File, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*recordingPlaybackEntry)
	}
	entry, exists := c.entries[key]
	if !exists {
		if len(c.entries) >= recordingPlaybackMaxClips {
			for oldKey, old := range c.entries {
				if old.refs == 0 {
					c.removeLocked(oldKey, old)
					break
				}
			}
		}
		if len(c.entries) >= recordingPlaybackMaxClips {
			c.mu.Unlock()
			return nil, nil, &recordingPlaybackError{status: http.StatusServiceUnavailable}
		}
		entry = &recordingPlaybackEntry{ready: make(chan struct{})}
		c.entries[key] = entry
	}
	entry.refs++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	c.mu.Unlock()

	if !exists {
		entry.path, entry.err = load()
		close(entry.ready)
	}
	select {
	case <-ctx.Done():
		c.release(key, entry)
		return nil, nil, ctx.Err()
	case <-entry.ready:
	}
	if entry.err != nil {
		c.release(key, entry)
		return nil, nil, entry.err
	}
	file, err := os.Open(entry.path)
	if err != nil {
		c.release(key, entry)
		return nil, nil, err
	}
	return file, func() {
		file.Close()
		c.release(key, entry)
	}, nil
}

func (c *recordingPlaybackCache) release(key string, entry *recordingPlaybackEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.refs--
	if entry.refs != 0 {
		return
	}
	if entry.err != nil {
		c.removeLocked(key, entry)
		return
	}
	entry.timer = time.AfterFunc(recordingPlaybackIdleTTL, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries[key] == entry && entry.refs == 0 {
			c.removeLocked(key, entry)
		}
	})
}

func (c *recordingPlaybackCache) removeLocked(key string, entry *recordingPlaybackEntry) {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.path != "" {
		os.Remove(entry.path)
	}
	delete(c.entries, key)
}
