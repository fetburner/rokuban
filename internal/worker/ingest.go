package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/tsstat"
)

const (
	maxInJobRetries = 5

	// connectRetryBaseDelay / connectRetryMaxDelay は StreamRecord への接続が
	// 失敗したときの指数バックオフの下限・上限。mirakc が即座に refuse する状況
	// （再起動直後など）で 6 回のリトライを一瞬で使い切らないようにする。
	// 転送中断（Range 再開）側は stallReader が既に間を置くのでバックオフ不要。
	connectRetryBaseDelay = 200 * time.Millisecond
	connectRetryMaxDelay  = 5 * time.Second
)

// ingestFile は ingest の出力ファイルと、fsync 対象の親ディレクトリを
// 抽象化する。os.File の全 API は必要ない。テストでは Sync / Close の失敗を
// 注入して、失敗時に DB 登録とエッジ原本削除へ進まないことを確認する。
type ingestFile interface {
	io.Writer
	Sync() error
	Close() error
}

var openIngestFile = func(path string) (ingestFile, error) {
	return os.Create(path)
}

var openIngestDirectory = func(path string) (ingestFile, error) {
	return os.Open(path)
}

// IngestWorker は mirakc からの TS ファイル転送を行う River ワーカー。
type IngestWorker struct {
	river.WorkerDefaults[jobs.IngestJobArgs]

	// MirakcClients は site → mirakc クライアントの map（issue #532。1 プロセスが
	// N site を束縛できるため、この 1 インスタンスが複数 site の ingest_<site>
	// キューを同時に購読しうる）。Work は verifySite で args.Site に対応する
	// クライアントを取り出してから使う。
	MirakcClients map[string]*mirakc.Client
	Pool          *pgxpool.Pool
	MediaDir      string

	// StallTimeout は転送中の無進捗検知タイムアウト（config.ingest.stall_timeout。
	// config.defaults() が既定値 30 秒を埋めるので、ここでは常に config が
	// 渡した値をそのまま使う）。
	StallTimeout time.Duration

	// ProgressInterval は recording_ingest_progress を書き直す最短間隔
	// （issue #212）。0 は「未設定」で ingestProgressInterval に解決する
	// （resolveProgressInterval）。テストが転送の途中経過を観測するために
	// 短くできるようにしてあるだけで、運用上は既定のままでよい。
	ProgressInterval time.Duration

	// RelPathLockTimeout は rel_path advisory lock の取得（pool.Acquire と
	// pg_try_advisory_lock）に与える上限。0 は「未設定」で
	// defaultRelPathLockTimeout に解決する（ProgressInterval と同じ規約。
	// resolveRelPathLockTimeout 参照。config キーが無い --- ProgressInterval の
	// doc コメント参照）。
	RelPathLockTimeout time.Duration
}

// Timeout は River の総時間タイムアウトを無効化する。
//
// ingest は数百 MB〜数十 GB のバイト転送で、所要時間は録画長と回線速度で決まる。
// River の既定（JobTimeoutDefault = 1 分）では実際の録画がまず完走しない。
//
// 総時間で切らない代わりに、進捗が止まったことを stallReader が検知して打ち切る
// （StallTimeout）。「タイムアウトは総時間でなくストール検知」という M1-5-2 の
// 設計はこれが揃って初めて成立する。
func (w *IngestWorker) Timeout(*river.Job[jobs.IngestJobArgs]) time.Duration {
	return -1
}

// resolveProgressInterval は設定された ProgressInterval があればそれを、
// なければ既定の ingestProgressInterval を返す（config キーが無いフィールドの
// 「0 は未設定」規約。ProgressInterval の doc コメント参照）。
func (w *IngestWorker) resolveProgressInterval() time.Duration {
	if w.ProgressInterval == 0 {
		return ingestProgressInterval
	}
	return w.ProgressInterval
}

