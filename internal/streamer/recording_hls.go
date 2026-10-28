package streamer

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
)

const (
	// RecordingHLSRoutePattern は世代付き録画 HLS の固定深さパスである。
	RecordingHLSRoutePattern        = "/api/media/stores/{store}/recordings/{id}/hls/{version}/{profile}"
	recordingHLSProbeTimeout        = 30 * time.Second
	recordingHLSCacheControl        = "private, max-age=31536000, immutable"
	recordingHLSRetryAfter          = "1"
	recordingHLSMetadataLimit       = 256
	recordingHLSTempReadTimeout     = 90 * time.Second
	recordingHLSTempVisibilityDelay = 55 * time.Second
)

var (
	errRecordingHLSDBUnavailable = errors.New("recording HLS database is unavailable")
	errRecordingHLSTempNotReady  = errors.New("recording HLS temp is not ready")
	errRecordingHLSStaleVersion  = errors.New("recording HLS version is stale")
)

type recordingHLSStore interface {
	GetRecordingHLSTarget(context.Context, int64) (store.RecordingHLSInputTarget, error)
}

// RecordingHLSConfig は世代付き録画 HLS 配信の設定を表す。
type RecordingHLSConfig struct {
	StoreName       string
	MediaDir        string
	SegmentDir      string
	FFmpeg          string
	FFprobe         string
	Profiles        []LiveProfile
	HWAccel         *ffargs.HWAccel
	Captions        bool
	CacheBytes      int64
	MaxSegmentBytes int64
	MaxConcurrent   int
	WaitTimeout     time.Duration
}

type recordingHLSKey struct {
	recordingID int64
	generation  int64
	kind        store.RecordingHLSInputKind
}

type recordingHLSTiming struct {
	originPTS        int64
	videoDurationPTS int64
	audioStreamCount int
	hasSubtitles     bool
}

type recordingHLSMetadata struct {
	startedAt    time.Time
	tempSize     int64
	timing       recordingHLSTiming
	hasSubtitles bool
	final        bool
}

type recordingHLSMetadataEntry struct {
	key      recordingHLSKey
	metadata recordingHLSMetadata
}

type recordingHLSMetadataCache struct {
	mu      sync.Mutex
	entries map[recordingHLSKey]*list.Element
	lru     list.List
}

// RecordingHLS は録画中の temp と commit 済み原本を同じ世代付き URL で配信する。
type RecordingHLS struct {
	media   recordingHLSStore
	cfg     RecordingHLSConfig
	manager *SegmentManager
	meta    recordingHLSMetadataCache
}

// NewRecordingHLS は録画 HLS 配信とプロセス内のセグメント管理を作る。
func NewRecordingHLS(media recordingHLSStore, cfg RecordingHLSConfig) (*RecordingHLS, error) {
	if media == nil {
		return nil, errors.New("recording HLS media store is nil")
	}
	if !mediapath.ValidStoreName(cfg.StoreName) {
		return nil, errors.New("recording HLS store name is invalid")
	}
	if cfg.MediaDir == "" || cfg.SegmentDir == "" {
		return nil, errors.New("recording HLS media and segment directories are required")
	}
	if cfg.FFprobe == "" {
		cfg.FFprobe = "ffprobe"
	}
	cfg.FFprobe = ffargs.FFprobePath(cfg.FFprobe)
	if _, err := exec.LookPath(cfg.FFprobe); err != nil {
		return nil, fmt.Errorf("recording HLS ffprobe %q not found in PATH: %w", cfg.FFprobe, err)
	}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config: LiveConfig{
			FFmpeg:   cfg.FFmpeg,
			HWAccel:  cfg.HWAccel,
			Profiles: cfg.Profiles,
			Captions: cfg.Captions,
		},
		ScratchRoot:     cfg.SegmentDir,
		CacheBytes:      cfg.CacheBytes,
		MaxSegmentBytes: cfg.MaxSegmentBytes,
		MaxConcurrent:   cfg.MaxConcurrent,
	})
	if err != nil {
		return nil, fmt.Errorf("creating recording HLS segment manager: %w", err)
	}
	return &RecordingHLS{
		media:   media,
		cfg:     cfg,
		manager: manager,
		meta: recordingHLSMetadataCache{
			entries: make(map[recordingHLSKey]*list.Element),
		},
	}, nil
}

