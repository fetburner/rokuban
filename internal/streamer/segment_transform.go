package streamer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/store"
)

const (
	// defaultSegmentPrefetchSegments は、変換が要求済みの最大番号より先へ進める本数の初期値。
	// 5 本 × 2 秒 = 10 秒先まで作っておけば、連続再生が次々に数セグメント先を要求しても変換を止めずに
	// 追従でき、1 つの視聴位置が scratch に溜める量も (先読み + 1) 本 × 最大 5 rendition に収まる。
	// 測定して決めた値ではなく初期値であり、実運用の CPU と scratch の実測で見直す。
	defaultSegmentPrefetchSegments = 5
	// defaultSegmentWaitWindow は、変換中の位置 p に対して p ≤ m ≤ p + 窓 の要求を新しい変換を
	// 起動せずに待つ幅の初期値。Jellyfin の近傍シーク許容幅に倣った 24 秒で、境界は
	// TestSegmentTransformWaitWindowIs24Seconds が固定する。
	defaultSegmentWaitWindow = 24 * time.Second
	// defaultSegmentSeekPreRoll は、要求セグメントの何秒前から読み始めるか。
	// 2 秒は、合成 GOP（25 fps、GOP 50 フレーム、B フレーム無し、D=2 秒）で、pre-roll 無しの直接シーク
	// （絶対秒）の先頭が +41 frame ずれ、2 秒の pre-roll と trim でずれが 0 frame になった測定による。
	// 測ったのはこの合成 GOP だけで、実録画の GOP の上限ではない。
	// TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid（n=0..8、2 種類の TS）は pre-roll の有無を
	// 弁別しない（pre-roll 0 秒・1 秒でも通ることを確認した）。値そのものを固定するのは引数のテストだけである。
	defaultSegmentSeekPreRoll = 2 * time.Second
	// defaultSegmentTransformIdleTimeout は、待たれていない変換を残す時間。live.idle_timeout の
	// 既定 30 秒に合わせ、HLS の 1 回の要求の連なりが途切れたら入力 fd と書きかけの出力を手放す。
	defaultSegmentTransformIdleTimeout = 30 * time.Second
	// defaultSegmentTransformPollInterval は playlist と出力ファイルの監視間隔。
	// TestSegmentTransformSIGSTOPAtPrefetchThenSIGCONTOnDemand が止めて再開する経路を実 ffmpeg で通す。
	defaultSegmentTransformPollInterval = 10 * time.Millisecond
	// mpegTSTimestampClock は MPEG-TS の 90 kHz クロック。
	mpegTSTimestampClock = int64(90_000)
	// defaultSegmentTransformConcurrency は生きている ffmpeg の同時数の初期値。一時停止中も数える。
	// 2 本なら、1 本が先読みで止まっていても別の視聴位置をもう 1 本進められる。
	defaultSegmentTransformConcurrency = 2
	segmentTransformRenditionCount     = 5
)

var (
	errSegmentManagerClosed     = errors.New("segment manager is closed")
	errSegmentTransformStopped  = errors.New("segment transform stopped before requested segment was ready")
	errSegmentGrowingInputRetry = errors.New("growing segment input ended before the requested segment was ready")
	// errSegmentRetry は、待っていた変換が止められたので Get が最初からやり直すための内部の印。
	errSegmentRetry = errors.New("segment request must be retried")
	// errSegmentBeyondEnd は、録画の最後のセグメントより後ろを要求したときのエラー。
	errSegmentBeyondEnd = errors.New("segment number is beyond the end of the recording")
)

type transformPauseReason uint8

const (
	transformRunning transformPauseReason = iota
	transformPausedAhead
	transformPausedCache
)

type segmentTransformGroup struct {
	recordingID string
	generation  string
	profile     string
	inputKind   store.RecordingHLSInputKind
}

type segmentTransformResult struct {
	file *os.File
	err  error
}

type segmentTransformWaiter struct {
	ctx context.Context
	ch  chan segmentTransformResult
}