// resolveRelPathLockTimeout は設定された RelPathLockTimeout があればそれを、
// なければ既定の defaultRelPathLockTimeout を返す（resolveProgressInterval と
// 同じ「0 は未設定」の規約）。
func (w *IngestWorker) resolveRelPathLockTimeout() time.Duration {
	if w.RelPathLockTimeout == 0 {
		return defaultRelPathLockTimeout
	}
	return w.RelPathLockTimeout
}

// Work は ingest ジョブを実行する。ストリーム取得・TS 統計収集・DB コミット・エッジ削除を行う。
func (w *IngestWorker) Work(ctx context.Context, job *river.Job[jobs.IngestJobArgs]) error {
	args := job.Args
	log := slog.With("site", args.Site, "record_id", args.RecordID)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.IngestDuration.Observe(time.Since(started).Seconds())
		metrics.IngestJobs.WithLabelValues(result).Inc()
	}()

	// mirakc の record id はインスタンススコープ。他サイトのジョブをこの
	// プロセスの mirakc に投げると、別番組をこの recording としてコミットしうる
	// （issue #139）。DB 参照（lookupRecordingID）や mirakc/FS への一切の
	// アクセスより前に照合する。
	client, err := verifySite(w.MirakcClients, args.Site, jobs.IngestQueue)
	if err != nil {
		return err
	}

	recordingID, expectedBytes, err := w.lookupIngestTarget(ctx, args)
	if err != nil {
		return fmt.Errorf("looking up recording_id: %w", err)
	}

	// 冪等性チェック：この recording_id の original media_asset が既にコミット
	// 済みなら転送をやり直さない。エッジ record の削除
	// （handleAlreadyCommittedIngest / enqueueIngestFollowups の DeleteRecord）は
	// 失敗してもログのみで ingest 成功扱いにしている（意図的）ため、mirakc 側に
	// record が残ったまま 5 分後の record_sweep → watcher.processRecord
	// （status=finished）経由で同じ record の ingest ジョブが再投入されうる。
	// pendingJobStates の UniqueOpts は completed を除外するのでこの再投入は
	// 止まらない。ここで止めないと os.Create がコミット済みファイルを 0
	// バイトに切り詰めて全量を再ダウンロードし、streamer は不変条件 3
	// （コミット = DB 行）で既にコミット済みの録画に対して欠けたファイルを
	// 配ることになる。
	alreadyCommitted, err := w.hasOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking existing media_asset: %w", err)
	}
	if alreadyCommitted {
		result = "success"
		w.handleAlreadyCommittedIngest(ctx, client, args, recordingID, log)
		return nil
	}

	relPath, fullPath, err := w.determineRelPath(ctx, args, client)
	if err != nil {
		return fmt.Errorf("determining rel_path: %w", err)
	}

	// rel_path の advisory lock を、mirakc のストリームを開く
	// （transferIngestRecord の StreamRecord）より前・下の os.Create より前に
	// 取る。media_assets の一意索引
	// （rel_path, WHERE state <> 'deleted'）が効くのは commit の INSERT の
	// 瞬間だが、宛先へのバイトはそれより前に落ちる（docs/storage/contract.md
	// §3 ルール 3 の順序そのもの）。順序だけでは実ファイルを守れないので、
	// 排他を索引より前に置く（docs/recording/ingest.md §5.3）。
	//
	// 負けた側（acquired=false）はバイトを 1 つも書かずに失敗し、River の
	// バックオフで再試行する。ロックは commit まで defer で保持し続ける。
	release, acquired, err := acquireRelPathLock(ctx, w.Pool, relPath, w.resolveRelPathLockTimeout())
	if err != nil {
		return fmt.Errorf("acquiring rel_path lock: %w", err)
	}
	if !acquired {
		log.Warn("ingest: rel_path is being transferred by another ingest job, deferring", "rel_path", relPath)
		return fmt.Errorf("ingest: rel_path %q is being transferred by another ingest job; deferring (recording_id=%d)", relPath, recordingID)
	}
	defer release()

	// checkRelPathConflict はロックの下（＝転送開始前だが排他は既に確定した後）
	// で引く。ロックを持っている間は他の ingest がこの rel_path を狙って
	// 転送を始めることはできないので、ここでの SELECT の結果は commit まで
	// 安定する --- **ingest 対 ingest に関してはもはや先読みではなく決着その
	// もの**（doc コメント参照）。ここで拾うのは「別の（今 transfer 中では
	// ない）recording が過去にこの rel_path を使って既にコミットした」という
	// 恒久的な衝突（contentPath 重複、issue #197）で、これは delete_reconcile
	// の状態遷移に対しては引き続きヒント（TOCTOU が残る）でしかない。
	if conflictRecordingID, err := w.checkRelPathConflict(ctx, relPath); err != nil {
		return fmt.Errorf("checking rel_path conflict: %w", err)
	} else if conflictRecordingID != 0 {
		return fmt.Errorf("ingest: rel_path %q is already used by another media_asset that has not been deleted (recording_id=%d); refusing to overwrite its file (recording_id=%d)",
			relPath, conflictRecordingID, recordingID)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", filepath.Dir(fullPath), err)
	}

	f, err := openIngestFile(fullPath)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", fullPath, err)
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = f.Close()
		}
	}()

	counter := tsstat.NewCounter(f)

	progress := &ingestProgressReporter{
		pool:          w.Pool,
		recordingID:   recordingID,
		expectedBytes: expectedBytes,
		interval:      w.resolveProgressInterval(),
		log:           log,
	}
	// 転送の途中経過を recording_ingest_progress に写す（issue #212）。行の存在
	// そのものが「転送中」の主張なので（不変条件 10）、1 バイトも流れる前に
	// 1 行書いてから始める --- 遅い回線で最初の 1 バイトが来るまで数十秒かかる
	// ことがあり、そこが「何も起きていないように見える」時間帯そのものだから。
	progress.start(ctx)
	// progressWriter は counter の外側に置く（io.Copy → progressWriter →
	// counter → f）。TS 統計は counter が数えるので、ここでは書けたバイト数を
	// 数えるだけ。
	dst := &progressWriter{
		w:       counter,
		onWrite: func(written int64) { progress.report(ctx, written) },
	}

	offset, err := w.transferIngestRecord(ctx, client, args.RecordID, dst, progress, log)
	if err != nil {
		return err
	}
	expectedLen, err := client.HeadRecordStream(ctx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}

	// Close 成功だけではホスト電源断後の永続化を保証しない。転送完了後に
	// 一度だけファイル本体を fsync し、Close してから親ディレクトリの
	// ディレクトリエントリも fsync する。途中の定期 fsync は行わない
	// （S3 系 FUSE 上で転送途中の実体化を増やさないため）。この全てが成功する
	// まで DB へ登録せず、失敗時は mirakc の record を保持して再試行させる。
	persistStarted := time.Now()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	if err := f.Close(); err != nil {
		fileClosed = true
		return fmt.Errorf("closing file: %w", err)
	}
	fileClosed = true
	if err := syncIngestParentDirectory(filepath.Dir(fullPath)); err != nil {
		return err
	}

	// pid_type_changes > 0 は録画中に PMT が PID を付け替えたということ。
	// 種別は最後に見たものを採用するので、変化そのものはここにしか残らない
	// （docs/recording.md §1「例外の境界」）。
	log.Info("ingest: transfer complete", "bytes", offset,
		"drops", counter.TotalDrops(), "errors", counter.TotalErrors(),
		"scrambled", counter.TotalScrambled(),
		"pid_type_changes", counter.TypeChanges(),
		"persist_duration", time.Since(persistStarted))

	recordIngestMetrics(offset, counter)

	if err := w.commit(ctx, recordingID, relPath, offset, counter); err != nil {
		return fmt.Errorf("committing ingest: %w", err)
	}

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	result = "success"

	w.enqueueIngestFollowups(ctx, client, args.RecordID, recordingID, log)

	return nil
}