// Mount は世代付き録画 HLS の master、rendition playlist、segment ルートを登録する。
func (h *RecordingHLS) Mount(r chi.Router) {
	base := RecordingHLSRoutePattern
	r.Get(base+"/master.m3u8", h.MasterPlaylist)
	r.Get(base+"/{rendition}/index.m3u8", h.RenditionPlaylist)
	r.Get(base+"/{rendition}/{name}", h.Segment)
}

// Run は停止要求までキャッシュ・変換メトリクスを更新し、終了時に変換を止める。
func (h *RecordingHLS) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	h.updateMetrics()
	for {
		select {
		case <-ctx.Done():
			if err := h.manager.Close(); err != nil {
				return fmt.Errorf("closing recording HLS segment manager: %w", err)
			}
			metrics.RecordingHLSCacheBytes.Set(0)
			metrics.RecordingHLSActiveTransforms.Set(0)
			return nil
		case <-ticker.C:
			h.updateMetrics()
		}
	}
}

// Close は変換を止めてこのプロセスの録画 HLS scratch を掃除する。
func (h *RecordingHLS) Close() error {
	if err := h.manager.Close(); err != nil {
		return fmt.Errorf("closing recording HLS segment manager: %w", err)
	}
	h.updateMetrics()
	return nil
}

func (h *RecordingHLS) updateMetrics() {
	cacheBytes, activeTransforms := h.manager.Stats()
	metrics.RecordingHLSCacheBytes.Set(float64(cacheBytes))
	metrics.RecordingHLSActiveTransforms.Set(float64(activeTransforms))
}

// MasterPlaylist は全 rendition を参照する master playlist を即時に返す。
func (h *RecordingHLS) MasterPlaylist(w http.ResponseWriter, r *http.Request) {
	request, ok := h.request(w, r, h.cfg.Captions)
	if !ok {
		return
	}
	if _, ok := h.profile(chi.URLParam(r, "profile")); !ok {
		http.NotFound(w, r)
		return
	}
	playlist := buildRecordingHLSMasterPlaylist(request.metadata.hasSubtitles)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	_, _ = w.Write(playlist)
}

