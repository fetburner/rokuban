package streamer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
)

const (
	// A five-segment read-ahead matches the initial window used by Jellyfin while
	// keeping one seek position from filling the scratch filesystem.
	defaultSegmentPrefetchSegments = 5
	// The initial 24-second sharing window follows Jellyfin's nearby-seek
	// tolerance; TestSegmentTransformWaitWindowIs24Seconds fixes its boundary.
	defaultSegmentWaitWindow = 24 * time.Second
	// A two-second seek pre-roll plus a PTS trim matched the synthetic GOP in
	// TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid; it is not a GOP bound.
	defaultSegmentSeekPreRoll = 2 * time.Second
	// Match live.idle_timeout's 30-second default for one HLS request burst, then
	// release the input fd and partial output instead of holding scratch forever.
	defaultSegmentTransformIdleTimeout = 30 * time.Second
	// Polling every 10 ms keeps the watcher responsive to playlist and segment
	// publication; TestSegmentTransformSIGSTOPAtPrefetchThenSIGCONTOnDemand covers
	// the resulting pause and resume path with real FFmpeg.
	defaultSegmentTransformPollInterval = 10 * time.Millisecond
	// MPEG-TS timestamps use a 90 kHz clock.
	mpegTSTimestampClock = int64(90_000)
	// Two active transforms bound CPU fan-out while allowing one other seek to
	// progress; callers can set a different limit in SegmentTransformOptions.
	defaultSegmentTransformConcurrency = 2
	segmentTransformRenditionCount     = 4
)

var (
	errSegmentManagerClosed    = errors.New("segment manager is closed")
	errSegmentTransformStopped = errors.New("segment transform stopped before requested segment was ready")
)

type transformPauseReason uint8

const (
	transformRunning transformPauseReason = iota
	transformPausedAhead
	transformPausedCapacity
	transformPausedCache
)

type segmentTransformGroup struct {
	recordingID string
	generation  string
	profile     string
}

type segmentTransformResult struct {
	file *os.File
	err  error
}

type segmentTransformWaiter struct {
	ctx context.Context
	ch  chan segmentTransformResult
}

// SegmentTransformInput describes one seekable recording source. OpenFile returns
// a descriptor for this transform; the child sees it as fd 3 when Path is empty.
// InputArgs is inserted before -i, so a growing-file source can add file-protocol
// options in a later integration without changing the transform manager.
type SegmentTransformInput struct {
	OpenFile  func(context.Context) (*os.File, error)
	Path      string
	InputArgs []string
	// OriginPTS is the first video PTS in 90 kHz MPEG-TS ticks.
	OriginPTS int64
	// FormatStartPTS is the input format start_time in 90 kHz MPEG-TS ticks.
	// Input-side -ss is relative to this value; keeping it explicit avoids
	// seeking from the oldest audio PES when its PTS predates the video.
	FormatStartPTS   int64
	AudioStreamCount int
}

type segmentTransformTask struct {
	group         segmentTransformGroup
	profile       LiveProfile
	input         SegmentTransformInput
	requestStart  int64
	startNumber   int64
	currentNumber int64
	maxRequested  int64
	lastDemand    time.Time
	pauseReason   transformPauseReason
	stopping      bool
	finished      bool
	cmd           *exec.Cmd
	wait          chan error
	done          chan struct{}
	workDir       string
	stderr        *tailBuffer
	waiters       map[SegmentKey][]*segmentTransformWaiter
}

// SegmentTransformOptions configures the in-memory cache and ffmpeg process pool.
// CacheBytes must exceed MaxConcurrent × (PrefetchSegments + 1) × four renditions ×
// MaxSegmentBytes so each transform can retain the requested segment and its
// read-ahead window across video and audio outputs.
type SegmentTransformOptions struct {
	Config           LiveConfig
	ScratchRoot      string
	CacheBytes       int64
	MaxSegmentBytes  int64
	MaxConcurrent    int
	PrefetchSegments int
	IdleTimeout      time.Duration
	PollInterval     time.Duration
}

// SegmentManager converts completed original recordings into fixed-length HLS
// segments and keeps those files in a byte-bounded process-local LRU.
type SegmentManager struct {
	mu               sync.Mutex
	cfg              LiveConfig
	root             string
	cache            *segmentCache
	maxSegmentBytes  int64
	maxConcurrent    int
	prefetchSegments int
	idleTimeout      time.Duration
	pollInterval     time.Duration
	ctx              context.Context
	cancel           context.CancelFunc
	tasks            map[segmentTransformGroup]map[*segmentTransformTask]struct{}
	running          int
	nextTaskID       atomic.Uint64
	closed           bool
}