// syncIngestParentDirectory は新規ファイルの直近の親ディレクトリを fsync
// する。ファイル本体の Sync だけでは、ディレクトリエントリが永続化される
// 保証がないため、ファイルの Close 後かつ DB 登録前に呼ぶ。
func syncIngestParentDirectory(path string) error {
	dir, err := openIngestDirectory(path)
	if err != nil {
		return fmt.Errorf("opening parent directory %s: %w", path, err)
	}
	dirClosed := false
	defer func() {
		if !dirClosed {
			_ = dir.Close()
		}
	}()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("syncing parent directory %s: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		dirClosed = true
		return fmt.Errorf("closing parent directory %s: %w", path, err)
	}
	dirClosed = true
	return nil
}

// handleAlreadyCommittedIngest は原本が既にある ingest の残務を処理する。
// 進捗行とエッジ record の削除失敗はログだけにして ingest を成功扱いにする。
//
// 転送は行っていないが、DB からは成功と区別が付かない状態なのでメトリクスも
// 成功として数える（呼び出し元が result="success" を設定する）。ここでの
// 唯一の残り仕事はエッジ record の削除（下記）の再試行であり、それが完了
// すれば ingest の目的（コミット済み・record 削除済み）は満たされている。
func (w *IngestWorker) handleAlreadyCommittedIngest(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, log *slog.Logger) {
	log.Info("ingest: media_asset already committed, skipping transfer", "recording_id", recordingID)
	// 前回の実行が転送の途中で死に、その後別経路（internal/inplace.Register
	// など）で原本がコミットされた場合、進捗行だけが取り残される。原本行が
	// ある録画の進捗表示は API 側が無視する（原本の有無が真実。不変条件 5）
	// ので害は無いが、掃除できる場所で掃除しておく。
	if err := sqlcgen.New(w.Pool).DeleteRecordingIngestProgress(ctx, recordingID); err != nil {
		log.Warn("ingest: failed to clear stale transfer progress", "recording_id", recordingID, "err", err)
	}
	// 原本があるなら encode の desired−observed も埋める（ヒント。真実は
	// EnqueueMissingEncodes のレベルトリガー判定。issue #65）。
	enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID)
	if _, err := client.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (already committed)", "err", err)
	}
}