// RenditionPlaylist は入力の長さと状態に応じた EVENT playlist を即時に返す。
func (h *RecordingHLS) RenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	request, ok := h.request(w, r, true)
	if !ok {
		return
	}
	profile, ok := h.profile(chi.URLParam(r, "profile"))
	rendition := chi.URLParam(r, "rendition")
	if !ok || !supportedSegmentRendition(rendition) || (rendition == "subtitles" && (!h.cfg.Captions || !request.metadata.hasSubtitles)) {
		http.NotFound(w, r)
		return
	}
	var playlist []byte
	var err error
	if request.inputKind == store.RecordingHLSInputTemp {
		playlist, err = buildRecordingHLSTempPlaylist(request.metadata, profile, rendition, time.Now())
	} else {
		playlist, err = buildRecordingHLSRenditionPlaylist(request.metadata, profile, rendition)
	}
	if err != nil {
		http.Error(w, "recording HLS playlist could not be built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	_, _ = w.Write(playlist)
}

// Segment はキャッシュ済みセグメントを返すか、変換完了まで待って返す。
func (h *RecordingHLS) Segment(w http.ResponseWriter, r *http.Request) {
	request, ok := h.request(w, r, true)
	if !ok {
		return
	}
	profileName := chi.URLParam(r, "profile")
	_, ok = h.profile(profileName)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rendition := chi.URLParam(r, "rendition")
	if !supportedSegmentRendition(rendition) || (rendition == "subtitles" && (!h.cfg.Captions || !request.metadata.hasSubtitles)) {
		http.NotFound(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	extension := ".ts"
	if rendition == "subtitles" {
		extension = ".vtt"
	}
	if !strings.HasSuffix(name, extension) {
		http.NotFound(w, r)
		return
	}
	number, ok := parseCanonicalRecordingID(strings.TrimSuffix(name, extension))
	if !ok {
		http.NotFound(w, r)
		return
	}
	key := SegmentKey{
		RecordingID: strconv.FormatInt(request.recordingID, 10),
		Generation:  strconv.FormatInt(request.generation, 10),
		Profile:     profileName,
		Rendition:   rendition,
		Number:      number,
	}
	input := SegmentTransformInput{
		Kind:             request.inputKind,
		OriginPTS:        request.metadata.timing.originPTS,
		VideoDurationPTS: request.metadata.timing.videoDurationPTS,
		AudioStreamCount: request.metadata.timing.audioStreamCount,
		HasSubtitles:     request.metadata.hasSubtitles,
	}
	if request.inputKind == store.RecordingHLSInputTemp {
		input.Growing = !request.metadata.final
		if input.Growing {
			input.InputArgs = []string{
				"-skip_estimate_duration_from_pts", "1",
				"-follow", "1",
				"-rw_timeout", strconv.FormatInt(recordingHLSTempReadTimeout.Microseconds(), 10),
			}
		}
	}
	if request.dbUnavailable {
		input.OpenFile = func(context.Context) (*os.File, error) { return nil, errRecordingHLSDBUnavailable }
	} else {
		target := request.target
		recordingID := request.recordingID
		input.OpenFile = func(ctx context.Context) (*os.File, error) {
			return h.openInput(ctx, recordingID, target)
		}
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), h.cfg.WaitTimeout)
	defer cancel()
	file, cacheHit, err := h.manager.GetWithCacheStatus(waitCtx, key, input)
	if cacheHit {
		metrics.RecordingHLSCacheRequests.WithLabelValues("hit").Inc()
	} else {
		metrics.RecordingHLSCacheRequests.WithLabelValues("miss").Inc()
	}
	h.updateMetrics()
	if err != nil {
		h.writeSegmentError(w, r, request, err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Warn("streamer: closing recording HLS segment", "recording_id", request.recordingID, "generation", request.generation, "segment", number, "err", err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		slog.Error("streamer: stat recording HLS segment", "recording_id", request.recordingID, "generation", request.generation, "segment", number, "err", err)
		http.Error(w, "recording HLS segment unavailable", http.StatusInternalServerError)
		return
	}
	if rendition == "subtitles" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", recordingHLSCacheControl)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, err = io.Copy(w, file)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("streamer: writing recording HLS segment", "recording_id", request.recordingID, "generation", request.generation, "segment", number, "err", err)
	}
}

type recordingHLSRequest struct {
	recordingID   int64
	generation    int64
	target        store.RecordingHLSInputTarget
	inputKind     store.RecordingHLSInputKind
	metadata      recordingHLSMetadata
	dbUnavailable bool
}

func (h *RecordingHLS) request(w http.ResponseWriter, r *http.Request, needMetadata bool) (recordingHLSRequest, bool) {
	if chi.URLParam(r, "store") != h.cfg.StoreName {
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	version := chi.URLParam(r, "version")
	if !strings.HasPrefix(version, "v") {
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	generation, ok := parseCanonicalRecordingID(strings.TrimPrefix(version, "v"))
	if !ok {
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	target, err := h.media.GetRecordingHLSTarget(r.Context(), recordingID)
	if errors.Is(err, store.ErrNotFound) {
		h.manager.StopRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10))
		h.meta.removeRecording(recordingID, generation)
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	if err != nil {
		cacheKey, metadata, cached := h.meta.getAny(recordingID, generation)
		if !h.manager.HasRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10)) || (needMetadata && !cached) {
			slog.Warn("streamer: checking recording HLS target failed", "recording_id", recordingID, "generation", generation, "err", err)
			h.writeUnavailable(w)
			return recordingHLSRequest{}, false
		}
		if cacheKey.kind == store.RecordingHLSInputTemp {
			metadata.final = false
			metadata.timing.videoDurationPTS = 0
		}
		return recordingHLSRequest{recordingID: recordingID, generation: generation, inputKind: cacheKey.kind, metadata: metadata, dbUnavailable: true}, true
	}
	if target.Kind == store.RecordingHLSInputTemp && target.StoreName != h.cfg.StoreName {
		h.manager.StopRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10))
		h.meta.removeRecording(recordingID, generation)
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	if target.IngestGeneration != generation {
		h.manager.StopRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10))
		h.meta.removeRecording(recordingID, generation)
		http.Error(w, "recording HLS generation is stale", http.StatusGone)
		return recordingHLSRequest{}, false
	}
	if !needMetadata {
		return recordingHLSRequest{recordingID: recordingID, generation: generation, target: target, inputKind: target.Kind}, true
	}
	metadata, err := h.metadataForTarget(r.Context(), recordingID, generation, target)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		h.manager.StopRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10))
		h.meta.removeRecording(recordingID, generation)
		http.NotFound(w, r)
		return recordingHLSRequest{}, false
	}
	if err != nil {
		if errors.Is(err, errRecordingHLSDBUnavailable) || errors.Is(err, errRecordingHLSTempNotReady) {
			if cached, ok := h.meta.get(recordingHLSKey{recordingID: recordingID, generation: generation, kind: target.Kind}); ok && h.manager.HasRecording(strconv.FormatInt(recordingID, 10), strconv.FormatInt(generation, 10)) {
				if target.Kind == store.RecordingHLSInputTemp {
					cached.final = false
					cached.timing.videoDurationPTS = 0
				}
				return recordingHLSRequest{recordingID: recordingID, generation: generation, metadata: cached, target: target, inputKind: target.Kind, dbUnavailable: true}, true
			}
			h.writeUnavailable(w)
			return recordingHLSRequest{}, false
		}
		if errors.Is(err, errRecordingHLSStaleVersion) {
			http.Error(w, "recording HLS generation is stale", http.StatusGone)
			return recordingHLSRequest{}, false
		}
		slog.Error("streamer: inspecting recording HLS input", "recording_id", recordingID, "generation", generation, "err", err)
		http.Error(w, "recording HLS source could not be inspected", http.StatusInternalServerError)
		return recordingHLSRequest{}, false
	}
	return recordingHLSRequest{recordingID: recordingID, generation: generation, target: target, inputKind: target.Kind, metadata: metadata}, true
}

