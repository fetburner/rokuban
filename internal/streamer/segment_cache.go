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
	// errSegmentTooLarge は、セグメントがキャッシュ全体より大きく、空きを待っても入らないことを表す。
	errSegmentTooLarge  = errors.New("segment is larger than the whole cache")
	errSegmentNotCached = errors.New("segment is not cached")
)

// SegmentKey は、録画の 1 世代から作る不変の HLS セグメント 1 本を識別する。
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

// segmentCache は、1 つのファイルシステム上のファイルを持つ、バイト数上限のプロセス内 LRU。
// lease は、待っている要求がファイルを開いて配信を始めるまで、そのエントリを退避から守る。
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

// segmentLease は、呼び出し側へ返す記述子を持つ。登録した側の lease は、
// 待っている呼び出し側が自分のファイルを開くまで、キャッシュのエントリも守る。
type segmentLease struct {
	file        *os.File
	cache       *segmentCache
	key         SegmentKey
	protected   bool
	protectOnce sync.Once
	closeOnce   sync.Once
}

// releaseProtection は、待っている呼び出し側が記述子を開いた後に、キャッシュがこの path を
// unlink してよい状態にする。開いた記述子は unlink 後も読める。
func (l *segmentLease) releaseProtection() {
	if l == nil || !l.protected {
		return
	}
	l.protectOnce.Do(func() {
		l.cache.release(l.key)
	})
}

// release はファイルを閉じ、残っている登録側の保護を手放す。
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

// publish は、完成した scratch のファイルをハードリンクでキャッシュに登録する。同じ key を
// 別の書き手が先に登録していたら、既存のファイルを使い、こちらは捨てる。返す lease は、
// 待っている呼び出し側が自分の lease を取るまで、勝った方を守る。
// セグメントがキャッシュ全体より大きければ errSegmentTooLarge を返し、
// 空きが無ければ errSegmentCacheFull を返す。
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
		return nil, false, errSegmentTooLarge
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
