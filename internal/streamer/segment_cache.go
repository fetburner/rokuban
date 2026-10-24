package streamer

import (
	"container/list"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	errSegmentCacheFull = errors.New("segment cache has no evictable space")
	errSegmentNotCached = errors.New("segment is not cached")
)

// SegmentKey identifies one immutable HLS segment produced from a recording generation.
type SegmentKey struct {
	RecordingID string
	Generation  string
	Profile     string
	Rendition   string
	Number      int64
}

func (k SegmentKey) validate() error {
	if k.RecordingID == "" || k.Generation == "" || k.Profile == "" || k.Rendition == "" || k.Number < 0 {
		return fmt.Errorf("invalid segment key: %#v", k)
	}
	return nil
}

type segmentCacheEntry struct {
	key        SegmentKey
	path       string
	size       int64
	leases     int
	lruElement *list.Element
}

// segmentCache is a byte-bounded, process-local LRU of files on one filesystem.
// A lease protects an entry until a response has opened the file and begun delivery.
type segmentCache struct {
	mu       sync.Mutex
	root     string
	maxBytes int64
	used     int64
	entries  map[SegmentKey]*segmentCacheEntry
	lru      list.List
	changed  chan struct{}
}

func newSegmentCache(root string, maxBytes int64) (*segmentCache, error) {
	if root == "" {
		return nil, errors.New("segment cache root is empty")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("segment cache byte limit must be positive: %d", maxBytes)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("creating segment cache root: %w", err)
	}
	return &segmentCache{
		root:     root,
		maxBytes: maxBytes,
		entries:  make(map[SegmentKey]*segmentCacheEntry),
		changed:  make(chan struct{}),
	}, nil
}

// segmentLease holds the file descriptor returned to a caller. A publishing lease
// additionally protects the cache entry until waiting callers have opened their files.
type segmentLease struct {
	file        *os.File
	cache       *segmentCache
	key         SegmentKey
	protected   bool
	protectOnce sync.Once
	closeOnce   sync.Once
}

// releaseProtection allows the cache to unlink this path after its waiting callers
// have opened file descriptors. Already-open descriptors remain readable after unlink.
func (l *segmentLease) releaseProtection() {
	if l == nil || !l.protected {
		return
	}
	l.protectOnce.Do(func() {
		l.cache.release(l.key)
	})
}

// release closes the file and drops any publishing protection still held.
func (l *segmentLease) release() {
	if l == nil {
		return
	}
	l.releaseProtection()
	l.closeOnce.Do(func() {
		if l.file != nil {
			_ = l.file.Close()
		}
	})
}

func (c *segmentCache) acquire(key SegmentKey) (*segmentLease, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return nil, false, nil
	}
	file, err := os.Open(entry.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.removeEntryLocked(entry)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("opening cached segment: %w", err)
	}
	c.lru.MoveToFront(entry.lruElement)
	return &segmentLease{file: file}, true, nil
}

// publish hard-links a finished scratch file into the cache. If another writer won
// the same key, the existing file is used. The returned lease keeps the winner safe
// while waiting callers acquire their own leases.
func (c *segmentCache) publish(tempPath string, key SegmentKey) (*segmentLease, bool, error) {
	if err := key.validate(); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry := c.entries[key]; entry != nil {
		file, err := os.Open(entry.path)
		if err != nil {
			return nil, false, fmt.Errorf("opening winning cached segment: %w", err)
		}
		entry.leases++
		c.lru.MoveToFront(entry.lruElement)
		_ = os.Remove(tempPath)
		return &segmentLease{file: file, cache: c, key: key, protected: true}, false, nil
	}

	info, err := os.Stat(tempPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat finished segment: %w", err)
	}
	if info.Size() <= 0 {
		return nil, false, fmt.Errorf("finished segment %q is empty", tempPath)
	}
	if info.Size() > c.maxBytes {
		return nil, false, errSegmentCacheFull
	}

	dest := c.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return nil, false, fmt.Errorf("creating segment cache directory: %w", err)
	}
	createdLink := true
	if err := os.Link(tempPath, dest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, false, fmt.Errorf("linking segment into cache: %w", err)
		}
		createdLink = false
	}
	publishedInfo := info
	if !createdLink {
		publishedInfo, err = os.Stat(dest)
		if err != nil {
			return nil, false, fmt.Errorf("stat winning segment link: %w", err)
		}
	}

	if !c.evictForLocked(publishedInfo.Size()) {
		if createdLink {
			_ = os.Remove(dest)
		}
		return nil, false, errSegmentCacheFull
	}
	file, err := os.Open(dest)
	if err != nil {
		if createdLink {
			_ = os.Remove(dest)
		}
		return nil, false, fmt.Errorf("opening published segment: %w", err)
	}
	entry := &segmentCacheEntry{key: key, path: dest, size: publishedInfo.Size(), leases: 1}
	entry.lruElement = c.lru.PushFront(entry)
	c.entries[key] = entry
	c.used += entry.size
	c.signalChangedLocked()
	_ = os.Remove(tempPath)
	return &segmentLease{file: file, cache: c, key: key, protected: true}, createdLink, nil
}

func (c *segmentCache) pathFor(key SegmentKey) string {
	// Values are hex encoded so names cannot escape the owned cache root.
	return filepath.Join(c.root,
		fmt.Sprintf("%x", key.RecordingID),
		fmt.Sprintf("%x", key.Generation),
		fmt.Sprintf("%x", key.Profile),
		fmt.Sprintf("%x", key.Rendition),
		fmt.Sprintf("%020d.ts", key.Number))
}

func (c *segmentCache) evictForLocked(needed int64) bool {
	if needed < 0 || needed > c.maxBytes {
		return false
	}
	for needed > c.maxBytes-c.used {
		var victim *segmentCacheEntry
		for element := c.lru.Back(); element != nil; element = element.Prev() {
			candidate := element.Value.(*segmentCacheEntry)
			if candidate.leases == 0 {
				victim = candidate
				break
			}
		}
		if victim == nil {
			return false
		}
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
		c.removeEntryLocked(victim)
	}
	return true
}

func (c *segmentCache) removeEntryLocked(entry *segmentCacheEntry) {
	delete(c.entries, entry.key)
	c.lru.Remove(entry.lruElement)
	c.used -= entry.size
	c.signalChangedLocked()
}

func (c *segmentCache) release(key SegmentKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return
	}
	if entry.leases > 0 {
		entry.leases--
		c.signalChangedLocked()
	}
}

func (c *segmentCache) signalChangedLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *segmentCache) spaceChanged() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func (c *segmentCache) usedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}