func (h *RecordingHLS) metadataForTarget(ctx context.Context, recordingID, generation int64, target store.RecordingHLSInputTarget) (recordingHLSMetadata, error) {
	key := recordingHLSKey{recordingID: recordingID, generation: generation, kind: target.Kind}
	final := target.Kind == store.RecordingHLSInputOriginal || (target.TempFinalized && recordingHLSTempEnded(target.RecordStatus))
	if metadata, ok := h.meta.get(key); ok {
		if target.StartedAt != nil && metadata.startedAt.Equal(*target.StartedAt) && metadata.final == final {
			if target.Kind != store.RecordingHLSInputTemp || !final {
				return metadata, nil
			}
			file, err := h.openTemp(ctx, recordingID, target)
			if err != nil {
				return recordingHLSMetadata{}, err
			}
			info, statErr := file.Stat()
			closeErr := file.Close()
			if statErr != nil {
				return recordingHLSMetadata{}, fmt.Errorf("stating finalized recording HLS temp: %w", statErr)
			}
			if closeErr != nil {
				return recordingHLSMetadata{}, fmt.Errorf("closing checked recording HLS temp: %w", closeErr)
			}
			if metadata.tempSize == target.SizeBytes && info.Size() == target.SizeBytes {
				return metadata, nil
			}
		}
		h.meta.remove(key)
	}
	if target.StartedAt == nil {
		if target.Kind == store.RecordingHLSInputTemp {
			return recordingHLSMetadata{}, fmt.Errorf("%w: recording start time is not available", errRecordingHLSTempNotReady)
		}
		return recordingHLSMetadata{}, errors.New("recording HLS target has no recording start time")
	}
	file, err := h.openInput(ctx, recordingID, target)
	if err != nil {
		return recordingHLSMetadata{}, err
	}
	timing, err := probeRecordingHLSTiming(ctx, h.cfg.FFprobe, file)
	closeErr := file.Close()
	if err != nil {
		if target.Kind == store.RecordingHLSInputTemp {
			return recordingHLSMetadata{}, fmt.Errorf("%w: probing growing recording HLS temp: %w", errRecordingHLSTempNotReady, err)
		}
		return recordingHLSMetadata{}, err
	}
	if closeErr != nil {
		return recordingHLSMetadata{}, fmt.Errorf("closing probed recording HLS original: %w", closeErr)
	}
	if target.Kind == store.RecordingHLSInputTemp && !final {
		timing.videoDurationPTS = 0
	}
	metadata := recordingHLSMetadata{
		startedAt: *target.StartedAt, tempSize: target.SizeBytes, timing: timing,
		hasSubtitles: timing.hasSubtitles, final: final,
	}
	h.meta.put(key, metadata)
	return metadata, nil
}