// NewSegmentManager creates the process-local segment manager and clears only its
// owned scratch subdirectory before accepting requests.
func NewSegmentManager(opts SegmentTransformOptions) (*SegmentManager, error) {
	if opts.ScratchRoot == "" {
		return nil, errors.New("segment manager scratch root is empty")
	}
	if opts.CacheBytes <= 0 || opts.MaxSegmentBytes <= 0 {
		return nil, errors.New("segment manager cache and maximum segment bytes must be positive")
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = defaultSegmentTransformConcurrency
	}
	if opts.PrefetchSegments == 0 {
		opts.PrefetchSegments = defaultSegmentPrefetchSegments
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = defaultSegmentTransformIdleTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultSegmentTransformPollInterval
	}
	if opts.MaxConcurrent < 1 || opts.PrefetchSegments < 1 || opts.IdleTimeout < 0 || opts.PollInterval < time.Millisecond {
		return nil, errors.New("invalid segment transform concurrency, prefetch, idle timeout, or poll interval")
	}
	if int64(opts.PrefetchSegments) == math.MaxInt64 {
		return nil, errors.New("segment manager cache minimum overflows int64")
	}
	prefetchWindow := int64(opts.PrefetchSegments) + 1
	if int64(opts.MaxConcurrent) > math.MaxInt64/prefetchWindow {
		return nil, errors.New("segment manager cache minimum overflows int64")
	}
	concurrencyAndPrefetch := int64(opts.MaxConcurrent) * prefetchWindow
	if concurrencyAndPrefetch > math.MaxInt64/int64(segmentTransformRenditionCount) {
		return nil, errors.New("segment manager cache minimum overflows int64")
	}
	concurrencyAndPrefetch *= int64(segmentTransformRenditionCount)
	if concurrencyAndPrefetch > math.MaxInt64/opts.MaxSegmentBytes {
		return nil, errors.New("segment manager cache minimum overflows int64")
	}
	minimumCacheBytes := concurrencyAndPrefetch * opts.MaxSegmentBytes
	if opts.CacheBytes <= minimumCacheBytes {
		return nil, fmt.Errorf("segment cache byte limit %d must be greater than %d", opts.CacheBytes, minimumCacheBytes)
	}

	cfg := opts.Config
	cfg.FFmpeg = ffargs.FFmpegPath(cfg.FFmpeg)
	if len(cfg.Profiles) == 0 {
		return nil, errors.New("segment manager requires at least one profile")
	}
	for _, profile := range cfg.Profiles {
		if profile.Name == "" || profile.SegmentSeconds < 1 || profile.VideoCodec == "" || profile.AudioCodec == "" {
			return nil, fmt.Errorf("invalid segment transform profile: %#v", profile)
		}
	}
	root := filepath.Join(opts.ScratchRoot, "recording-hls")
	if err := os.MkdirAll(opts.ScratchRoot, 0o750); err != nil {
		return nil, fmt.Errorf("creating segment scratch parent: %w", err)
	}
	if err := clearDirectoryContents(root); err != nil {
		return nil, fmt.Errorf("clearing stale segment scratch: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o750); err != nil {
		return nil, fmt.Errorf("creating segment work root: %w", err)
	}
	cache, err := newSegmentCache(filepath.Join(root, "cache"), opts.CacheBytes)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SegmentManager{
		cfg:              cfg,
		root:             root,
		cache:            cache,
		maxSegmentBytes:  opts.MaxSegmentBytes,
		maxConcurrent:    opts.MaxConcurrent,
		prefetchSegments: opts.PrefetchSegments,
		idleTimeout:      opts.IdleTimeout,
		pollInterval:     opts.PollInterval,
		ctx:              ctx,
		cancel:           cancel,
		tasks:            make(map[segmentTransformGroup]map[*segmentTransformTask]struct{}),
	}, nil
}

// Get returns an open file for the requested cached segment. The caller owns the
// returned descriptor and may keep reading it after the cache unlinks its path.
func (m *SegmentManager) Get(ctx context.Context, key SegmentKey, input SegmentTransformInput) (*os.File, error) {
	if err := key.validate(); err != nil {
		return nil, err
	}
	if !supportedSegmentRendition(key.Rendition) {
		return nil, fmt.Errorf("unsupported segment rendition %q", key.Rendition)
	}
	if input.OpenFile == nil {
		return nil, errors.New("segment transform input has no file opener")
	}
	if input.AudioStreamCount < 1 {
		return nil, fmt.Errorf("segment transform audio stream count must be positive: %d", input.AudioStreamCount)
	}
	if ctx == nil {
		return nil, errors.New("segment request context is nil")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errSegmentManagerClosed
	}
	if lease, ok, err := m.cache.acquire(key); err != nil {
		m.mu.Unlock()
		return nil, err
	} else if ok {
		m.mu.Unlock()
		return lease.file, nil
	}

	profile, ok := m.cfg.profile(key.Profile)
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown segment transform profile %q", key.Profile)
	}
	group := segmentTransformGroup{recordingID: key.RecordingID, generation: key.Generation, profile: key.Profile}
	task := m.findTaskLocked(group, key.Number, profile.SegmentSeconds)
	waiter := &segmentTransformWaiter{ctx: ctx, ch: make(chan segmentTransformResult)}
	if task == nil {
		newTask, err := m.startTaskLocked(ctx, group, profile, input, key, waiter)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		task = newTask
	} else {
		task.waiters[key] = append(task.waiters[key], waiter)
		if key.Number > task.maxRequested {
			task.maxRequested = key.Number
		}
		task.lastDemand = time.Now()
		if task.pauseReason == transformPausedAhead || task.pauseReason == transformPausedCapacity {
			if err := m.resumeTaskLocked(task); err != nil {
				m.removeWaiterLocked(task, key, waiter)
				m.mu.Unlock()
				return nil, err
			}
		}
	}
	m.mu.Unlock()

	select {
	case result := <-waiter.ch:
		return result.file, result.err
	case <-ctx.Done():
		m.removeWaiter(task, key, waiter)
		return nil, ctx.Err()
	case <-m.ctx.Done():
		m.removeWaiter(task, key, waiter)
		return nil, errSegmentManagerClosed
	}
}