// SegmentTransformInput は、シークできる録画 1 本の入力を表す。
// OpenFile は変換ごとに記述子を返し、Path が空なら子プロセスが fd 3 として読む。
// InputArgs は -i の前に入るので、伸びるファイルの入力は変換の管理を変えずに
// file プロトコルの引数を足せる。
type SegmentTransformInput struct {
	Kind      store.RecordingHLSInputKind
	OpenFile  func(context.Context) (*os.File, error)
	Path      string
	InputArgs []string
	// OriginPTS は最初の映像 PTS（90 kHz の MPEG-TS tick）。
	OriginPTS int64
	// VideoDurationPTS は原点から録画の末尾までの映像の長さ（90 kHz tick）。0 は未確定を表す。
	// 確定しているとき（原本）は、次のように扱う。
	//   - 音声には apad と atrim で映像の終端までを出させるので、全 rendition が映像と同じ番号で終わる。
	//   - rendition ごとの ENDLIST と最後のファイルで、その rendition の短い末尾を完成とみなす。
	//     ただし音声と字幕を完成とみなすのは、同じ変換の映像が ENDLIST 付きで最後の番号に届いたときだけ。
	//   - 音声と字幕は、同じ変換の映像が出し終えた最後の番号（videoLast）までしか公開しない。
	//     入力が途中で切れると、音声は映像の無い区間まで apad の無音で進むためである。
	// 未確定のとき（伸びる temp）は ENDLIST を根拠に最後扱いしない（入力が途中で EOF 扱いになっても
	// ffmpeg は終了コード 0 で ENDLIST を書くため）。
	// 変換の進み（updateCurrentNumber）は、どちらの場合も映像だけで数える。
	VideoDurationPTS int64
	// VideoLastFramePTS は最後のフレームの開始時刻（原点からの 90 kHz tick）。VideoDurationPTS と同時に設定する。
	// 最後のセグメント番号は、この値の属する格子で決まる。終端の格子に開始するフレームが無い短い行は作れないので載せない。
	VideoLastFramePTS int64
	AudioStreamCount  int
	// HasSubtitles は captions 設定が有効な入力に WebVTT rendition があることを表す。
	HasSubtitles bool
}

// finalSegmentNumber は、映像の長さと最後のフレームの開始時刻（tick）から最後のセグメント番号を返す。未確定は -1。
// 最後のフレームの開始時刻が属する格子を最後とする。そこには必ずフレームが始まるので、変換は作れる。
func finalSegmentNumber(durationPTS, lastFramePTS int64, segmentSeconds int) int64 {
	if durationPTS <= 0 || lastFramePTS < 0 || lastFramePTS >= durationPTS || segmentSeconds < 1 {
		return -1
	}
	segmentPTS := int64(segmentSeconds) * mpegTSTimestampClock
	return lastFramePTS / segmentPTS
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
	stop          chan struct{}
	workDir       string
	stderr        *tailBuffer
	waiters       map[SegmentKey][]*segmentTransformWaiter
}

// SegmentTransformOptions は、メモリ上のキャッシュと ffmpeg の同時実行を設定する。
// CacheBytes は MaxConcurrent × (PrefetchSegments + 1) × 最大 5 rendition × MaxSegmentBytes
// より大きくする。要求されたセグメントと先読み分を、映像・音声・字幕の出力ぶん保持できる下限である。
// MaxSegmentBytes はこの下限の見積もりにだけ使い、実際のセグメントが超えても配信は止めない
// （キャッシュ全体より大きいときだけ、待たずにエラーを返す）。
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

// SegmentManager は、録画入力を固定長の HLS セグメントへ変換し、
// そのファイルをバイト数上限の LRU に保つ。
//
// 同時に生きている ffmpeg は、SIGSTOP で止めているものも含めて MaxConcurrent 本までである。
// 上限に当たった新しい要求は、待っている要求（waiter）を持たない変換のうち、最後に待たれた
// 時刻が最も古いものを kill して空きを作る。kill は進捗を捨てるので、再起動のたびに pre-roll 分の
// 再デコードが増える（増える量は未測定）。waiter を持つ変換は kill しない。上限ぎりぎりで 2 人の
// 視聴者が交互に要求すると、互いの変換を kill し合って、どちらも進まなくなるからである。
// 全部の変換に waiter がいるときは kill せず、空きが出るまで Get が呼び出し側の ctx の期限まで待つ。
// 期限が切れたら ctx のエラーを返すので、HTTP 層はそれを 503 と Retry-After にする。
//
// 先読みで止める SIGSTOP/SIGCONT は、libx264 のソフトウェアエンコードだけで確認している
// （TestSegmentTransformResumeAfterSIGSTOPUpdatesPlaylistPromptly）。ハードウェアエンコーダでの
// SIGSTOP は未検証である。
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
	// capacityCh は、変換が終わった・waiter が空になったときに閉じて作り直す。
	// 上限で待っている Get がこれで起きる。
	capacityCh chan struct{}
	nextTaskID atomic.Uint64
	closed     bool
}

// NewSegmentManager は、プロセス内のセグメント管理を作る。
// 要求を受ける前に、自分の scratch 配下だけを掃除する。
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
		capacityCh:       make(chan struct{}),
	}, nil
}

// Get は、キャッシュ済みのセグメントを開いたファイルとして返す。
// 返した記述子は呼び出し側のもので、キャッシュが path を unlink した後も読み続けられる。
// 録画の最後より後ろの番号は、待たずにすぐエラーを返す。
func (m *SegmentManager) Get(ctx context.Context, key SegmentKey, input SegmentTransformInput) (*os.File, error) {
	file, _, err := m.GetWithCacheStatus(ctx, key, input)
	return file, err
}