func (h *RecordingHLS) openInput(ctx context.Context, recordingID int64, target store.RecordingHLSInputTarget) (*os.File, error) {
	if target.Kind == store.RecordingHLSInputTemp {
		return h.openTemp(ctx, recordingID, target)
	}
	return h.openOriginal(ctx, recordingID, target)
}

func (h *RecordingHLS) openOriginal(ctx context.Context, recordingID int64, target store.RecordingHLSInputTarget) (*os.File, error) {
	path, err := mediapath.Resolve(h.cfg.MediaDir, target.RelPath)
	if err != nil {
		return nil, fmt.Errorf("resolving recording HLS original path: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("opening recording HLS original: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("stating recording HLS original: %w", err)
		}
		return nil, store.ErrNotFound
	}
	current, err := h.media.GetRecordingHLSTarget(ctx, recordingID)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("%w: verifying recording HLS original: %w", errRecordingHLSDBUnavailable, err)
	}
	if current.Kind != store.RecordingHLSInputOriginal || current.ID != target.ID || current.RelPath != target.RelPath || current.IngestGeneration != target.IngestGeneration {
		_ = file.Close()
		if current.IngestGeneration != target.IngestGeneration {
			return nil, errRecordingHLSStaleVersion
		}
		return nil, store.ErrNotFound
	}
	return file, nil
}

func (h *RecordingHLS) openTemp(ctx context.Context, recordingID int64, target store.RecordingHLSInputTarget) (*os.File, error) {
	if target.StoreName != h.cfg.StoreName {
		return nil, store.ErrNotFound
	}
	path, err := mediapath.Resolve(h.cfg.MediaDir, target.RelPath)
	if err != nil {
		return nil, fmt.Errorf("resolving recording HLS temp path: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if recordingHLSTempEnded(target.RecordStatus) {
				return nil, store.ErrNotFound
			}
			return nil, fmt.Errorf("opening recording HLS temp: %w", errRecordingHLSTempNotReady)
		}
		return nil, fmt.Errorf("opening recording HLS temp: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("stating recording HLS temp: %w", err)
		}
		return nil, fmt.Errorf("recording HLS temp is not regular: %w", errRecordingHLSTempNotReady)
	}
	current, err := h.media.GetRecordingHLSTarget(ctx, recordingID)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("verifying recording HLS temp: %w", errRecordingHLSTempNotReady)
		}
		return nil, fmt.Errorf("%w: verifying recording HLS temp: %w", errRecordingHLSDBUnavailable, err)
	}
	if current.IngestGeneration != target.IngestGeneration {
		_ = file.Close()
		return nil, errRecordingHLSStaleVersion
	}
	if current.Kind != store.RecordingHLSInputTemp || current.RelPath != target.RelPath || current.StoreName != target.StoreName {
		_ = file.Close()
		return nil, fmt.Errorf("recording HLS temp changed while opening: %w", errRecordingHLSTempNotReady)
	}
	if target.TempFinalized && recordingHLSTempEnded(target.RecordStatus) &&
		(!current.TempFinalized || current.RecordStatus != target.RecordStatus || current.SizeBytes != target.SizeBytes || info.Size() != target.SizeBytes) {
		_ = file.Close()
		return nil, fmt.Errorf("recording HLS terminal temp snapshot changed while opening: %w", errRecordingHLSTempNotReady)
	}
	return file, nil
}