// Close stops child processes, waits for their watchers, and removes owned scratch files.
func (m *SegmentManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	var tasks []*segmentTransformTask
	for _, groupTasks := range m.tasks {
		for task := range groupTasks {
			tasks = append(tasks, task)
			if task.cmd != nil && task.cmd.Process != nil {
				_ = task.cmd.Process.Kill()
			}
		}
	}
	m.mu.Unlock()
	for _, task := range tasks {
		<-task.done
	}
	if err := clearDirectoryContents(m.root); err != nil {
		return fmt.Errorf("clearing segment scratch on close: %w", err)
	}
	return nil
}

func (m *SegmentManager) findTaskLocked(group segmentTransformGroup, number int64, segmentSeconds int) *segmentTransformTask {
	tasks := m.tasks[group]
	var best *segmentTransformTask
	bestDistance := int64(math.MaxInt64)
	for task := range tasks {
		if task.finished || task.stopping || number < task.currentNumber {
			continue
		}
		distance := number - task.currentNumber
		if distance > int64(defaultSegmentWaitWindow/time.Second)/int64(segmentSeconds) {
			continue
		}
		if distance < bestDistance {
			best, bestDistance = task, distance
		}
	}
	return best
}

func (m *SegmentManager) startTaskLocked(
	ctx context.Context,
	group segmentTransformGroup,
	profile LiveProfile,
	input SegmentTransformInput,
	key SegmentKey,
	waiter *segmentTransformWaiter,
) (*segmentTransformTask, error) {
	startNumber := key.Number
	if startNumber > 0 {
		startNumber--
	}
	id := m.nextTaskID.Add(1)
	workDir := filepath.Join(m.root, "work", strconv.FormatUint(id, 10))
	if err := os.MkdirAll(filepath.Join(workDir, "segments"), 0o750); err != nil {
		return nil, fmt.Errorf("creating transform scratch: %w", err)
	}
	args, err := buildSegmentTransformArgs(m.cfg, profile, input, startNumber, workDir)
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, err
	}
	file, err := input.OpenFile(ctx)
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("opening segment transform input: %w", err)
	}
	if file == nil {
		_ = os.RemoveAll(workDir)
		return nil, errors.New("segment transform input opener returned a nil file")
	}
	if err := m.makeRunningRoomLocked(nil); err != nil {
		_ = file.Close()
		_ = os.RemoveAll(workDir)
		return nil, err
	}
	stderr := &tailBuffer{limit: 16 * 1024}
	cmd := exec.Command(m.cfg.FFmpeg, args...)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		_ = os.RemoveAll(workDir)
		m.resumeOldestCapacityTaskLocked()
		return nil, fmt.Errorf("starting segment ffmpeg: %w", err)
	}
	_ = file.Close()

	now := time.Now()
	task := &segmentTransformTask{
		group:         group,
		profile:       profile,
		input:         input,
		requestStart:  key.Number,
		startNumber:   startNumber,
		currentNumber: startNumber,
		maxRequested:  key.Number,
		lastDemand:    now,
		cmd:           cmd,
		wait:          make(chan error, 1),
		done:          make(chan struct{}),
		workDir:       workDir,
		stderr:        stderr,
		waiters:       map[SegmentKey][]*segmentTransformWaiter{key: {waiter}},
	}
	if m.tasks[group] == nil {
		m.tasks[group] = make(map[*segmentTransformTask]struct{})
	}
	m.tasks[group][task] = struct{}{}
	m.running++
	go func() {
		task.wait <- cmd.Wait()
	}()
	go m.watch(task)
	return task, nil
}

