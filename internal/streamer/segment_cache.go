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
	key  SegmentKey
	path string
	size int64
	// leases は、このエントリから開いて、まだ Close していない記述子の数。
	leases int
	// removed は、索引と LRU から外れたことを表す。外れても、開いている記述子の分のバイトは
	// 最後の Close まで used に残る（unlink 済みでも tmpfs を使うため）。
	removed    bool
	lruElement *list.Element
}

// segmentCache は、1 つのファイルシステム上のファイルを持つ、バイト数上限のプロセス内 LRU。
//
// used が数えるのは、索引にあるエントリのバイトと、索引から外れたが記述子が開いているエントリの
// バイトの合計である。unlink 済みのファイルも最後の Close まで tmpfs に残るので、こう数えないと
// tmpfs の使用量が maxBytes を超える。読みを止めたクライアントが記述子を持ち続けると、新しい
// セグメントは空きを待つ。待つ期限を超えると 503 と Retry-After になる。
// 開いている記述子があるエントリは退避の候補にならない。
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

// SegmentFile は、キャッシュから開いたセグメントの記述子。Close するまで、そのバイトは
// キャッシュの使用量に数える（unlink 済みでも tmpfs に残るため）。
type SegmentFile struct {
	*os.File
	cache     *segmentCache
	entry     *segmentCacheEntry
	closeOnce sync.Once
}

// Close は記述子を閉じ、キャッシュの数を 1 つ減らす。2 回目以降の呼び出しは何もしない。
func (f *SegmentFile) Close() error {
	var err error
	f.closeOnce.Do(func() {
		err = f.File.Close()
		f.cache.closeFile(f.entry)
	})
	return err
}

// segmentLease は、呼び出し側へ返す記述子を持つ。
type segmentLease struct {
	file *SegmentFile
}

// release は記述子を閉じる。nil に対して呼んでも安全。
func (l *segmentLease) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
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
	entry.leases++
	c.lru.MoveToFront(entry.lruElement)
	return &segmentLease{file: &SegmentFile{File: file, cache: c, entry: entry}}, true, nil
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
		return &segmentLease{file: &SegmentFile{File: file, cache: c, entry: entry}}, false, nil
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
	return &segmentLease{file: &SegmentFile{File: file, cache: c, entry: entry}}, createdLink, nil
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

// removeEntryLocked は、エントリを索引と LRU から外す。開いている記述子が無ければ、そのバイトを
// used から引く。開いている記述子があれば、最後の closeFile まで used に残す。
func (c *segmentCache) removeEntryLocked(entry *segmentCacheEntry) {
	delete(c.entries, entry.key)
	c.lru.Remove(entry.lruElement)
	entry.removed = true
	if entry.leases == 0 {
		c.used -= entry.size
	}
	c.signalChangedLocked()
}

// closeFile は、entry から開いた記述子が閉じたことを記録する。キーではなくエントリで数えるので、
// 外されたエントリと同じキーの新しいエントリの数を減らすことは無い。
func (c *segmentCache) closeFile(entry *segmentCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.leases--
	if entry.removed && entry.leases == 0 {
		c.used -= entry.size
	}
	c.signalChangedLocked()
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

func (c *segmentCache) hasRecording(recordingID, generation string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.RecordingID == recordingID && key.Generation == generation {
			return true
		}
	}
	return false
}

func (c *segmentCache) removeRecording(recordingID, generation string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if key.RecordingID != recordingID || key.Generation != generation {
			continue
		}
		_ = os.Remove(entry.path)
		c.removeEntryLocked(entry)
	}
}