func (h *RecordingHLS) profile(name string) (LiveProfile, bool) {
	for _, profile := range h.cfg.Profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return LiveProfile{}, false
}

func (h *RecordingHLS) writeSegmentError(w http.ResponseWriter, r *http.Request, request recordingHLSRequest, err error) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		h.manager.StopRecording(strconv.FormatInt(request.recordingID, 10), strconv.FormatInt(request.generation, 10))
		h.meta.removeRecording(request.recordingID, request.generation)
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errRecordingHLSStaleVersion) {
		http.Error(w, "recording HLS generation is stale", http.StatusGone)
		return
	}
	if errors.Is(err, errSegmentBeyondEnd) {
		http.Error(w, "recording HLS segment is outside the recording", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRecordingHLSDBUnavailable) || errors.Is(err, errRecordingHLSTempNotReady) || errors.Is(err, errSegmentTransformStopped) || errors.Is(err, errSegmentNotCached) || errors.Is(err, errSegmentGrowingInputRetry) {
		h.writeUnavailable(w)
		return
	}
	slog.Error("streamer: obtaining recording HLS segment", "recording_id", request.recordingID, "generation", request.generation, "err", err)
	http.Error(w, "recording HLS segment transform failed", http.StatusInternalServerError)
}

func (h *RecordingHLS) writeUnavailable(w http.ResponseWriter) {
	metrics.RecordingHLSUnavailable.Inc()
	w.Header().Set("Retry-After", recordingHLSRetryAfter)
	http.Error(w, "recording HLS segment is not ready", http.StatusServiceUnavailable)
}

func (c *recordingHLSMetadataCache) get(key recordingHLSKey) (recordingHLSMetadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		return recordingHLSMetadata{}, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(recordingHLSMetadataEntry).metadata, true
}

func (c *recordingHLSMetadataCache) getAny(recordingID, generation int64) (recordingHLSKey, recordingHLSMetadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for element := c.lru.Front(); element != nil; element = element.Next() {
		entry := element.Value.(recordingHLSMetadataEntry)
		if entry.key.recordingID == recordingID && entry.key.generation == generation {
			c.lru.MoveToFront(element)
			return entry.key, entry.metadata, true
		}
	}
	return recordingHLSKey{}, recordingHLSMetadata{}, false
}

func (c *recordingHLSMetadataCache) put(key recordingHLSKey, metadata recordingHLSMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		element.Value = recordingHLSMetadataEntry{key: key, metadata: metadata}
		c.lru.MoveToFront(element)
		return
	}
	c.entries[key] = c.lru.PushFront(recordingHLSMetadataEntry{key: key, metadata: metadata})
	if c.lru.Len() > recordingHLSMetadataLimit {
		oldest := c.lru.Back()
		entry := oldest.Value.(recordingHLSMetadataEntry)
		delete(c.entries, entry.key)
		c.lru.Remove(oldest)
	}
}

func (c *recordingHLSMetadataCache) remove(key recordingHLSKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		delete(c.entries, key)
		c.lru.Remove(element)
	}
}

func (c *recordingHLSMetadataCache) removeRecording(recordingID, generation int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, element := range c.entries {
		if key.recordingID == recordingID && key.generation == generation {
			delete(c.entries, key)
			c.lru.Remove(element)
		}
	}
}