func (m *SegmentManager) makeRunningRoomLocked(except *segmentTransformTask) error {
	if m.running < m.maxConcurrent {
		return nil
	}
	var oldest *segmentTransformTask
	for _, groupTasks := range m.tasks {
		for task := range groupTasks {
			if task == except || task.finished || task.stopping || task.pauseReason != transformRunning {
				continue
			}
			if oldest == nil || task.lastDemand.Before(oldest.lastDemand) {
				oldest = task
			}
		}
	}
	if oldest == nil {
		return errors.New("segment transform concurrency limit has no running process to pause")
	}
	if err := oldest.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return fmt.Errorf("pausing least-recently-requested transform: %w", err)
	}
	oldest.pauseReason = transformPausedCapacity
	m.running--
	return nil
}

func (m *SegmentManager) resumeTaskLocked(task *segmentTransformTask) error {
	if task.finished || task.stopping || task.pauseReason == transformRunning {
		return nil
	}
	if err := m.makeRunningRoomLocked(task); err != nil {
		return err
	}
	if err := task.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		m.resumeOldestCapacityTaskLocked()
		return fmt.Errorf("resuming segment transform: %w", err)
	}
	task.pauseReason = transformRunning
	m.running++
	return nil
}

func (m *SegmentManager) pauseTask(task *segmentTransformTask, reason transformPauseReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.finished || task.stopping || task.pauseReason != transformRunning {
		return
	}
	if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return
	}
	task.pauseReason = reason
	m.running--
}

func (m *SegmentManager) maybePauseForAhead(task *segmentTransformTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.finished || task.stopping || task.pauseReason != transformRunning {
		return
	}
	if task.currentNumber <= task.maxRequested+int64(m.prefetchSegments) {
		return
	}
	if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return
	}
	task.pauseReason = transformPausedAhead
	m.running--
}

func (m *SegmentManager) maybeResumeWaitingTasks() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for m.running < m.maxConcurrent && m.resumeOldestCapacityTaskLocked() {
	}
}

func (m *SegmentManager) resumeOldestCapacityTaskLocked() bool {
	var oldest *segmentTransformTask
	for _, groupTasks := range m.tasks {
		for task := range groupTasks {
			if task.finished || task.stopping || task.pauseReason != transformPausedCapacity {
				continue
			}
			if len(task.waiters) == 0 && time.Since(task.lastDemand) >= m.idleTimeout {
				continue
			}
			if oldest == nil || task.lastDemand.Before(oldest.lastDemand) {
				oldest = task
			}
		}
	}
	if oldest == nil || oldest.cmd.Process.Signal(syscall.SIGCONT) != nil {
		return false
	}
	oldest.pauseReason = transformRunning
	m.running++
	return true
}

func (m *SegmentManager) removeWaiter(task *segmentTransformTask, key SegmentKey, waiter *segmentTransformWaiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeWaiterLocked(task, key, waiter)
}

func (m *SegmentManager) removeWaiterLocked(task *segmentTransformTask, key SegmentKey, waiter *segmentTransformWaiter) {
	waiters := task.waiters[key]
	for i, candidate := range waiters {
		if candidate == waiter {
			task.waiters[key] = append(waiters[:i], waiters[i+1:]...)
			if len(task.waiters[key]) == 0 {
				delete(task.waiters, key)
			}
			return
		}
	}
}

