package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/tsstat"
)

const (
	defaultStallTimeout = 30 * time.Second
	maxInJobRetries     = 5
	ingestQueue         = "ingest"
)

// IngestJobArgs は ingest ジョブの引数。mirakc サイトと record ID を指定する。
type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

// Kind は River ジョブの種別名を返す。
func (IngestJobArgs) Kind() string { return "ingest" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// watcher は record-saved イベントと定期の全量突き合わせの両方から同じ record を
// 投入しうるので、ByArgs で一意化して二重取り込みを防ぐ。ByState は
// pendingJobStates に絞る（既定は completed を含むため、一度取り込んだ record を
// 手動で取り直せなくなる。取り込み済みかどうかは media_assets 行が真実であり、
// River のジョブ履歴で表現するものではない）。
func (IngestJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: ingestQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// IngestWorker は mirakc からの TS ファイル転送を行う River ワーカー。
type IngestWorker struct {
	river.WorkerDefaults[IngestJobArgs]
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string
	StallTimeout time.Duration
}

// Timeout は River の総時間タイムアウトを無効化する。
//
// ingest は数百 MB〜数十 GB のバイト転送で、所要時間は録画長と回線速度で決まる。
// River の既定（JobTimeoutDefault = 1 分）では実際の録画がまず完走しない。
//
// 総時間で切らない代わりに、進捗が止まったことを stallReader が検知して打ち切る
// （StallTimeout）。「タイムアウトは総時間でなくストール検知」という M1-5-2 の
// 設計はこれが揃って初めて成立する。
func (w *IngestWorker) Timeout(*river.Job[IngestJobArgs]) time.Duration {
	return -1
}

// Work は ingest ジョブを実行する。ストリーム取得・TS 統計収集・DB コミット・エッジ削除を行う。
func (w *IngestWorker) Work(ctx context.Context, job *river.Job[IngestJobArgs]) error {
	args := job.Args
	log := slog.With("site", args.Site, "record_id", args.RecordID)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.IngestDuration.Observe(time.Since(started).Seconds())
		metrics.IngestJobs.WithLabelValues(result).Inc()
	}()

	stallTimeout := w.StallTimeout
	if stallTimeout == 0 {
		stallTimeout = defaultStallTimeout
	}

	recordingID, err := w.lookupRecordingID(ctx, args)
	if err != nil {
		return fmt.Errorf("looking up recording_id: %w", err)
	}

	relPath, fullPath, err := w.determineRelPath(ctx, args)
	if err != nil {
		return fmt.Errorf("determining rel_path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", filepath.Dir(fullPath), err)
	}

	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", fullPath, err)
	}
	defer func() { _ = f.Close() }()

	counter := tsstat.NewCounter(f)

	var offset int64
	for attempt := 0; ; attempt++ {
		stallCtx, stallCancel := context.WithCancel(ctx)

		body, _, err := w.MirakcClient.StreamRecord(stallCtx, args.RecordID, offset)
		if err != nil {
			stallCancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt >= maxInJobRetries {
				return fmt.Errorf("streaming record (attempt %d): %w", attempt, err)
			}
			log.Warn("ingest: stream connect failed, retrying", "attempt", attempt, "err", err)
			continue
		}

		timer := time.AfterFunc(stallTimeout, func() { stallCancel() })
		sr := &stallReader{r: body, timer: timer, d: stallTimeout}
		n, copyErr := io.Copy(counter, sr)
		timer.Stop()
		stallCancel()
		_ = body.Close()

		offset += n

		if copyErr == nil {
			break
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if attempt >= maxInJobRetries {
			return fmt.Errorf("transfer failed after %d retries at offset %d: %w", attempt, offset, copyErr)
		}

		log.Warn("ingest: transfer interrupted, retrying with Range",
			"offset", offset, "attempt", attempt, "err", copyErr)
	}

	expectedLen, err := w.MirakcClient.HeadRecordStream(ctx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}

	log.Info("ingest: transfer complete", "bytes", offset,
		"drops", counter.TotalDrops(), "errors", counter.TotalErrors(),
		"scrambled", counter.TotalScrambled())

	metrics.IngestBytes.Add(float64(offset))
	metrics.IngestDroppedPackets.Add(float64(counter.TotalDrops()))
	metrics.IngestErrorPackets.Add(float64(counter.TotalErrors()))
	metrics.IngestScrambledPackets.Add(float64(counter.TotalScrambled()))

	if err := w.commit(ctx, recordingID, relPath, offset, counter); err != nil {
		return fmt.Errorf("committing ingest: %w", err)
	}

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	result = "success"

	if _, err := w.MirakcClient.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (committed OK)", "err", err)
	}

	return nil
}

func (w *IngestWorker) lookupRecordingID(ctx context.Context, args IngestJobArgs) (int64, error) {
	q := sqlcgen.New(w.Pool)
	recID, err := q.GetRecordSyncRecordingID(ctx, sqlcgen.GetRecordSyncRecordingIDParams{
		Site:     args.Site,
		RecordID: args.RecordID,
	})
	if err != nil {
		return 0, fmt.Errorf("querying record_sync: %w", err)
	}
	if recID == nil {
		return 0, fmt.Errorf("record_sync (%s, %s) has no recording_id", args.Site, args.RecordID)
	}
	return *recID, nil
}

// determineRelPath は保存先の相対パスと、それを解決した絶対パスを返す。
// relPath は mirakc の contentPath 由来なので、メディアディレクトリの外を
// 指していないことを検証する。
func (w *IngestWorker) determineRelPath(ctx context.Context, args IngestJobArgs) (relPath, fullPath string, err error) {
	record, err := w.MirakcClient.GetRecord(ctx, args.RecordID)
	if err != nil {
		return "", "", fmt.Errorf("getting mirakc record: %w", err)
	}
	if cp := record.Recording.Options.ContentPath; cp != nil && *cp != "" {
		relPath = *cp
	} else {
		relPath = filepath.Base(record.Content.Path)
	}
	fullPath, err = mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return "", "", err
	}
	return relPath, fullPath, nil
}

func (w *IngestWorker) commit(ctx context.Context, recordingID int64, relPath string, size int64, counter *tsstat.Counter) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		return fmt.Errorf("inserting media_asset: %w", err)
	}

	stats := counter.Stats()
	pids := make([]int, 0, len(stats))
	for pid := range stats {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		s := stats[pid]
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          int32(pid),
			Packets:      s.Packets,
			Drops:        s.Drops,
			Errors:       s.Errors,
			Scrambled:    s.Scrambled,
		}); err != nil {
			return fmt.Errorf("inserting drop_stat for PID %d: %w", pid, err)
		}
	}

	if counter.TotalScrambled() > 0 {
		event := db.QualityEvent{
			At:    time.Now(),
			Event: "bcas_anomaly",
		}
		evJSON, err := json.Marshal([]db.QualityEvent{event})
		if err != nil {
			return fmt.Errorf("marshalling quality event: %w", err)
		}
		if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{
			Events: evJSON,
			ID:     recordingID,
		}); err != nil {
			return fmt.Errorf("appending quality event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}