// transferIngestRecord は mirakc のストリームを Range 再開しながら宛先へ転送する。
// rel_path の advisory lock は呼び出し元が取得済みであることを前提にする。
func (w *IngestWorker) transferIngestRecord(ctx context.Context, client *mirakc.Client, recordID string, dst io.Writer, progress *ingestProgressReporter, log *slog.Logger) (int64, error) {
	var offset int64
	for attempt := 0; ; attempt++ {
		stallCtx, stallCancel := context.WithCancel(ctx)
		body, _, err := client.StreamRecord(stallCtx, recordID, offset)
		if err != nil {
			stallCancel()
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if attempt >= maxInJobRetries {
				return 0, fmt.Errorf("streaming record (attempt %d): %w", attempt, err)
			}
			delay := connectRetryDelay(attempt)
			log.Warn("ingest: stream connect failed, retrying", "attempt", attempt, "err", err, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			continue
		}

		timer := time.AfterFunc(w.StallTimeout, func() { stallCancel() })
		n, copyErr := io.Copy(dst, &stallReader{r: body, timer: timer, d: w.StallTimeout})
		timer.Stop()
		stallCancel()
		_ = body.Close()
		offset += n

		// バイトを書けた転送試行が終わるたび、最後の値を間引き無しで焼く。
		// interval 内の burst 後に接続が切れ、その後の再開も全て失敗すると、
		// ここで書かなければ最後の間引き前の値のままジョブが終わる。0 バイトの
		// 試行は進捗ではないので observed_at を新しくしない。
		if n > 0 {
			progress.flush(ctx, offset)
		}
		if copyErr == nil {
			return offset, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if attempt >= maxInJobRetries {
			return 0, fmt.Errorf("transfer failed after %d retries at offset %d: %w", attempt, offset, copyErr)
		}
		log.Warn("ingest: transfer interrupted, retrying with Range", "offset", offset, "attempt", attempt, "err", copyErr)
	}
}

// recordIngestMetrics は転送結果のバイト数・TS 統計をメトリクスへ記録する。
func recordIngestMetrics(offset int64, counter *tsstat.Counter) {
	metrics.IngestBytes.Add(float64(offset))
	metrics.IngestDroppedPackets.Add(float64(counter.TotalDrops()))
	metrics.IngestErrorPackets.Add(float64(counter.TotalErrors()))
	metrics.IngestScrambledPackets.Add(float64(counter.TotalScrambled()))
}

// enqueueIngestFollowups はコミット済み ingest の encode / thumbnail 投入ヒントと
// mirakc record の削除を行う。補助処理の失敗はログに記録して本処理を成功扱いにする。
//
// encode 投入はヒント。desired（encode_profiles）− observed（encoded assets）
// を埋めるレベルトリガー（命令的チェーンではない。issue #65）。
//
// thumbnail 投入はヒント。desired − observed を EnqueueThumbnailIfNeeded が
// 判定する（レベルトリガー。命令的チェーンではない。issue #66）。
// River クライアントが無いテスト経路では黙ってスキップする。
func (w *IngestWorker) enqueueIngestFollowups(ctx context.Context, client *mirakc.Client, recordID string, recordingID int64, log *slog.Logger) {
	enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID)
	if riverClient, clientErr := river.ClientFromContextSafely[pgx5.Tx](ctx); clientErr == nil {
		if enqueueErr := EnqueueThumbnailIfNeeded(ctx, w.Pool, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue thumbnail job", "recording_id", recordingID, "err", enqueueErr)
		}
	}
	if _, err := client.DeleteRecord(ctx, recordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (committed OK)", "err", err)
	}
}

// connectRetryDelay は StreamRecord への接続リトライの待ち時間を指数バックオフで返す。
// attempt は 0 始まり。connectRetryMaxDelay で頭打ちにする。
func connectRetryDelay(attempt int) time.Duration {
	delay := connectRetryBaseDelay << attempt
	if delay > connectRetryMaxDelay || delay <= 0 {
		return connectRetryMaxDelay
	}
	return delay
}

// hasOriginalMediaAsset は recordingID に対する kind='original' の media_asset
// 行が既に存在するかを返す。ingest の冪等性チェックに使う（Work 参照）。
func (w *IngestWorker) hasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	q := sqlcgen.New(w.Pool)
	_, err := q.GetOriginalMediaAssetID(ctx, recordingID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("querying media_assets: %w", err)
}

// checkRelPathConflict は relPath を既に使っている、まだ削除されていない
// （state <> 'deleted'。'active' に限らず、削除処理中の 'deleting' も含む）
// media_asset があれば、その recording_id を返す（無ければ 0, nil）。Work が
// rel_path の advisory lock を取得した後・os.Create の前に呼ぶ。
//
// **ingest 対 ingest に関しては、これはもはや「先読み」ではなく決着そのもの
// である。** Work はこの関数を呼ぶ前に同じ relPath の advisory lock を
// commit まで保持し続けるので（acquireRelPathLock）、他の ingest ジョブは
// この関数が実行されている間、同じ relPath への転送を一切開始できない ---
// したがってこの SELECT の結果（衝突の有無）は、この ingest が commit する
// 瞬間まで安定する。ここで拾うのは「別の（今 transfer 中ではない）
// recording が過去にこの rel_path を使って既にコミットした」という恒久的な
// 衝突（同一サイト内の contentPath 重複、issue #197）であり、advisory lock
// を取っていない別の recording が同時に同じ relPath へ転送を始めることは
// もう起こらない。
//
// **ただし delete_reconcile の状態遷移に対しては、従来どおりヒントのまま
// である。** delete_reconcile は rel_path の advisory lock を取らないので、
// この SELECT と実際の CreateMediaAsset の INSERT の間に delete_reconcile が
// 'deleting' → 'deleted' の遷移を進める TOCTOU の窓は残る。正しさの根拠は
// 常に media_assets の一意索引（CREATE UNIQUE INDEX ON media_assets
// (rel_path) WHERE state <> 'deleted'）であり、
// レベルトリガー（CLAUDE.md 不変条件 5）の原則どおり、この関数を「一意索引を
// 通す前の安価なゲート」以上の役割にしない。一意索引を緩めたり INSERT の
// エラー処理を弱めたりする理由には使わない。
//
// WHERE state <> 'deleted' はその一意索引の述語と同じにする。削除済み
// （state='deleted'）の行が使っていた rel_path は正当に再利用できるので、
// ここで引っかけて誤って失敗させてはいけない。'deleting'（delete_reconcile の
// unlink 前後の中間状態）はまだ 'deleted' ではない --- unlink 前・unlink
// 失敗中は実ファイルが残っており、かつ resolveUnqualifiedDeletingAsset が
// ファイルの現存を確認した上でその行を active に戻しうる
// （delete_reconcile.go）ため、'deleting' の rel_path を ingest が上書きすると
// 「DB は active、実体は別番組」が再生産される。したがって 'deleting' も
// 'active' と同じく衝突として扱う。
func (w *IngestWorker) checkRelPathConflict(ctx context.Context, relPath string) (int64, error) {
	q := sqlcgen.New(w.Pool)
	conflictRecordingID, err := q.GetLiveMediaAssetByRelPath(ctx, relPath)
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("querying media_assets: %w", err)
	}
	return conflictRecordingID, nil
}