// GetWithCacheStatus はセグメントを返し、要求時点でキャッシュにあったかを示す。
func (m *SegmentManager) GetWithCacheStatus(ctx context.Context, key SegmentKey, input SegmentTransformInput) (*os.File, bool, error) {
	if err := key.validate(); err != nil {
		return nil, false, err
	}
	if !supportedSegmentRendition(key.Rendition) {
		return nil, false, fmt.Errorf("unsupported segment rendition %q", key.Rendition)
	}
	if input.OpenFile == nil {
		return nil, false, errors.New("segment transform input has no file opener")
	}
	if input.AudioStreamCount < 1 {
		return nil, false, fmt.Errorf("segment transform audio stream count must be positive: %d", input.AudioStreamCount)
	}
	if ctx == nil {
		return nil, false, errors.New("segment request context is nil")
	}
	profile, ok := m.cfg.profile(key.Profile)
	if !ok {
		return nil, false, fmt.Errorf("unknown segment transform profile %q", key.Profile)
	}
	if final := finalSegmentNumber(input.VideoDurationPTS, input.VideoLastFramePTS, profile.SegmentSeconds); final >= 0 && key.Number > final {
		return nil, false, fmt.Errorf("%w: segment %d, last segment %d", errSegmentBeyondEnd, key.Number, final)
	}
	for {
		file, cached, err := m.getOnce(ctx, key, profile, input)
		if errors.Is(err, errSegmentRetry) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, false, ctxErr
			}
			continue
		}
		return file, cached, err
	}
}

func (m *SegmentManager) getOnce(
	ctx context.Context,
	key SegmentKey,
	profile LiveProfile,
	input SegmentTransformInput,
) (*os.File, bool, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, errSegmentManagerClosed
	}
	group := segmentTransformGroup{
		recordingID: key.RecordingID, generation: key.Generation, profile: key.Profile,
		inputKind: input.Kind,
	}
	if lease, ok, err := m.cache.acquire(key); err != nil {
		m.mu.Unlock()
		return nil, false, err
	} else if ok {
		m.touchTasksForCachedSegmentLocked(group, key.Number)
		m.mu.Unlock()
		return lease.file, true, nil
	}

	task := m.findTaskLocked(group, key.Number, profile.SegmentSeconds)
	waiter := &segmentTransformWaiter{ctx: ctx, ch: make(chan segmentTransformResult)}
	if task == nil {
		if !m.makeRoomLocked() {
			capacity := m.capacityCh
			m.mu.Unlock()
			select {
			case <-capacity:
				return nil, false, errSegmentRetry
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-m.ctx.Done():
				return nil, false, errSegmentManagerClosed
			}
		}
		newTask, err := m.startTaskLocked(ctx, group, profile, input, key, waiter)
		if err != nil {
			m.mu.Unlock()
			return nil, false, err
		}
		task = newTask
	} else {
		task.waiters[key] = append(task.waiters[key], waiter)
		if err := m.touchTaskLocked(task, key.Number); err != nil {
			m.removeWaiterLocked(task, key, waiter)
			m.mu.Unlock()
			return nil, false, err
		}
	}
	m.mu.Unlock()

	select {
	case result := <-waiter.ch:
		return result.file, false, result.err
	case <-ctx.Done():
		m.removeWaiter(task, key, waiter)
		return nil, false, ctx.Err()
	case <-m.ctx.Done():
		m.removeWaiter(task, key, waiter)
		return nil, false, errSegmentManagerClosed
	}
}

// StopRecording は録画世代の変換を止め、その世代のキャッシュを破棄する。
func (m *SegmentManager) StopRecording(recordingID, generation string) {
	m.mu.Lock()
	for group, tasks := range m.tasks {
		if group.recordingID != recordingID || group.generation != generation {
			continue
		}
		for task := range tasks {
			m.stopTaskLocked(task)
		}
	}
	m.cache.removeRecording(recordingID, generation)
	m.mu.Unlock()
}

// HasRecording は世代のキャッシュまたは変換が残っているかを返す。
func (m *SegmentManager) HasRecording(recordingID, generation string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for group, tasks := range m.tasks {
		if group.recordingID != recordingID || group.generation != generation {
			continue
		}
		for task := range tasks {
			if !task.finished {
				return true
			}
		}
	}
	return m.cache.hasRecording(recordingID, generation)
}

// Stats はキャッシュ使用量と生きている変換数を返す。
func (m *SegmentManager) Stats() (cacheBytes int64, activeTransforms int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tasks := range m.tasks {
		for task := range tasks {
			if !task.finished {
				activeTransforms++
			}
		}
	}
	return m.cache.usedBytes(), activeTransforms
}