func buildRecordingHLSMasterPlaylist(withSubtitles bool) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i, rendition := range segmentRenditions[1:] {
		defaultValue := "NO"
		if i == 0 {
			defaultValue = "YES"
		}
		name := []string{"標準", "主音声", "副音声"}[i]
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,URI=\"%s/index.m3u8\"\n", name, defaultValue, rendition)
	}
	if withSubtitles {
		b.WriteString("#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"字幕\",DEFAULT=YES,AUTOSELECT=YES,URI=\"subtitles/index.m3u8\"\n")
	}
	// LiveProfile にはビットレートが無い（CRF/QP で品質を決める）ので、BANDWIDTH は出力から
	// 導けず、固定値 20 Mbps を宣言する。実際の出力がこの値に収まるかは未検証。
	// web/e2e/recording-segmented-vod.mjs の `seek-buffered` の判定がこの値に依存する。
	// 値を変えるときはその E2E を回し直す。
	if withSubtitles {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=20000000,AUDIO=\"aud\",SUBTITLES=\"subs\"\nvideo/index.m3u8\n")
	} else {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=20000000,AUDIO=\"aud\"\nvideo/index.m3u8\n")
	}
	return []byte(b.String())
}

func buildRecordingHLSRenditionPlaylist(metadata recordingHLSMetadata, profile LiveProfile, rendition string) ([]byte, error) {
	if (!supportedSegmentRendition(rendition) || (rendition == "subtitles" && !metadata.hasSubtitles)) || profile.SegmentSeconds < 1 {
		return nil, errors.New("invalid recording HLS rendition or segment duration")
	}
	last := finalSegmentNumber(metadata.timing.videoDurationPTS, profile.SegmentSeconds)
	if last < 0 {
		return nil, errors.New("recording HLS video duration is unavailable")
	}
	return buildRecordingHLSPlaylist(metadata, profile, rendition, last, true)
}

func buildRecordingHLSTempPlaylist(metadata recordingHLSMetadata, profile LiveProfile, rendition string, now time.Time) ([]byte, error) {
	if (!supportedSegmentRendition(rendition) || (rendition == "subtitles" && !metadata.hasSubtitles)) || profile.SegmentSeconds < 1 {
		return nil, errors.New("invalid recording HLS rendition or segment duration")
	}
	last := recordingHLSTempLastSegmentNumber(metadata.startedAt, now, profile.SegmentSeconds)
	if metadata.final {
		last = finalSegmentNumber(metadata.timing.videoDurationPTS, profile.SegmentSeconds)
		if last < 0 {
			return nil, errors.New("recording HLS final temp duration is unavailable")
		}
	}
	return buildRecordingHLSPlaylist(metadata, profile, rendition, last, metadata.final)
}

// 壊し方: 55 秒の余裕を引かず、録画の実時間とともに末尾が増える判定をなくす。
func recordingHLSTempLastSegmentNumber(startedAt, now time.Time, segmentSeconds int) int64 {
	if segmentSeconds < 1 {
		return -1
	}
	margin := recordingHLSTempVisibilityDelay + time.Duration(segmentSeconds)*time.Second
	elapsed := now.Sub(startedAt) - margin
	if elapsed < 0 {
		return -1
	}
	return int64(elapsed / (time.Duration(segmentSeconds) * time.Second))
}

func recordingHLSTempEnded(status string) bool {
	return status == "canceled" || status == "failed"
}

func buildRecordingHLSPlaylist(metadata recordingHLSMetadata, profile LiveProfile, rendition string, last int64, endlist bool) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n", profile.SegmentSeconds)
	remaining := float64(metadata.timing.videoDurationPTS) / float64(mpegTSTimestampClock)
	programDateTime := metadata.startedAt
	for number := int64(0); number <= last; number++ {
		segmentSeconds := float64(profile.SegmentSeconds)
		if endlist {
			segmentSeconds = math.Min(float64(profile.SegmentSeconds), remaining)
		}
		if segmentSeconds <= 0 {
			return nil, errors.New("recording HLS segment duration is not positive")
		}
		extension := ".ts"
		if rendition == "subtitles" {
			extension = ".vtt"
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%s,\n%d%s\n",
			programDateTime.Format(time.RFC3339Nano), strconv.FormatFloat(segmentSeconds, 'f', 3, 64), number, extension)
		if endlist {
			remaining -= segmentSeconds
		}
		programDateTime = programDateTime.Add(time.Duration(profile.SegmentSeconds) * time.Second)
	}
	if endlist {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String()), nil
}