// lookupIngestTarget は record_sync から転送先の recording_id と、進捗の分母に
// 使う content_length を 1 回のクエリで読む。
//
// content_length は watcher が mirakc record から観測した値（record.content.length）
// で、mirakc が返さなければ nil。分母をここから取るのは、転送中に使える唯一の
// 材料だから --- HEAD の Content-Length は転送完了後の照合（層 3）にしか取って
// おらず、ファイル stat は api ロールが読めない（不変条件 1）。nil のときは
// 進捗をバイト数だけで出し、% は出さない（issue #212）。
func (w *IngestWorker) lookupIngestTarget(ctx context.Context, args jobs.IngestJobArgs) (recordingID int64, expectedBytes *int64, err error) {
	q := sqlcgen.New(w.Pool)
	row, err := q.GetRecordSyncIngestTarget(ctx, sqlcgen.GetRecordSyncIngestTargetParams{
		Site:     args.Site,
		RecordID: args.RecordID,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("querying record_sync: %w", err)
	}
	if row.RecordingID == nil {
		return 0, nil, fmt.Errorf("record_sync (%s, %s) has no recording_id", args.Site, args.RecordID)
	}
	return *row.RecordingID, row.ContentLength, nil
}

// determineRelPath は保存先の相対パスと、それを解決した絶対パスを返す。
// relPath は mirakc の contentPath 由来なので、メディアディレクトリの外を
// 指していないことを検証する。`sites/{site}/` を前置する判断（前置する理由・
// 固定の 1 段目を挟む理由・前置前の既存行を移行しない判断）は
// docs/storage/contract.md §rel_path の名前空間にある。
//
// 前置に使うのは args.Site。Work は determineRelPath を呼ぶ前に verifySite
// （internal/worker/worker.go）で args.Site が w.MirakcClients のいずれかの
// キーと一致することを検査済みである。w.MirakcClients のキーは常に実際の
// （空でない）site 名なので（config のバリデーションが site 名の非空を要求する。
// verifySite は jobSite を正規化しない）、verifySite を通過した args.Site は
// 常に非空である。
//
// client は verifySite が返したクライアント（Work が呼び出し元で解決済み）を
// そのまま受け取る --- ここで再度 w.MirakcClients を引くと、verifySite が通した
// site と実際に使うクライアントがズレる経路を作ってしまう。
//
// contentPath / Content.Path がどちらも空だと relPath が "."（カレント
// ディレクトリ）になる。前置後は "sites/{site}/." が Join/Clean で "." が
// 消えて "sites/{site}" という一見正当なパスになり mediapath.Resolve を
// 通ってしまい、os.Create が "{media_dir}/sites/{site}" を通常ファイルとして
// 作ってしまう（以後その site 配下の ingest が全て MkdirAll で
// "not a directory" になる。docs/storage/contract.md §rel_path の名前空間
// 参照）。前置前に弾く（下記）。
func (w *IngestWorker) determineRelPath(ctx context.Context, args jobs.IngestJobArgs, client *mirakc.Client) (relPath, fullPath string, err error) {
	record, err := client.GetRecord(ctx, args.RecordID)
	if err != nil {
		return "", "", fmt.Errorf("getting mirakc record: %w", err)
	}
	if cp := record.Recording.Options.ContentPath; cp != nil && *cp != "" {
		relPath = *cp
	} else {
		relPath = filepath.Base(record.Content.Path)
	}
	// contentPath / Content.Path がどちらも空だと filepath.Base("") == "."。
	// 前置すると mediapath.Resolve の脱出検知をすり抜けてディレクトリが作られて
	// しまう（上の doc コメント参照）ので、前置前に明示的に弾く。
	if relPath == "." || relPath == "/" {
		return "", "", fmt.Errorf("mirakc record %s has no usable content path (contentPath and Content.Path both empty)", args.RecordID)
	}
	// rel_path のパス区切りは DB 上で '/' 規約（internal/worker/encode.go の
	// EncodedRelPath 参照）。args.Site は site 名の構文制約で '/' を含み得ない
	// ので単純な文字列結合で足りる。catalog.SiteRelPathPrefix は rescue の
	// ストレージスキャン（internal/catalog の classifySiteForRescuedFile）がこの前置を
	// 逆に読むので、書き手と読み手でリテラルを重複させずここに揃える。
	relPath = catalog.SiteRelPathPrefix + args.Site + "/" + relPath
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

	// 進捗行（issue #212）は原本の INSERT と同じ tx で消す。コミット = DB 行
	// （不変条件 3）なので、原本行が生まれる瞬間に進捗行が消えることで
	// 「原本があるのに取り込み中」という中間状態が読者から見えない。
	if err := q.DeleteRecordingIngestProgress(ctx, recordingID); err != nil {
		return fmt.Errorf("clearing ingest progress: %w", err)
	}

	// 原本のコミットと同じ tx で「この録画の望ましい最終状態」を焼く
	// （issue #103。resolveAndSnapshotEncodePolicy の doc コメント参照）。
	if err := w.resolveAndSnapshotEncodePolicy(ctx, q, recordingID); err != nil {
		return fmt.Errorf("snapshotting encode policy: %w", err)
	}

	stats := counter.Stats()
	pids := make([]int, 0, len(stats))
	for pid := range stats {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		s := stats[pid]
		// 分類できなかった PID は種別なし（NULL）。空文字を「未分類」という値として
		// 永続化しない（M2-13, issue #24）。
		var pidType *string
		if s.Type != "" {
			t := s.Type
			pidType = &t
		}
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          int32(pid),
			Packets:      s.Packets,
			Drops:        s.Drops,
			Errors:       s.Errors,
			Scrambled:    s.Scrambled,
			PidType:      pidType,
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

// resolveAndSnapshotEncodePolicy は「この録画の望ましい最終状態」を
// recording_encode_policy へ凍結する（行の存在そのものが「凍結済み」を
// 意味する。不変条件 3「コミット = DB 行」・不変条件 10「意味を持たない行を
// 作らない」）。呼び出し元の commit が原本 media_asset の INSERT と同じ tx で、
// かつ encode ジョブの投入（EnqueueMissingEncodes）より必ず先に呼ぶ（順序が
// 逆だと初回パスで desired が空のまま enqueue される）。
//
// 凍結か毎パス再導出か・凍結する瞬間・予約を放送イベントキーで引く理由・
// 解決に失敗しても凍結する理由・source 別のログレベル・凍結が依存する寿命と
// エッジの滞留の交点・冪等性・EncodeProfiles の nil の扱い・until_encoded
// クランプの判断は docs/storage/retention.md §6「原本 TS の保持ポリシー」に
// ある。
func (w *IngestWorker) resolveAndSnapshotEncodePolicy(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	rec, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading recording %d: %w", recordingID, err)
	}

	row, err := q.GetReservationEncodePolicyByEvent(ctx, sqlcgen.GetReservationEncodePolicyByEventParams{
		Site:      rec.Site,
		NetworkID: rec.NetworkID,
		ServiceID: rec.ServiceID,
		EventID:   rec.EventID,
	})
	// 解決に失敗しても凍結自体はスキップしない（issue #159。doc コメント
	// 「解決失敗時も凍結する」参照）。既定値（'always' / '{}'）で凍結する ---
	// 何も INSERT しないと、原本 media_asset の有無で「凍結済みか」を判定する
	// 不変条件が破れ、かつ issue #133
	// の事後追加（AppendRecordingEncodeProfiles）が「行が既にある」ことを前提に
	// できなくなる。
	keepOriginal := "always"
	encodeProfiles := []string{}
	if err != nil {
		if !errors.Is(err, pgx5.ErrNoRows) {
			return fmt.Errorf("loading reservation encode policy for recording %d (site=%s network_id=%d service_id=%d event_id=%d): %w",
				recordingID, rec.Site, rec.NetworkID, rec.ServiceID, rec.EventID, err)
		}
		// source='rule' は常に「予約はあったのに引けなくなった」（doc コメント
		// 「予約をどのキーで引くか」の 3 原因を参照。issue #214 の交点を含む）
		// なので Warn。source='manual' も「意図があった」ことを示す snapshot なので
		// Warn にする。source='unattributed' は予約も意図も特定できない録画で、
		// 予約が最初から無い日常的なケースを表すため Info に落とす。どの source
		// でも黙って return しない —— 判別できないことをログの欠落で埋め合わせない。
		logArgs := []any{
			"recording_id", recordingID,
			"site", rec.Site,
			"network_id", rec.NetworkID,
			"service_id", rec.ServiceID,
			"event_id", rec.EventID,
		}
		if rec.Source == reservation.SourceUnattributed {
			slog.Info("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		} else {
			slog.Warn("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		}
	} else {
		eff, err := reservation.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
		if err != nil {
			return fmt.Errorf("computing effective options for program %d: %w", row.Reservation.ProgramID, err)
		}
		if eff.KeepOriginal != nil {
			keepOriginal = *eff.KeepOriginal
		}
		if eff.EncodeProfiles != nil {
			encodeProfiles = *eff.EncodeProfiles
		}
	}

	// クランプ（doc コメント「keepOriginal='until_encoded' × encodeProfiles=[] の
	// クランプ」参照）。ルール単独・override 単独ではそれぞれ禁則を満たしていても、
	// マージ結果としてこの組み合わせが生成されうる。cardinality(encode_profiles) > 0
	// を要求する CHECK（issue #104）とここで矛盾すると、このメソッドを呼ぶ tx
	// （原本 media_asset の INSERT と同一）ごとロールバックし録画が消える
	// （不変条件 3）ため、書く前に安全側へ倒す。
	if keepOriginal == "until_encoded" && len(encodeProfiles) == 0 {
		keepOriginal = "always"
	}

	return q.FreezeRecordingEncodePolicy(ctx, sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    recordingID,
		KeepOriginal:   keepOriginal,
		EncodeProfiles: encodeProfiles,
	})
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