func (m *SegmentManager) watch(task *segmentTransformTask) {
	defer close(task.done)
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-task.wait:
			success := waitErr == nil
			scanErr := m.scanTransform(task, success)
			if scanErr != nil && waitErr == nil {
				waitErr = scanErr
			}
			m.finishTask(task, waitErr)
			return
		case <-m.ctx.Done():
			if task.cmd.Process != nil {
				_ = task.cmd.Process.Kill()
			}
			waitErr := <-task.wait
			_ = m.scanTransform(task, false)
			m.finishTask(task, errors.Join(errSegmentManagerClosed, waitErr))
			return
		case <-ticker.C:
			if err := m.scanTransform(task, false); err != nil {
				m.mu.Lock()
				if !task.finished && !task.stopping {
					task.stopping = true
					if task.pauseReason == transformRunning {
						m.running--
						task.pauseReason = transformPausedCapacity
					}
					_ = task.cmd.Process.Kill()
				}
				m.mu.Unlock()
			}
			m.maybeStopAtCachedSegment(task)
			m.maybePauseForAhead(task)
			m.maybeStopIdleTask(task)
			m.maybeResumeWaitingTasks()
		}
	}
}

func (m *SegmentManager) scanTransform(task *segmentTransformTask, normalExit bool) error {
	entries := make(map[string][]int64, segmentTransformRenditionCount)
	endList := make(map[string]bool, segmentTransformRenditionCount)
	for _, rendition := range segmentRenditions {
		variant := segmentVariantForRendition[rendition]
		playlist := filepath.Join(task.workDir, task.profile.Name+"."+variant+".m3u8")
		segments, hasEndList, err := readSegmentPlaylist(playlist, task.profile.Name, variant)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		entries[rendition] = segments
		endList[rendition] = hasEndList
	}
	m.updateCurrentNumber(task, entries)
	m.mu.Lock()
	maxCacheNumber := task.maxRequested + int64(m.prefetchSegments)
	m.mu.Unlock()
	for _, rendition := range segmentRenditions {
		variant := segmentVariantForRendition[rendition]
		for _, number := range entries[rendition] {
			if number < task.requestStart || number > maxCacheNumber {
				continue
			}
			path := filepath.Join(task.workDir, "segments", fmt.Sprintf("%s.%s_seg%05d.ts", task.profile.Name, variant, number))
			nextPath := filepath.Join(task.workDir, "segments", fmt.Sprintf("%s.%s_seg%05d.ts", task.profile.Name, variant, number+1))
			nextStarted := fileExists(nextPath) || fileExists(nextPath+".tmp")
			isFinal := normalExit && endList[rendition]
			if !nextStarted && !isFinal {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue
			}
			key := SegmentKey{
				RecordingID: task.group.recordingID,
				Generation:  task.group.generation,
				Profile:     task.group.profile,
				Rendition:   rendition,
				Number:      number,
			}
			if err := m.publishCompleted(task, key, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *SegmentManager) updateCurrentNumber(task *segmentTransformTask, entries map[string][]int64) {
	highest := task.startNumber
	for _, variant := range segmentVariantForRendition {
		pattern := filepath.Join(task.workDir, "segments", task.profile.Name+"."+variant+"_seg*.ts*")
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			base := filepath.Base(match)
			number, ok := parseOutputSegmentNumber(base, task.profile.Name, variant)
			if ok && number > highest {
				highest = number
			}
		}
	}
	for _, numbers := range entries {
		for _, number := range numbers {
			if number+1 > highest {
				highest = number + 1
			}
		}
	}
	m.mu.Lock()
	if highest > task.currentNumber {
		task.currentNumber = highest
	}
	m.mu.Unlock()
}

func (m *SegmentManager) publishCompleted(task *segmentTransformTask, key SegmentKey, path string) error {
	if lease, ok, err := m.cache.acquire(key); err != nil {
		return err
	} else if ok {
		_ = lease.file.Close()
		_ = os.Remove(path)
		return nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() > m.maxSegmentBytes {
		return fmt.Errorf("segment %s is %d bytes, above configured maximum %d", path, info.Size(), m.maxSegmentBytes)
	}

	for {
		changed := m.cache.spaceChanged()
		lease, _, err := m.cache.publish(path, key)
		if errors.Is(err, errSegmentCacheFull) {
			m.pauseTask(task, transformPausedCache)
			select {
			case <-changed:
				continue
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		}
		if err != nil {
			return err
		}
		m.fulfillWaiters(task.group, key, lease)
		m.mu.Lock()
		if task.pauseReason == transformPausedCache && !task.finished && !task.stopping {
			_ = m.resumeTaskLocked(task)
		}
		m.mu.Unlock()
		return nil
	}
}

func (m *SegmentManager) fulfillWaiters(group segmentTransformGroup, key SegmentKey, publisher *segmentLease) {
	type delivery struct {
		waiter *segmentTransformWaiter
		file   *os.File
		err    error
	}
	var deliveries []delivery
	m.mu.Lock()
	if tasks := m.tasks[group]; tasks != nil {
		for task := range tasks {
			for _, waiter := range task.waiters[key] {
				if waiter.ctx.Err() != nil {
					continue
				}
				lease, ok, err := m.cache.acquire(key)
				if err != nil {
					deliveries = append(deliveries, delivery{waiter: waiter, err: err})
					continue
				}
				if !ok {
					deliveries = append(deliveries, delivery{waiter: waiter, err: errSegmentNotCached})
					continue
				}
				deliveries = append(deliveries, delivery{waiter: waiter, file: lease.file})
			}
			delete(task.waiters, key)
		}
	}
	m.mu.Unlock()
	publisher.release()
	for _, item := range deliveries {
		select {
		case item.waiter.ch <- segmentTransformResult{file: item.file, err: item.err}:
		case <-item.waiter.ctx.Done():
			_ = item.file.Close()
		case <-m.ctx.Done():
			_ = item.file.Close()
		}
	}
}

//nolint:gocognit // This lock-held transition moves every waiter before killing its process; splitting it obscures that order.
func (m *SegmentManager) maybeStopAtCachedSegment(task *segmentTransformTask) {
	m.mu.Lock()
	if task.finished || task.stopping || task.currentNumber < task.requestStart {
		m.mu.Unlock()
		return
	}
	current := task.currentNumber
	m.mu.Unlock()

	for _, rendition := range segmentRenditions {
		key := SegmentKey{
			RecordingID: task.group.recordingID,
			Generation:  task.group.generation,
			Profile:     task.group.profile,
			Rendition:   rendition,
			Number:      current,
		}
		lease, ok, err := m.cache.acquire(key)
		if err != nil || !ok {
			return
		}
		_ = lease.file.Close()
	}

	type pendingRequest struct {
		key     SegmentKey
		waiters []*segmentTransformWaiter
	}
	type delivery struct {
		waiter *segmentTransformWaiter
		file   *os.File
		err    error
	}
	var requests []pendingRequest
	var deliveries []delivery

	m.mu.Lock()
	if task.finished || task.stopping || task.currentNumber != current {
		m.mu.Unlock()
		return
	}
	for key, waiters := range task.waiters {
		requests = append(requests, pendingRequest{key: key, waiters: append([]*segmentTransformWaiter(nil), waiters...)})
	}
	task.waiters = make(map[SegmentKey][]*segmentTransformWaiter)
	task.stopping = true
	if task.pauseReason == transformRunning {
		m.running--
	}
	task.pauseReason = transformPausedCapacity
	_ = task.cmd.Process.Kill()

	sort.Slice(requests, func(i, j int) bool {
		if requests[i].key.Number == requests[j].key.Number {
			return requests[i].key.Rendition < requests[j].key.Rendition
		}
		return requests[i].key.Number < requests[j].key.Number
	})
	for _, request := range requests {
		var activeWaiters []*segmentTransformWaiter
		for _, waiter := range request.waiters {
			if waiter.ctx.Err() == nil {
				activeWaiters = append(activeWaiters, waiter)
			}
		}
		if len(activeWaiters) == 0 {
			continue
		}
		if lease, ok, err := m.cache.acquire(request.key); err != nil {
			for _, waiter := range activeWaiters {
				deliveries = append(deliveries, delivery{waiter: waiter, err: err})
			}
			continue
		} else if ok {
			_ = lease.file.Close()
			for _, waiter := range activeWaiters {
				fileLease, stillCached, openErr := m.cache.acquire(request.key)
				if openErr != nil {
					deliveries = append(deliveries, delivery{waiter: waiter, err: openErr})
				} else if !stillCached {
					deliveries = append(deliveries, delivery{waiter: waiter, err: errSegmentNotCached})
				} else {
					deliveries = append(deliveries, delivery{waiter: waiter, file: fileLease.file})
				}
			}
			continue
		}
		profile := task.profile
		next := m.findTaskLocked(task.group, request.key.Number, profile.SegmentSeconds)
		if next == nil {
			first := activeWaiters[0]
			var err error
			next, err = m.startTaskLocked(m.ctx, task.group, profile, task.input, request.key, first)
			if err != nil {
				for _, waiter := range activeWaiters {
					deliveries = append(deliveries, delivery{waiter: waiter, err: err})
				}
				continue
			}
			activeWaiters = activeWaiters[1:]
		} else {
			if request.key.Number > next.maxRequested {
				next.maxRequested = request.key.Number
			}
			next.lastDemand = time.Now()
		}
		next.waiters[request.key] = append(next.waiters[request.key], activeWaiters...)
		if next.pauseReason == transformPausedAhead || next.pauseReason == transformPausedCapacity {
			if err := m.resumeTaskLocked(next); err != nil {
				for _, waiter := range activeWaiters {
					m.removeWaiterLocked(next, request.key, waiter)
					deliveries = append(deliveries, delivery{waiter: waiter, err: err})
				}
			}
		}
	}
	m.mu.Unlock()

	for _, item := range deliveries {
		select {
		case item.waiter.ch <- segmentTransformResult{file: item.file, err: item.err}:
		case <-item.waiter.ctx.Done():
			if item.file != nil {
				_ = item.file.Close()
			}
		case <-m.ctx.Done():
			if item.file != nil {
				_ = item.file.Close()
			}
		}
	}
}

func (m *SegmentManager) maybeStopIdleTask(task *segmentTransformTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.finished || task.stopping || len(task.waiters) > 0 || time.Since(task.lastDemand) < m.idleTimeout {
		return
	}
	task.stopping = true
	if task.pauseReason == transformRunning {
		m.running--
		task.pauseReason = transformPausedCapacity
	}
	_ = task.cmd.Process.Kill()
}

func (m *SegmentManager) finishTask(task *segmentTransformTask, processErr error) {
	m.mu.Lock()
	if task.finished {
		m.mu.Unlock()
		return
	}
	task.finished = true
	if task.pauseReason == transformRunning {
		m.running--
	}
	task.pauseReason = transformRunning
	if groupTasks := m.tasks[task.group]; groupTasks != nil {
		delete(groupTasks, task)
		if len(groupTasks) == 0 {
			delete(m.tasks, task.group)
		}
	}
	var waiters []*segmentTransformWaiter
	for _, pending := range task.waiters {
		waiters = append(waiters, pending...)
	}
	task.waiters = make(map[SegmentKey][]*segmentTransformWaiter)
	m.mu.Unlock()

	var waiterErr error
	if processErr == nil {
		waiterErr = errSegmentNotCached
	} else if task.stopping && !errors.Is(processErr, errSegmentManagerClosed) {
		waiterErr = errSegmentTransformStopped
	} else {
		waiterErr = fmt.Errorf("segment ffmpeg exited: %w: %s", processErr, task.stderr.String())
	}
	for _, waiter := range waiters {
		select {
		case waiter.ch <- segmentTransformResult{err: waiterErr}:
		case <-waiter.ctx.Done():
		case <-m.ctx.Done():
		}
	}
	_ = os.RemoveAll(task.workDir)
	m.maybeResumeWaitingTasks()
}

var segmentRenditions = []string{"video", "audio-standard", "audio-main", "audio-sub"}

var segmentVariantForRendition = map[string]string{
	"video":          "0",
	"audio-standard": "1",
	"audio-main":     "2",
	"audio-sub":      "3",
}

func supportedSegmentRendition(rendition string) bool {
	_, ok := segmentVariantForRendition[rendition]
	return ok
}

func buildSegmentTransformArgs(
	cfg LiveConfig,
	profile LiveProfile,
	input SegmentTransformInput,
	startNumber int64,
	workDir string,
) ([]string, error) {
	if startNumber < 0 {
		return nil, fmt.Errorf("segment transform start number is negative: %d", startNumber)
	}
	if input.AudioStreamCount < 1 {
		return nil, fmt.Errorf("segment transform audio stream count must be positive: %d", input.AudioStreamCount)
	}
	if profile.SegmentSeconds < 1 {
		return nil, fmt.Errorf("segment transform segment duration must be positive: %d", profile.SegmentSeconds)
	}
	if startNumber > math.MaxInt64/int64(profile.SegmentSeconds)/mpegTSTimestampClock {
		return nil, errors.New("segment transform seek timestamp overflows int64")
	}
	deltaPTS := startNumber * int64(profile.SegmentSeconds) * mpegTSTimestampClock
	if input.OriginPTS > math.MaxInt64-deltaPTS {
		return nil, errors.New("segment transform seek timestamp overflows int64")
	}
	trimPTS := input.OriginPTS + deltaPTS
	seekPTS := input.OriginPTS
	preRollPTS := int64(defaultSegmentSeekPreRoll / time.Second * time.Duration(mpegTSTimestampClock))
	if deltaPTS > preRollPTS {
		seekPTS = trimPTS - preRollPTS
	}
	seekOffsetPTS, ok := subtractInt64(seekPTS, input.FormatStartPTS)
	if !ok {
		return nil, errors.New("segment transform seek offset overflows int64")
	}
	seekSeconds := strconv.FormatFloat(float64(seekOffsetPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)
	trimSeconds := strconv.FormatFloat(float64(trimPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)

	cfg.Profiles = []LiveProfile{profile}
	cfg.Captions = false
	inputPath := input.Path
	if inputPath == "" {
		inputPath = originalVODFFmpegInputPath
	}
	// Offset 1 is only a placeholder that makes the shared builder emit input-side
	// -ss; the measured pre-roll seek relative to format.start_time is installed below.
	args := buildHLSFFmpegArgsForPlaylistType(
		cfg, workDir, false, hlsOriginalEventPlaylist, inputPath, 1, input.AudioStreamCount,
	)
	var err error
	ssIndex := indexOf(args, "-ss")
	if ssIndex < 0 || ssIndex+1 >= len(args) {
		return nil, errors.New("segment HLS args do not contain input-side -ss")
	}
	args[ssIndex+1] = seekSeconds
	args = append(args[:ssIndex], append([]string{"-copyts"}, args[ssIndex:]...)...)
	if len(input.InputArgs) > 0 {
		inputIndex := indexOf(args, "-i")
		if inputIndex < 0 {
			return nil, errors.New("segment HLS args do not contain input -i")
		}
		args = append(args[:inputIndex], append(append([]string(nil), input.InputArgs...), args[inputIndex:]...)...)
	}
	args, err = appendSegmentOutputFilter(args, "-vf", "trim=start="+trimSeconds+",setpts=PTS-STARTPTS")
	if err != nil {
		return nil, err
	}
	for audioIndex := 0; audioIndex < 3; audioIndex++ {
		args, err = appendSegmentOutputFilter(
			args,
			"-filter:a:"+strconv.Itoa(audioIndex),
			"atrim=start="+trimSeconds+",asetpts=PTS-STARTPTS",
		)
		if err != nil {
			return nil, err
		}
	}
	listSize := indexOf(args, "-hls_list_size")
	if listSize < 0 || listSize+1 >= len(args) {
		return nil, errors.New("segment HLS args do not contain -hls_list_size")
	}
	args = append(args[:listSize+2], append([]string{"-start_number", strconv.FormatInt(startNumber, 10)}, args[listSize+2:]...)...)
	outputFormat := lastIndexOf(args, "-f")
	if outputFormat < 0 {
		return nil, errors.New("segment HLS args do not contain output -f")
	}
	args = append(args[:outputFormat], append([]string{
		"-muxpreload", "0", "-muxdelay", "0", "-output_ts_offset", trimSeconds,
	}, args[outputFormat:]...)...)
	return args, nil
}

func subtractInt64(left, right int64) (int64, bool) {
	if right > 0 && left < math.MinInt64+right || right < 0 && left > math.MaxInt64+right {
		return 0, false
	}
	return left - right, true
}

func appendSegmentOutputFilter(args []string, option, filter string) ([]string, error) {
	if index := indexOf(args, option); index >= 0 {
		if index+1 >= len(args) {
			return nil, fmt.Errorf("segment HLS args have no value after %s", option)
		}
		args[index+1] += "," + filter
		return args, nil
	}
	index := indexOf(args, "-force_key_frames")
	if index < 0 {
		return nil, errors.New("segment HLS args do not contain -force_key_frames")
	}
	return append(args[:index], append([]string{option, filter}, args[index:]...)...), nil
}

func indexOf(args []string, target string) int {
	for i, arg := range args {
		if arg == target {
			return i
		}
	}
	return -1
}

func lastIndexOf(args []string, target string) int {
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == target {
			return i
		}
	}
	return -1
}

func clearDirectoryContents(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readSegmentPlaylist(path, profile, variant string) ([]int64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	var segments []int64
	scanner := bufio.NewScanner(file)
	needURI := false
	endList := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "#EXT-X-ENDLIST" {
			endList = true
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			needURI = true
			continue
		}
		if !needURI || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if number, ok := parseOutputSegmentNumber(filepath.Base(line), profile, variant); ok {
			segments = append(segments, number)
		}
		needURI = false
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("reading HLS segment playlist: %w", err)
	}
	return segments, endList, nil
}

func parseOutputSegmentNumber(name, profile, variant string) (int64, bool) {
	prefix := profile + "." + variant + "_seg"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	numberText := strings.TrimPrefix(name, prefix)
	numberText = strings.TrimSuffix(numberText, ".tmp")
	numberText = strings.TrimSuffix(numberText, ".ts")
	number, err := strconv.ParseInt(numberText, 10, 64)
	return number, err == nil && number >= 0
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	if overflow := len(b.data) + len(p) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.data))
}