// Close は、子プロセスを止め、監視の終了を待ち、自分の scratch を消す。
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

// findTaskLocked は、number を待てる変換を返す。
// 待てる下限は max(requestStart, currentNumber-1) である。currentNumber-1 は次のセグメントが
// 始まった（完成済みで、この変換がもうすぐ公開する）番号で、先読みで止まった変換の最後の 1 本を
// 含む。requestStart より前（pre-roll で作って捨てる n-1）は待たずに新しい変換へ回す。
// 待つ幅は currentNumber からの距離で測る。
func (m *SegmentManager) findTaskLocked(group segmentTransformGroup, number int64, segmentSeconds int) *segmentTransformTask {
	tasks := m.tasks[group]
	var best *segmentTransformTask
	bestDistance := int64(math.MaxInt64)
	for task := range tasks {
		if task.finished || task.stopping {
			continue
		}
		if number < max(task.requestStart, task.currentNumber-1) {
			continue
		}
		distance := max(number-task.currentNumber, 0)
		if distance > int64(defaultSegmentWaitWindow/time.Second)/int64(segmentSeconds) {
			continue
		}
		if distance < bestDistance {
			best, bestDistance = task, distance
		}
	}
	return best
}

// makeRoomLocked は、新しい ffmpeg を起動できるなら true を返す。
// 生きている（finished でない）変換が上限未満のときだけ true である。
// 上限に当たっていて、止め中でない変換も上限以上あるなら、waiter を持たない変換のうち
// lastDemand が最も古いものを止める。止めた変換が終わるまで空きは出ないので、呼び出し側は
// capacityCh で待って最初からやり直す。
func (m *SegmentManager) makeRoomLocked() bool {
	live, active := 0, 0
	var victim *segmentTransformTask
	for _, groupTasks := range m.tasks {
		for task := range groupTasks {
			if task.finished {
				continue
			}
			live++
			if task.stopping {
				continue
			}
			active++
			if len(task.waiters) == 0 && (victim == nil || task.lastDemand.Before(victim.lastDemand)) {
				victim = task
			}
		}
	}
	if live < m.maxConcurrent {
		return true
	}
	if active >= m.maxConcurrent && victim != nil {
		m.stopTaskLocked(victim)
	}
	return false
}

// stopTaskLocked は変換を止め中にして kill する。
func (m *SegmentManager) stopTaskLocked(task *segmentTransformTask) {
	if task.finished || task.stopping {
		return
	}
	task.stopping = true
	if task.stop != nil {
		close(task.stop)
	}
	_ = task.cmd.Process.Kill()
}

func (m *SegmentManager) signalCapacityLocked() {
	close(m.capacityCh)
	m.capacityCh = make(chan struct{})
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
	stderr := &tailBuffer{limit: 16 * 1024}
	cmd := exec.Command(m.cfg.FFmpeg, args...)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("starting segment ffmpeg: %w", err)
	}
	_ = file.Close()

	task := &segmentTransformTask{
		group:         group,
		profile:       profile,
		input:         input,
		requestStart:  key.Number,
		startNumber:   startNumber,
		currentNumber: startNumber,
		maxRequested:  key.Number,
		lastDemand:    time.Now(),
		cmd:           cmd,
		wait:          make(chan error, 1),
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
		workDir:       workDir,
		stderr:        stderr,
		waiters:       map[SegmentKey][]*segmentTransformWaiter{key: {waiter}},
	}
	if m.tasks[group] == nil {
		m.tasks[group] = make(map[*segmentTransformTask]struct{})
	}
	m.tasks[group][task] = struct{}{}
	go func() {
		task.wait <- cmd.Wait()
	}()
	go m.watch(task)
	return task, nil
}

// touchTaskLocked は、number が要求されたことを変換に伝える。
// 先読みで止まっている変換は、要求で先読みの範囲に入ったときだけ SIGCONT で再開する。
func (m *SegmentManager) touchTaskLocked(task *segmentTransformTask, number int64) error {
	if number > task.maxRequested {
		task.maxRequested = number
	}
	task.lastDemand = time.Now()
	if task.finished || task.stopping || task.pauseReason != transformPausedAhead {
		return nil
	}
	if task.currentNumber > task.maxRequested+int64(m.prefetchSegments) {
		return nil
	}
	return m.resumeTaskLocked(task)
}

// touchTasksForCachedSegmentLocked は、キャッシュに当たった要求を、その番号を作った・
// 作っている範囲の変換に伝える。これが無いと、視聴者がキャッシュ済みの範囲を読み進める間、
// 先読みで止めた変換が再開されず、キャッシュの先端に着いた視聴者が毎回待たされる。
func (m *SegmentManager) touchTasksForCachedSegmentLocked(group segmentTransformGroup, number int64) {
	for task := range m.tasks[group] {
		if task.finished || task.stopping || number < task.requestStart || number > task.currentNumber {
			continue
		}
		_ = m.touchTaskLocked(task, number)
	}
}