func probeRecordingHLSTiming(ctx context.Context, ffprobe string, file *os.File) (recordingHLSTiming, error) {
	probeCtx, cancel := context.WithTimeout(ctx, recordingHLSProbeTimeout)
	defer cancel()
	ffprobe = ffargs.FFprobePath(ffprobe)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return recordingHLSTiming{}, fmt.Errorf("rewinding recording HLS original: %w", err)
	}
	packetCmd := exec.CommandContext(probeCtx, ffprobe,
		"-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=pts",
		"-read_intervals", "%+#1", "-of", "csv=p=0", "-i", originalVODFFmpegInputPath,
	)
	packetCmd.ExtraFiles = []*os.File{file}
	packetOutput, err := packetCmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return recordingHLSTiming{}, probeCtx.Err()
		}
		return recordingHLSTiming{}, fmt.Errorf("probing recording HLS first video packet: %w", err)
	}
	packetField := strings.Split(strings.TrimSpace(string(packetOutput)), ",")[0]
	originPTS, err := strconv.ParseInt(packetField, 10, 64)
	if err != nil {
		return recordingHLSTiming{}, fmt.Errorf("parsing recording HLS first video packet PTS %q: %w", packetField, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return recordingHLSTiming{}, fmt.Errorf("rewinding recording HLS original: %w", err)
	}
	streamCmd := exec.CommandContext(probeCtx, ffprobe,
		"-v", "error", "-show_entries", "stream=codec_type,start_time,duration", "-of", "json", "-i", originalVODFFmpegInputPath,
	)
	streamCmd.ExtraFiles = []*os.File{file}
	streamOutput, err := streamCmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return recordingHLSTiming{}, probeCtx.Err()
		}
		return recordingHLSTiming{}, fmt.Errorf("probing recording HLS streams and duration: %w", err)
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(streamOutput, &result); err != nil {
		return recordingHLSTiming{}, fmt.Errorf("decoding recording HLS stream probe: %w", err)
	}
	var ok bool
	var videoStart, videoDuration float64
	var hasVideo bool
	var resultHasSubtitles bool
	audioCount := 0
	for _, stream := range result.Streams {
		switch stream.CodecType {
		case "video":
			if !hasVideo {
				videoStart, hasVideo = finiteFloat(stream.StartTime)
				videoDuration, ok = finiteFloat(stream.Duration)
				hasVideo = hasVideo && ok
			}
		case "audio":
			audioCount++
		case "subtitle":
			resultHasSubtitles = true
		}
	}
	if !hasVideo || audioCount == 0 {
		return recordingHLSTiming{}, errors.New("recording HLS probe did not find a video and audio stream")
	}
	// SegmentManager は originPTS を n=0 の境界にするため、映像 stream の終端から
	// その同じ PTS 原点を引く。format.start_time を使うと先頭に古い音声 PES がある
	// TS で、その隙間ぶんだけ VOD playlist に存在しない segment を足してしまう。
	durationSeconds := videoStart + videoDuration - float64(originPTS)/float64(mpegTSTimestampClock)
	if durationSeconds <= 0 || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) || durationSeconds > float64(math.MaxInt64)/float64(mpegTSTimestampClock) {
		return recordingHLSTiming{}, fmt.Errorf("recording HLS probe returned invalid video duration %v", durationSeconds)
	}
	durationPTS := int64(math.Round(durationSeconds * float64(mpegTSTimestampClock)))
	return recordingHLSTiming{originPTS: originPTS, videoDurationPTS: durationPTS, audioStreamCount: audioCount, hasSubtitles: resultHasSubtitles}, nil
}

func finiteFloat(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(raw, 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}