func (m *SegmentManager) resumeTaskLocked(task *segmentTransformTask) error {
	if task.finished || task.stopping || task.pauseReason == transformRunning {
		return nil
	}
	if err := task.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		return fmt.Errorf("resuming segment transform: %w", err)
	}
	task.pauseReason = transformRunning
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
				if len(task.waiters) == 0 {
					m.signalCapacityLocked()
				}
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
				m.stopTaskLocked(task)
				m.mu.Unlock()
			}
			m.maybeStopAtCachedSegment(task)
			m.maybePauseForAhead(task)
			m.maybeStopIdleTask(task)
		}
	}
}

func (m *SegmentManager) scanTransform(task *segmentTransformTask, normalExit bool) error {
	entries := make(map[string][]int64, segmentTransformRenditionCount)
	endList := make(map[string]bool, segmentTransformRenditionCount)
	renditions := segmentRenditionsForInput(task.input)
	for _, rendition := range renditions {
		var playlist string
		var segments []int64
		var hasEndList bool
		var err error
		if rendition == "subtitles" {
			playlist = filepath.Join(task.workDir, "subtitles_0.m3u8")
			segments, hasEndList, err = readSubtitleSegmentPlaylist(playlist)
		} else {
			variant := segmentVariantForRendition[rendition]
			prefix := task.profile.Name
			playlistName := prefix + "." + variant + ".m3u8"
			if m.cfg.Captions {
				prefix = ""
				playlistName = "playlist_" + variant + ".m3u8"
			}
			playlist = filepath.Join(task.workDir, playlistName)
			segments, hasEndList, err = readSegmentPlaylist(playlist, prefix, variant)
		}
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
	finalNumber := finalSegmentNumber(task.input.VideoDurationPTS, task.input.VideoLastFramePTS, task.profile.SegmentSeconds)
	videoLast := int64(-1)
	if video := entries["video"]; len(video) > 0 {
		videoLast = video[len(video)-1]
	}
	videoReached := finalNumber >= 0 && endList["video"] && videoLast == finalNumber
	for _, rendition := range renditions {
		variant := segmentVariantForRendition[rendition]
		for _, number := range entries[rendition] {
			if number > maxCacheNumber {
				continue
			}
			var path, nextPath string
			if rendition == "subtitles" {
				path = filepath.Join(task.workDir, fmt.Sprintf("seg%d.vtt", number))
				nextPath = filepath.Join(task.workDir, fmt.Sprintf("seg%d.vtt", number+1))
			} else {
				prefix := task.profile.Name + "."
				if m.cfg.Captions {
					prefix = ""
				}
				path = filepath.Join(task.workDir, "segments", fmt.Sprintf("%s%s_seg%05d.ts", prefix, variant, number))
				nextPath = filepath.Join(task.workDir, "segments", fmt.Sprintf("%s%s_seg%05d.ts", prefix, variant, number+1))
			}
			nextStarted := fileExists(nextPath) || fileExists(nextPath+".tmp")
			if number < task.requestStart {
				// pre-roll のセグメントはキャッシュに入れない。完成したら scratch から消す。
				if nextStarted {
					_ = os.Remove(path)
				}
				continue
			}
			// 長さが確定した入力では、映像が先に進んだ番号までしか、音声と字幕を公開しない。
			// 入力が途中で切れても、音声は apad で映像の終端まで無音で埋まる。映像が届いていない区間の
			// 無音を、immutable のキャッシュに入れないためである。
			if finalNumber >= 0 && rendition != "video" && number > videoLast {
				continue
			}
			// 長さが確定した入力だけ、rendition ごとの ENDLIST と最後のファイルで完成とみなす。
			// 映像の番号と比べると、映像より 1 つ手前で終わる音声や字幕の末尾が公開されない。
			// 映像は長さの根拠なので、同じ変換の映像が確定した最後の番号に届いて ENDLIST を書いた後だけ、
			// どの rendition も完成とみなす。
			isLast := number == entries[rendition][len(entries[rendition])-1]
			isFinal := normalExit && videoReached && endList[rendition] && isLast
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
	// 映像の進みだけを数える。長さが確定した入力では、音声が apad で映像の終端より先まで無音で進む。
	// 伸びる入力でも、CPU 負荷下で音声が映像より 1 本先行した状態を観測した（原因は未検証）。
	// 音声の進みで「先読み済み」と判断して止めると、要求の番号の映像が公開されないまま止まる。その要求は
	// findTaskLocked の下限で止めた変換を候補から外し、別の ffmpeg を起動して止めた変換を追い出す
	// （負荷下の TestSegmentTransformCachedRequestResumesPrefetchPausedTransformWithRealFFmpeg で再現）。
	variant := segmentVariantForRendition["video"]
	prefix := task.profile.Name + "."
	profileName := task.profile.Name
	if m.cfg.Captions {
		prefix = ""
		profileName = ""
	}
	matches, _ := filepath.Glob(filepath.Join(task.workDir, "segments", prefix+variant+"_seg*.ts*"))
	for _, match := range matches {
		number, ok := parseOutputSegmentNumber(filepath.Base(match), profileName, variant)
		if ok && number > highest {
			highest = number
		}
	}
	for _, number := range entries["video"] {
		if number+1 > highest {
			highest = number + 1
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

	for {
		changed := m.cache.spaceChanged()
		lease, _, err := m.cache.publish(path, key)
		if errors.Is(err, errSegmentTooLarge) {
			// キャッシュ全体より大きいセグメントは、空きを待っても入らない。待たせずに waiter へ返す。
			_ = os.Remove(path)
			m.deliverKey(task.group, key, nil, err)
			return nil
		}
		if errors.Is(err, errSegmentCacheFull) {
			m.pauseTask(task, transformPausedCache)
			select {
			case <-changed:
				continue
			case <-task.stop:
				return errSegmentTransformStopped
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		}
		if err != nil {
			return err
		}
		m.deliverKey(task.group, key, lease, nil)
		m.mu.Lock()
		if task.pauseReason == transformPausedCache && !task.finished && !task.stopping {
			_ = m.resumeTaskLocked(task)
		}
		m.mu.Unlock()
		return nil
	}
}

// deliver は結果を waiter へ渡す。waiter が先に諦めていたら、渡すはずだったファイルを閉じる。
func (m *SegmentManager) deliver(waiter *segmentTransformWaiter, result segmentTransformResult) {
	select {
	case waiter.ch <- result:
	case <-waiter.ctx.Done():
		if result.file != nil {
			_ = result.file.Close()
		}
	case <-m.ctx.Done():
		if result.file != nil {
			_ = result.file.Close()
		}
	}
}

// deliverKey は key を待っている全 waiter に、publisher の lease から開いたファイルか failure を渡す。
func (m *SegmentManager) deliverKey(group segmentTransformGroup, key SegmentKey, publisher *segmentLease, failure error) {
	type delivery struct {
		waiter *segmentTransformWaiter
		result segmentTransformResult
	}
	var deliveries []delivery
	m.mu.Lock()
	for task := range m.tasks[group] {
		for _, waiter := range task.waiters[key] {
			if waiter.ctx.Err() != nil {
				continue
			}
			if failure != nil {
				deliveries = append(deliveries, delivery{waiter, segmentTransformResult{err: failure}})
				continue
			}
			lease, ok, err := m.cache.acquire(key)
			switch {
			case err != nil:
				deliveries = append(deliveries, delivery{waiter, segmentTransformResult{err: err}})
			case !ok:
				deliveries = append(deliveries, delivery{waiter, segmentTransformResult{err: errSegmentNotCached}})
			default:
				deliveries = append(deliveries, delivery{waiter, segmentTransformResult{file: lease.file}})
			}
		}
		if _, had := task.waiters[key]; had {
			delete(task.waiters, key)
			if len(task.waiters) == 0 {
				m.signalCapacityLocked()
			}
		}
	}
	m.mu.Unlock()
	publisher.release()
	for _, item := range deliveries {
		m.deliver(item.waiter, item.result)
	}
}

// maybeStopAtCachedSegment は、変換の現在位置が全レンディションともキャッシュ済みなら変換を止める。
// 待っていた要求には errSegmentRetry を返し、Get が最初からやり直す
// （キャッシュに当たるか、空きができ次第、新しい変換が起動する）。
func (m *SegmentManager) maybeStopAtCachedSegment(task *segmentTransformTask) {
	m.mu.Lock()
	if task.finished || task.stopping || task.currentNumber < task.requestStart {
		m.mu.Unlock()
		return
	}
	current := task.currentNumber
	m.mu.Unlock()

	for _, rendition := range segmentRenditionsForInput(task.input) {
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

	m.mu.Lock()
	if task.finished || task.stopping || task.currentNumber != current {
		m.mu.Unlock()
		return
	}
	// 現在位置より前の番号を待っている要求は、この変換がもうすぐ作る（playlist の更新が遅れているだけ）。
	// ここで止めると、他の変換が先に作った位置と競って、要求した番号を永久に作れなくなる。
	for key := range task.waiters {
		if key.Number < current {
			m.mu.Unlock()
			return
		}
	}
	var waiters []*segmentTransformWaiter
	for _, pending := range task.waiters {
		waiters = append(waiters, pending...)
	}
	task.waiters = make(map[SegmentKey][]*segmentTransformWaiter)
	m.stopTaskLocked(task)
	m.mu.Unlock()
	for _, waiter := range waiters {
		m.deliver(waiter, segmentTransformResult{err: errSegmentRetry})
	}
}

func (m *SegmentManager) maybeStopIdleTask(task *segmentTransformTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.finished || task.stopping || len(task.waiters) > 0 || time.Since(task.lastDemand) < m.idleTimeout {
		return
	}
	m.stopTaskLocked(task)
}

func (m *SegmentManager) finishTask(task *segmentTransformTask, processErr error) {
	m.mu.Lock()
	if task.finished {
		m.mu.Unlock()
		return
	}
	task.finished = true
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
	m.signalCapacityLocked()
	m.mu.Unlock()

	var waiterErr error
	if processErr == nil {
		waiterErr = errSegmentNotCached
	} else if task.stopping && !errors.Is(processErr, errSegmentManagerClosed) {
		waiterErr = errSegmentTransformStopped
	} else if task.input.Kind == store.RecordingHLSInputTemp {
		slog.Warn("streamer: growing recording HLS transform ended before the requested segment was ready", "recording_id", task.group.recordingID, "generation", task.group.generation, "profile", task.group.profile, "err", processErr, "stderr", task.stderr.String())
		waiterErr = fmt.Errorf("%w: %w", errSegmentGrowingInputRetry, processErr)
	} else {
		waiterErr = fmt.Errorf("segment ffmpeg exited: %w: %s", processErr, task.stderr.String())
	}
	for _, waiter := range waiters {
		m.deliver(waiter, segmentTransformResult{err: waiterErr})
	}
	_ = os.RemoveAll(task.workDir)
}

var segmentRenditions = []string{"video", "audio-standard", "audio-main", "audio-sub"}

var segmentVariantForRendition = map[string]string{
	"video":          "0",
	"audio-standard": "1",
	"audio-main":     "2",
	"audio-sub":      "3",
}

func supportedSegmentRendition(rendition string) bool {
	if rendition == "subtitles" {
		return true
	}
	_, ok := segmentVariantForRendition[rendition]
	return ok
}

func segmentRenditionsForInput(input SegmentTransformInput) []string {
	if !input.HasSubtitles {
		return segmentRenditions
	}
	return append(append([]string(nil), segmentRenditions...), "subtitles")
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
	seekSeconds := strconv.FormatFloat(float64(seekPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)
	trimSeconds := strconv.FormatFloat(float64(trimPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)

	cfg.Profiles = []LiveProfile{profile}
	withSubtitles := cfg.Captions && input.HasSubtitles
	inputPath := input.Path
	if inputPath == "" {
		inputPath = recordingHLSFFmpegInputPath
	}
	// オフセット 1 は共有ビルダーに入力側の -ss を出させるための仮の値で、
	// pre-roll を引いた絶対秒を下で差し込む。
	args := buildHLSFFmpegArgsForPlaylistType(
		cfg, workDir, withSubtitles, hlsRecordingEventPlaylist, inputPath, 1, input.AudioStreamCount,
	)
	var err error
	ssIndex := indexOf(args, "-ss")
	if ssIndex < 0 || ssIndex+1 >= len(args) {
		return nil, errors.New("segment HLS args do not contain input-side -ss")
	}
	args[ssIndex+1] = seekSeconds
	// 絶対秒でシークする。実 ffmpeg 9.0.2 の測定では、全ストリームが 5400 秒付近から始まる TS に
	// -copyts -seek_timestamp 1 -ss 5402 だけを渡すと出力が空になり、-noaccurate_seek を足すと
	// 先頭 PES が古い TS と揃った TS の両方で正しく出た（TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid）。
	// 空になった原因は未調査。フレーム精度は下の trim が担うので、accurate_seek は使わない。
	args = append(args[:ssIndex], append([]string{"-copyts", "-seek_timestamp", "1", "-noaccurate_seek"}, args[ssIndex:]...)...)
	if len(input.InputArgs) > 0 {
		inputIndex := indexOf(args, "-i")
		if inputIndex < 0 {
			return nil, errors.New("segment HLS args do not contain input -i")
		}
		args = append(args[:inputIndex], append(append([]string(nil), input.InputArgs...), args[inputIndex:]...)...)
	}
	// 最初のフレームは T の 0〜1 フレーム後ろにあり、その差は起動位置で変わる。STARTPTS で付け直すと
	// -force_key_frames の格子がその差に引きずられ、同じ番号のセグメントの先頭が起動位置で 1 フレーム
	// 変わった（実録画 2 本と TestSegmentTransformRestartKeepsSegmentStartFrame）。最初のフレームだけを
	// T（0）に置き、残りは元の PTS から T を引く。expr の t が最初のフレームから数えられても（実測の挙動。
	// FFmpeg の文書には無い）、出力の時刻から数えられても、格子は T に揃う。最初のセグメントは pre-roll で
	// キャッシュに入らないので、先頭フレームを動かしても配信には出ない（番号 0 の起動を除く）。
	args, err = appendSegmentOutputFilter(args, "-vf", "trim=start="+trimSeconds+",setpts=if(eq(N\\,0)\\,0\\,PTS-"+trimSeconds+"/TB)")
	if err != nil {
		return nil, err
	}
	audioFilter := "atrim=start=" + trimSeconds + ",asetpts=PTS-" + trimSeconds + "/TB"
	if input.VideoDurationPTS > 0 {
		// 長さが確定した入力では、音声が映像より短くても映像と同じ番号まで出す。apad=whole_dur は
		// サンプル数で数えるので、途中から起動した変換や途中で欠けた音声でずれる。PTS で切る。
		endSeconds := strconv.FormatFloat(float64(input.VideoDurationPTS-deltaPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)
		audioFilter += ",apad,atrim=end=" + endSeconds
	}
	for audioIndex := 0; audioIndex < 3; audioIndex++ {
		args, err = appendSegmentOutputFilter(
			args,
			"-filter:a:"+strconv.Itoa(audioIndex),
			audioFilter,
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
	// エンコーダの time base が既定（frame rate の逆数）だと、T からの時刻が 1 フレーム単位に丸められ、
	// 丸めの向きが起動位置で変わる。90 kHz にすると丸めが無くなる。passthrough が無いと、ffmpeg は
	// 90 kHz の固定フレームレートとしてフレームを複製した。
	// -output_ts_offset に負の時刻を渡すと、出力の先頭 PTS が 0 付近へずらされ、負の trim で起動した変換だけ
	// 格子から原点の分ずれた（TestSegmentTransformRealFFmpegKeepsGridAcrossEarlyPTSWrap）。
	// 出力の PTS は 2^33 を法として格子に合えばよいので、offset だけを 2^33 を法として非負に正規化する。
	outputOffsetPTS := ((trimPTS % recordingHLSPTSModulus) + recordingHLSPTSModulus) % recordingHLSPTSModulus
	outputOffsetSeconds := strconv.FormatFloat(float64(outputOffsetPTS)/float64(mpegTSTimestampClock), 'f', 9, 64)
	args = append(args[:outputFormat], append([]string{
		"-enc_time_base:v", "1/90000", "-fps_mode:v", "passthrough",
		"-muxpreload", "0", "-muxdelay", "0", "-output_ts_offset", outputOffsetSeconds,
	}, args[outputFormat:]...)...)
	return args, nil
}

func appendSegmentOutputFilter(args []string, option, filter string) ([]string, error) {
	options := []string{option}
	if option == "-vf" {
		// 字幕付き HLS は複数 video stream を 1 つの出力に含めるため、共有
		// ビルダーが型付き -filter:v:0 を使う。
		options = append(options, "-filter:v:0")
	}
	for _, candidate := range options {
		if index := indexOf(args, candidate); index >= 0 {
			if index+1 >= len(args) {
				return nil, fmt.Errorf("segment HLS args have no value after %s", candidate)
			}
			args[index+1] += "," + filter
			return args, nil
		}
	}
	index := indexOf(args, "-force_key_frames")
	if index < 0 {
		index = indexOf(args, "-force_key_frames:v:0")
	}
	if index < 0 {
		return nil, errors.New("segment HLS args do not contain -force_key_frames")
	}
	insertOption := option
	if option == "-vf" && indexOf(args, "-force_key_frames:v:0") >= 0 {
		insertOption = "-filter:v:0"
	}
	return append(args[:index], append([]string{insertOption, filter}, args[index:]...)...), nil
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

func readSubtitleSegmentPlaylist(path string) ([]int64, bool, error) {
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
		if number, ok := parseSubtitleSegmentNumber(filepath.Base(line)); ok {
			segments = append(segments, number)
		}
		needURI = false
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("reading HLS subtitle segment playlist: %w", err)
	}
	return segments, endList, nil
}

func parseSubtitleSegmentNumber(name string) (int64, bool) {
	text, ok := strings.CutPrefix(name, "seg")
	if !ok {
		return 0, false
	}
	text, ok = strings.CutSuffix(text, ".vtt")
	if !ok {
		return 0, false
	}
	return parseCanonicalRecordingID(text)
}

func parseOutputSegmentNumber(name, profile, variant string) (int64, bool) {
	prefix := variant + "_seg"
	if profile != "" {
		prefix = profile + "." + prefix
	}
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
