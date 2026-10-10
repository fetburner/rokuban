package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"golang.org/x/sys/unix"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/tsscan"
)

// errIngestRecordEndedAbnormally は、追従中の record が finished ではなく
// canceled / failed で終わったことを表す sentinel（RecordFollowReader の status hook が wrap し、
// Work が errors.Is で拾う）。
//
// **これを River の再試行に戻してはならない。** cancel / fail は中身が不採用と
// 確定した終端なので、Work は temp を破棄して再試行を止める。River の MaxAttempts
// は既定のまま（25）で、エッジの record は commit 成功時にしか消えないため、
// 取り消した長時間録画が全量再取得を繰り返す形にしてはいけない。
//
// 部分ファイルは資産として commit せず、エッジの record も消さない（purge すると
// content path を共有する後継録画のファイルまで消える）。理由は
// docs/recording/ingest.md §5.3 層 3。
var errIngestRecordEndedAbnormally = errors.New("mirakc record ended abnormally")

// errIngestSHA256Pending は、転送済みの temp を残して SHA-256 の再確認を snooze する内部制御エラー。
var errIngestSHA256Pending = errors.New("ingest content SHA-256 is pending")

// errIngestSliceElapsed は temp と SHA-256 の checkpoint を保存した試行の区切りを表す。
var errIngestSliceElapsed = errors.New("ingest transfer slice elapsed")

const (
	ingestWorkTimeout          = 5 * time.Minute
	ingestSHA256SnoozeInterval = 30 * time.Second
)

// ingestTransferSlice は 1 回の Work で replay と転送に使う時間の上限。
// 4 分の slice と 1 分の後処理予算で Work Timeout を 5 分にし、6 分の rescue 既定より
// 短くする。--once では snooze ごとに Pod が入れ替わるため、運用クラスタで起動時間が
// slice より十分短いことを確認する。
var ingestTransferSlice = 4 * time.Minute

// ingestSHA256BytesPerSecond は finished 後に mirakc が content.sha256 を非同期計算する
// 速度（バイト/秒）の見積もり。待ちの上限は時間ではなくこの速度で固定する ---
// 計算量は録画サイズに比例するので、時間の固定値では大きい録画ほどほぼ必ず
// timeout_skipped になる。報告例の実測は 2.35 GB を約 4 分（約 9.8 MB/s）で、
// 25% の余裕を含めて 7.8 MB/s とする。計測点が 1 件のため設定キーにはしない。
// 地デジ 30 分（約 3.8 GB）で約 8 分、BS 2 時間（約 20 GB）で約 43 分になる
// （この除算の結果であり、実機での待ち時間の測定ではない）。
var ingestSHA256BytesPerSecond int64 = 7_800_000

// ingestFollowPace は録画追従（mirakc.RecordFollowOptions）の待ち時間の上書き。
// 本番では零値（mirakc 側の既定を使う）。実時間のポーリング間隔や再試行バックオフを
// 払いたくないテストだけが setFastIngestFollow で差し替える。
var ingestFollowPace struct {
	pollMin, pollMax time.Duration
	retryDelay       func(attempt int) time.Duration
}

// ingestSHA256MinWait は小さい録画でも mirakc がハッシュ計算を始める猶予として待つ下限。
const ingestSHA256MinWait = 10 * time.Second

// ingestSHA256WaitFor は転送済みバイト数 size に対する SHA-256 待ちの上限を返す。
func ingestSHA256WaitFor(size int64) time.Duration {
	if size < 0 {
		size = 0
	}
	seconds := (size + ingestSHA256BytesPerSecond - 1) / ingestSHA256BytesPerSecond
	return max(time.Duration(seconds)*time.Second, ingestSHA256MinWait)
}

// ingestSHA256WaitDeadline は recording.endTime が有効ならその値を、そうでなければ
// temp の mtime を起点に、size に応じた SHA-256 待ち期限を返す。
func ingestSHA256WaitDeadline(size int64, endTime *mirakc.Milliseconds, tempModTime, now time.Time) time.Time {
	startedAt := tempModTime
	if endTime != nil && !endTime.Time().IsZero() && !endTime.Time().After(now) {
		startedAt = endTime.Time()
	}
	return startedAt.Add(ingestSHA256WaitFor(size))
}

// ingestFile は ingest の出力ファイルを抽象化する。os.File の全 API は
// 必要ない。テストでは Sync / Close の失敗を注入して、失敗時に DB 登録と
// エッジ原本削除へ進まないことを確認する。実装側の openIngestFile は
// lockIngestTempFile が開いた fd を dup するため、ロック対象と追記先が別 inode
// になることはない。
type ingestFile interface {
	io.Writer
	Sync() error
	Close() error
}

// hashingWriter は下流 writer が受理したバイトだけを hash に流す。
//
// 下流が (n > 0, err != nil) の部分書き込みを返しても、次の Range 再開は
// その n バイトの直後から始まる。hash を io.MultiWriter の 2 番目に置くと
// 1 番目の writer のエラーで hash への Write が呼ばれないため、受理済みの
// バイトがハッシュから欠落する。
type hashingWriter struct {
	w io.Writer
	h hash.Hash
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		_, hashErr := w.h.Write(p[:n])
		if err == nil {
			err = hashErr
		}
	}
	return n, err
}

// openIngestFile は flock 済みの temp fd を dup して ingestFile として返す。
// 同じ record の別試行は既存のファイルを共有して続きから書くので、元 fd も複製
// fd も O_APPEND で開かれている。dup は同じ inode / open file description を
// 共有するため、writer の Close で flock を先に解放せず、tempLock が commit
// 終了まで排他を保持できる。path はテストのログやエラーメッセージに使えるよう
// 引数に残しているが、ここで別のパスを開いてはならない。
var openIngestFile = func(_ string, locked *os.File) (ingestFile, error) {
	return duplicateIngestFile(locked)
}

func duplicateIngestFile(locked *os.File) (*os.File, error) {
	// F_DUPFD_CLOEXEC で複製と close-on-exec を原子的に行う。Dup の後に
	// CloseOnExec を呼ぶだけでは、その間の exec に fd が継承されうる。
	fd, err := unix.FcntlInt(locked.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dup := os.NewFile(uintptr(fd), locked.Name())
	if dup == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("creating duplicate ingest fd")
	}
	return dup, nil
}

// lockIngestTempFile は temp を作成し、同時に 1 つの ingest だけが掴めるように
// 排他 flock を取る。LOCK_NB にするのは、同じ record の別ジョブが live な転送を
// 壊さず River の再試行へ戻るためである。
//
// 返した fd は writer の元 fd として使い、openIngestFile はこれを dup する。
// lock fd と writer fd を別々に path open すると、ロック取得後の unlink / rename
// と writer の open の順序次第で、ロックした古い inode と別名の新しい inode に
// 同時追記できるためである。
//
// open 直後に別プロセスが同名 temp を unlink / rename する競合もある。flock を
// 取得した後で fd とパスの inode が同じかを確認し、見えない inode を返さない。
func lockIngestTempFile(path string) (*os.File, error) {
	return lockIngestTempFileWithFlags(path, os.O_CREATE)
}

// lockExistingIngestTempFile は既存 temp だけをロックする。orphan 回収が
// O_CREATE で新しい空ファイルを作ると、別の ingest が同名 temp を作る直前の
// 瞬間に回収側が作った空ファイルを「新しい temp」と誤認してしまうため、回収側
// ではこちらを使う。
func lockExistingIngestTempFile(path string) (*os.File, error) {
	return lockIngestTempFileWithFlags(path, 0)
}

func lockIngestTempFileWithFlags(path string, createFlag int) (*os.File, error) {
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|createFlag, 0o666)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			return nil, err
		}

		lockedInfo, err := lock.Stat()
		pathInfo, pathErr := os.Stat(path)
		if err == nil && pathErr == nil && os.SameFile(lockedInfo, pathInfo) {
			return lock, nil
		}

		// パスが別 inode になったか、unlink された。ロックした fd は
		// visible temp ではないので、解放して現在のパスを取り直す。
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
			return nil, pathErr
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("ingest temporary file %q was replaced while acquiring its lock", path)
}

func ingestTempLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// ingestTempFilePath は同じ site / record_id の River 再試行が同じ temp を開くための
// パスを返す。record_id は通常 mirakc の hex ID だが、念のためファイル名に使えない
// 区切りを含む値が media_dir の外へ出ないようにする。site も map から直接渡るため
// 同じ防御を適用する。
func ingestTempFilePath(dir, site, recordID string) string {
	escape := strings.NewReplacer(
		"%", "%25",
		"/", "%2F",
		"\\", "%5C",
	)
	return filepath.Join(dir, mediapath.IngestTempFilePrefix+escape.Replace(site)+"-"+escape.Replace(recordID))
}

// ingestReplayReader は read の境界で context cancellation を確認する。
// io.Copy に *os.File を直接渡すと File.WriteTo が選ばれて context を確認できない
// ため、Read だけを公開するラッパーにして chunk 単位で再生を止める。
type ingestReplayReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ingestReplayReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	return r.r.Read(p)
}

// replayIngestTempFile は既存 temp の全バイトを hasher に通して SHA-256 の状態を復元する。
// 読み出しは context のキャンセルを chunk 境界で検知し、temp は残す。
func replayIngestTempFile(ctx context.Context, path string, hasher hash.Hash) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("replaying ingest temporary file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stating ingest temporary file: %w", err)
	}
	want := info.Size()

	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening ingest temporary file for replay: %w", err)
	}
	n, copyErr := io.Copy(hasher, &ingestReplayReader{ctx: ctx, r: f})
	closeErr := f.Close()
	if copyErr != nil {
		return n, fmt.Errorf("replaying ingest temporary file: %w", copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("closing ingest temporary file after replay: %w", closeErr)
	}
	if n != want {
		return n, fmt.Errorf("ingest temporary file changed during replay: read=%d size=%d", n, want)
	}
	return n, nil
}

// renameIngestFile / syncIngestParentDir は確定プロトコルの OS 操作をテストから
// 観測・失敗注入できるようにする。実装の順序（DB INSERT → rename → 親 dir fsync
// → DB commit）を、ファイルシステムの実体に依存せず検証するためのフックである。
var renameIngestFile = os.Rename
var syncIngestParentDir = syncIngestDirectory

// beforeIngestFilePublication は DB の予約を終え、canonical file の公開を
// 始める直前に呼ぶテスト用フック。通常実行時は何もしない。DB セッションが
// 失われても、この直後の rename / fsync を rel_path filesystem lock が守る
// ことを、実際の transaction を使って検証するために置いている。
var beforeIngestFilePublication = func(context.Context, pgx5.Tx) error { return nil }
var commitIngestTransaction = func(ctx context.Context, tx pgx5.Tx) error {
	return tx.Commit(ctx)
}

// newIngestCommitQueries builds the sqlc Queries commit() issues its statements
// through. Tests override this to wrap tx in a sqlcgen.DBTX that counts SendBatch
// calls directly, because pgx v5.10's Conn.SendBatch returns emptyBatchResults for a
// zero-length batch before ever invoking a pgx.QueryTracer's TraceBatchStart
// (conn.go:943) — a tracer-based oracle cannot observe whether an empty batch was
// sent at all.
var newIngestCommitQueries = func(tx pgx5.Tx) *sqlcgen.Queries {
	return sqlcgen.New(tx)
}

func syncIngestDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("opening parent directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("syncing parent directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing parent directory: %w", err)
	}
	return nil
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
	CMDetect      config.CMDetectConfig

	// CutProfiles は cut: true のプロファイル名（config から注入）。凍結時の
	// クランプ（resolveAndSnapshotEncodePolicy）に使う。
	CutProfiles map[string]struct{}
	// LiveEnabled は config.live.enabled。原本 HLS が使えない場合だけ、凍結時に
	// cut-only の選択を安全側へクランプする。
	LiveEnabled bool

	// StallTimeout は転送中の無進捗検知タイムアウト（config.ingest.stall_timeout。
	// config.defaults() が既定値 30 秒を埋めるので、ここでは常に config が
	// 渡した値をそのまま使う）。
	StallTimeout time.Duration

	// ProgressInterval は recording_ingest_progress を書き直す最短間隔
	// （issue #212）。0 は「未設定」で ingestProgressInterval に解決する
	// （resolveProgressInterval）。テストが転送の途中経過を観測するために
	// 短くできるようにしてあるだけで、運用上は既定のままでよい。
	ProgressInterval time.Duration
}

// Timeout は ingest 1 試行の上限を返す。
//
// 4 分の replay / 転送区切りに、HEAD・ハッシュ確認・fsync・commit のため 1 分を足す。
// 区切りでは temp と SHA-256 状態を保存して River に snooze を返すため、録画全体の
// 転送速度や長さでジョブの締切は決まらない。5 分は River の既定 JobTimeout（1 分）を
// 超え、rescue の既定 6 分より短い。追従の無進捗検知は従来どおり StallTimeout が担う。
func (w *IngestWorker) Timeout(*river.Job[jobs.IngestJobArgs]) time.Duration {
	return ingestWorkTimeout
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

// Work は ingest ジョブを実行する。ストリーム取得・DB コミット・エッジ削除を行う。
//
// 戻り値を名前付きにするのは、defer が「この試行の結末」を分類して
// metrics.IngestJobs / IngestDuration へ記録するためである（記録の値域と
// 中断を数えない理由は下の defer のコメントと
// docs/operations/monitoring.md の rokuban_ingest_jobs_total）。
func (w *IngestWorker) Work(ctx context.Context, job *river.Job[jobs.IngestJobArgs]) (err error) {
	args := job.Args
	log := slog.With("site", args.Site, "record_id", args.RecordID)

	started := time.Now()
	result := "failure"
	skipMetrics := false
	defer func() {
		if skipMetrics {
			// 区切りと SHA-256 待ちは同じ job を続ける中間状態。
			return
		}
		// result は success / failure / canceled の 3 値。**この 3 値の外に増やさない**
		// （低カーディナリティが前提。record id や理由は入れない）。
		//
		cause := context.Cause(ctx)
		remote := errors.Is(cause, river.ErrJobCancelledRemotely)
		ctxErr := ctx.Err()
		// stop は中断として数えず、締切超過は試行失敗として数える。River が作る
		// work ctx は stop なら Canceled、Timeout 超過なら DeadlineExceeded になる。
		if ctxErr == context.Canceled && !remote &&
			(errors.Is(err, context.Canceled) || errors.Is(err, cause)) {
			return
		}
		if ctxErr == context.DeadlineExceeded {
			result = "failure"
		}
		// リモート取消は canceled に倒す。ただし **err == nil のときは倒さない** ---
		// River は res.Err != nil のときだけ cause で置き換えて cancelled にするので
		// （job_executor.go）、nil は completed（= その時点の result）のままにする。
		if err != nil && remote {
			result = "canceled"
		}
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
	// 止まらない。ここで止めないと新しい試行がコミット済み canonical file を
	// 置き換えて全量を再ダウンロードし、streamer は不変条件 3
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
	if err := w.ingestResolvedRecord(ctx, client, args, recordingID, expectedBytes, log, &result); err != nil {
		if errors.Is(err, errIngestSHA256Pending) {
			skipMetrics = true
			return river.JobSnooze(ingestSHA256SnoozeInterval)
		}
		if errors.Is(err, errIngestSliceElapsed) {
			skipMetrics = true
			return river.JobSnooze(0)
		}
		return err
	}
	return nil
}

//nolint:funlen // temp lock, transfer, hash verification, and publication share one cleanup boundary.
func (w *IngestWorker) ingestResolvedRecord(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, expectedBytes *int64, log *slog.Logger, result *string) error {
	relPath, fullPath, initialRecord, err := w.determineRelPath(ctx, args, client)
	if err != nil {
		return fmt.Errorf("determining rel_path: %w", err)
	}

	// 既に同じ rel_path を使う行がある場合は、無駄な転送を始める前に拒む。
	// これは安価なヒントであり、ingest 同士の競合を決着させるものではない。
	// 本当の採用順序は commit 内の未コミット INSERT（unique index の予約）で
	// 保証する。
	if conflictRecordingID, err := w.checkRelPathConflict(ctx, relPath); err != nil {
		return fmt.Errorf("checking rel_path conflict: %w", err)
	} else if conflictRecordingID != 0 {
		return fmt.Errorf("ingest: rel_path %q is already used by another media_asset that has not been deleted (recording_id=%d); refusing to overwrite its file (recording_id=%d)",
			relPath, conflictRecordingID, recordingID)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", filepath.Dir(fullPath), err)
	}

	tempPath := ingestTempFilePath(filepath.Dir(fullPath), args.Site, args.RecordID)
	tempLock, err := lockIngestTempFile(tempPath)
	if err != nil {
		return fmt.Errorf("locking ingest temporary file %s: %w", tempPath, err)
	}
	f, err := openIngestFile(tempPath, tempLock)
	if err != nil {
		_ = tempLock.Close()
		return fmt.Errorf("opening ingest temporary file %s: %w", tempPath, err)
	}
	// temp はプロセス死・ctx キャンセル・一時的な I/O / DB 失敗では残す。次の
	// River 試行が同じファイルを replay して続けるためである。中身が悪いと確定
	// した場合だけ下の removeTemp を立てる。rename 後は tempPath が存在しないので
	// cleanup の Remove は no-op になり、canonical file は孤児回収に委ねられる。
	removeTemp := false
	defer func() {
		if removeTemp {
			// tempLock は f の dup 元 fd で、commit 終了まで flock を保持する。
			// 別の試行が同名パスを作ってから古い cleanup がそれを消すことが
			// ないよう、ロックを保持したまま unlink してから lock fd を閉じる。
			_ = os.Remove(tempPath)
			_ = os.Remove(ingestCheckpointPath(tempPath))
		} else if _, statErr := os.Stat(tempPath); errors.Is(statErr, os.ErrNotExist) {
			// commit の rename 後に失敗した場合も checkpoint は temp と同じ寿命で終える。
			if err := os.Remove(ingestCheckpointPath(tempPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("ingest: removing checkpoint after temp publication failed", "err", err)
			}
		}
		_ = f.Close()
		// openIngestFile のテスト差し替えが underlying fd を閉じずに失敗
		// する場合にもロックを残さない。
		_ = tempLock.Close()
	}()

	// finished record の temp が HEAD の長さまで転送済みなら、replay する前に
	// ハッシュを再確認する。snooze 中も大きな temp を毎回読み直さない。
	preflightSHA256, waiting, err := w.preflightIngestSHA256(ctx, client, args.RecordID, recordingID, initialRecord, tempPath, f, log)
	if err != nil {
		return err
	}
	if waiting {
		return errIngestSHA256Pending
	}

	// replay と pull を合わせて 1 区切りにし、既存 temp が大きい場合も途中で再開できる。
	// River の Timeout はこの区切りより 1 分長く、checkpoint の fsync と commit に使う。
	sliceCtx, cancelSlice := context.WithTimeout(ctx, ingestTransferSlice)
	defer cancelSlice()
	progress := &ingestProgressReporter{
		pool:          w.Pool,
		recordingID:   recordingID,
		expectedBytes: expectedBytes,
		interval:      w.resolveProgressInterval(),
		log:           log,
	}
	hasher := sha256.New()
	offset, replayComplete, err := replayIngestTempFileWithCheckpoint(sliceCtx, tempPath, f, hasher)
	if err != nil {
		if isIngestSliceDeadline(err, sliceCtx, ctx) {
			progress.start(ctx, offset)
			return errIngestSliceElapsed
		}
		return err
	}
	if !replayComplete {
		return fmt.Errorf("replaying ingest temp ended without completion")
	}
	// 既存 temp の SHA-256 を復元してから、新規転送の受理バイトを追記先と hasher に流す。
	sink := &hashingWriter{w: f, h: hasher}

	// 転送の途中経過を recording_ingest_progress に写す（issue #212）。行の存在
	// そのものが「転送中」の主張なので（不変条件 10）、1 バイトも流れる前に
	// 1 行書いてから始める --- 遅い回線で最初の 1 バイトが来るまで数十秒かかる
	// ことがあり、そこが「何も起きていないように見える」時間帯そのものだから。
	progress.start(ctx, offset)
	// progressWriter は hashingWriter の外側に置き、temp に受理されたバイト数を報告する。
	dst := &progressWriter{
		w:       sink,
		written: offset,
		onWrite: func(written int64) { progress.report(ctx, written) },
	}

	var expectedSHA256 *string
	var observedEndTime *mirakc.Milliseconds
	offset, expectedSHA256, observedEndTime, err = w.transferIngestRecord(sliceCtx, client, args.RecordID, dst, progress, offset)
	if err != nil {
		if isIngestSliceDeadline(err, sliceCtx, ctx) {
			if err := persistIngestCheckpoint(tempPath, f, offset, hasher); err != nil {
				return err
			}
			progress.flush(ctx, offset)
			return errIngestSliceElapsed
		}
		if errors.Is(err, errIngestRecordEndedAbnormally) {
			removeTemp = true
			// 再試行に戻さない（errIngestRecordEndedAbnormally の doc コメント
			// 参照）。進捗行は他の削除経路（commit /
			// handleAlreadyCommittedIngest）と揃え、失敗してもジョブは落とさない。
			if delErr := sqlcgen.New(w.Pool).DeleteRecordingIngestProgress(ctx, recordingID); delErr != nil {
				log.Warn("ingest: failed to clear stale transfer progress", "recording_id", recordingID, "err", delErr)
			}
			// 取り消し・失敗は「転送が壊れた」ではないので、失敗として数えない。
			// 同じ result="failure" に混ぜると、利用者が止めた録画が失敗率に
			// 積まれて本物の失敗が埋もれる。
			*result = "canceled"
			return river.JobCancel(err)
		}
		return err
	}
	expectedLen, err := client.HeadRecordStream(ctx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		removeTemp = true
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}
	if expectedSHA256 == nil && preflightSHA256 != nil {
		expectedSHA256 = preflightSHA256
	}
	if expectedSHA256 == nil {
		var waiting bool
		expectedSHA256, waiting, err = w.maybeWaitForSHA256(ctx, client, args.RecordID, recordingID, tempPath, f, expectedLen, offset, observedEndTime, log)
		if err != nil {
			return err
		}
		if waiting {
			// maybeWaitForSHA256 が temp を Sync した後なので、hash 状態をその末尾へ合わせて保存する。
			if err := writeIngestCheckpoint(tempPath, offset, hasher); err != nil {
				return err
			}
			return errIngestSHA256Pending
		}
	}
	sha256Verification := "skipped"
	expectedHash, hashAvailable := normalizeContentSHA256(expectedSHA256)
	if expectedSHA256 == nil {
		sha256Verification = "timeout_skipped"
	} else if !hashAvailable {
		sha256Verification = "invalid_skipped"
		log.Warn("ingest: skipping invalid content sha256", "value", *expectedSHA256)
	}
	if hashAvailable {
		sha256Verification = "verified"
		actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
		if actualSHA256 != expectedHash {
			removeTemp = true
			metrics.IngestHashMismatches.Inc()
			log.Warn("ingest: content sha256 mismatch", "actual", actualSHA256, "expected", expectedHash, "bytes", offset)
			return fmt.Errorf("hash mismatch: actual=%s expected=%s bytes=%d", actualSHA256, expectedHash, offset)
		}
	}

	// Linux では遅延した書き込みエラー（ENOSPC / I/O エラー）は Close() では
	// 上がらず、fsync() でしか報告されない。offset はここまで転送できたバイト数を
	// メモリ上で数えた値であって実際にディスクへ落ちたことの確認ではないので、
	// 上の Content-Length 照合もこの種の失敗を素通りしてしまう。ここで fsync が
	// 失敗したら DB へ登録せず、mirakc の record を保持して再試行させる。途中の fsync は
	// 区切りでだけ行い、各バッファごとの実体化は増やさない。
	syncStarted := time.Now()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}

	log.Info("ingest: transfer complete", "bytes", offset,
		"sha256_verification", sha256Verification,
		"fsync_duration", time.Since(syncStarted))

	recordIngestMetrics(offset)

	if err := w.commit(ctx, recordingID, relPath, tempPath, fullPath, offset); err != nil {
		return fmt.Errorf("committing ingest: %w", err)
	}

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	*result = "success"

	w.enqueueIngestFollowups(ctx, client, args.RecordID, recordingID, log)

	return nil
}

// preflightIngestSHA256 は完成済み temp が既に揃っている場合、再生前に SHA-256 待ちを判定する。
func (w *IngestWorker) preflightIngestSHA256(ctx context.Context, client *mirakc.Client, recordID string, recordingID int64, initialRecord *mirakc.Record, tempPath string, file ingestFile, log *slog.Logger) (*string, bool, error) {
	if initialRecord.Recording.Status != db.RecordingStatusFinished || initialRecord.Content.Sha256 != nil {
		return nil, false, nil
	}
	expectedLen, err := client.HeadRecordStream(ctx, recordID)
	if err != nil {
		return nil, false, fmt.Errorf("HEAD record stream before temp replay: %w", err)
	}
	if expectedLen < 0 {
		return nil, false, nil
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat ingest temporary file before replay: %w", err)
	}
	if info.Size() != expectedLen {
		return nil, false, nil
	}
	return w.maybeWaitForSHA256(ctx, client, recordID, recordingID, tempPath, file, expectedLen, info.Size(), initialRecord.Recording.EndTime, log)
}

// maybeWaitForSHA256 は完成済み temp のハッシュ待ち期限を判定する。
// 待つ場合は temp を同期し、進捗行を消して呼び出し元に同じ job の snooze を返させる。
func (w *IngestWorker) maybeWaitForSHA256(ctx context.Context, client *mirakc.Client, recordID string, recordingID int64, tempPath string, file ingestFile, expectedLen, size int64, observedEndTime *mirakc.Milliseconds, log *slog.Logger) (*string, bool, error) {
	// HEAD の長さが不明なら転送済みを判定できない。content.length は照合の根拠に
	// 使わず、待たずに timeout_skipped で commit する。
	if expectedLen < 0 || size != expectedLen {
		return nil, false, nil
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat ingest temporary file before SHA-256 wait: %w", err)
	}
	now := time.Now()
	if !now.Before(ingestSHA256WaitDeadline(size, observedEndTime, info.ModTime(), now)) {
		return nil, false, nil
	}
	// Sync の間に届いたハッシュを拾うため、同期の後で record を取り直し、期限も再判定する。
	if err := file.Sync(); err != nil {
		return nil, false, fmt.Errorf("syncing ingest temp before SHA-256 wait: %w", err)
	}
	record, err := client.GetRecord(ctx, recordID)
	if err != nil {
		return nil, false, fmt.Errorf("refreshing mirakc record after syncing temp: %w", err)
	}
	if record.Recording.Status != db.RecordingStatusFinished {
		return nil, false, nil
	}
	if record.Content.Sha256 != nil {
		return record.Content.Sha256, false, nil
	}
	now = time.Now()
	endTime := record.Recording.EndTime
	if endTime == nil {
		endTime = observedEndTime
	}
	deadline := ingestSHA256WaitDeadline(size, endTime, info.ModTime(), now)
	if !now.Before(deadline) {
		return nil, false, nil
	}
	if err := sqlcgen.New(w.Pool).DeleteRecordingIngestProgress(ctx, recordingID); err != nil {
		return nil, false, fmt.Errorf("clearing ingest progress before SHA-256 wait: %w", err)
	}
	log.Info("ingest: content sha256 is pending; snoozing until recheck",
		"sha256_verification", "pending",
		"wait", time.Until(deadline),
		"deadline", deadline)
	return nil, true, nil
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
	// 原本があるなら対象を絞った reconcile をヒントとして投入する。
	enqueueEncodeReconcileFromContext(ctx, recordingID)
	if _, err := client.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (already committed)", "err", err)
	}
}

// transferIngestRecord は mirakc の追従 reader から record 固有の一時ファイルへ転送する。
// initialOffset は既存 temp を replay したバイト数で、プロセス再試行時の最初の
// Range 開始点になる。省略時は新規 temp の 0 バイトから始める。
//
// 書き込み先のエラーは mirakc の障害ではないので reader の再試行に入れず、そのまま返す。
func (w *IngestWorker) transferIngestRecord(ctx context.Context, client *mirakc.Client, recordID string, dst io.Writer, progress *ingestProgressReporter, initialOffset ...int64) (int64, *string, *mirakc.Milliseconds, error) {
	offset := int64(0)
	if len(initialOffset) > 0 {
		offset = initialOffset[0]
	}
	var expectedSHA256 *string
	var observedEndTime *mirakc.Milliseconds

	onRecord := func(record *mirakc.Record, currentOffset int64) error {
		status := record.Recording.Status
		if status == db.RecordingStatusCanceled || status == db.RecordingStatusFailed {
			return fmt.Errorf("mirakc record ended with status %q: %w", status, errIngestRecordEndedAbnormally)
		}
		if status != db.RecordingStatusRecording && status != db.RecordingStatusFinished {
			// The shared reader retries unknown statuses. Do not refresh progress
			// from a status it has not accepted.
			return nil
		}
		var expected *int64
		if record.Content.Length != nil {
			length := int64(*record.Content.Length)
			expected = &length
		}
		progress.observeProgress(ctx, currentOffset, expected)
		if status == db.RecordingStatusFinished && record.Content.Sha256 != nil {
			sha256 := *record.Content.Sha256
			expectedSHA256 = &sha256
		}
		if status == db.RecordingStatusFinished && record.Recording.EndTime != nil {
			endTime := *record.Recording.EndTime
			observedEndTime = &endTime
		}
		return nil
	}
	reader := mirakc.NewRecordFollowReader(ctx, client, recordID, offset, nil, mirakc.RecordFollowOptions{
		StallTimeout: w.StallTimeout,
		OnRecord:     onRecord,
		PollMin:      ingestFollowPace.pollMin,
		PollMax:      ingestFollowPace.pollMax,
		RetryDelay:   ingestFollowPace.retryDelay,
		// 正常に終わる Range では flush しない（間引きを無視すると追従中に秒 2 行になる）。
		OnRangeInterrupted: func(currentOffset, _ int64) {
			progress.flush(ctx, currentOffset)
		},
	})
	defer func() { _ = reader.Close() }()

	written, err := io.Copy(dst, reader)
	currentOffset := offset + written
	if err != nil {
		// io.Copy can fail because dst failed. That is a local storage error,
		// not a reason to retry the mirakc request. Keep the accepted byte count so a
		// slice deadline can persist the matching temp offset and hash state.
		return currentOffset, expectedSHA256, observedEndTime, err
	}
	return currentOffset, expectedSHA256, observedEndTime, nil
}

// normalizeContentSHA256 は mirakc の optional な SHA-256 表記を比較用の
// 小文字 hex に正規化する。空文字や非 hex の値は「使えるハッシュなし」として
// 扱う。値が不正な proxy / 旧実装で ingest 全体を永久に塞がないためである。
func normalizeContentSHA256(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	if len(normalized) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", false
	}
	return normalized, true
}

// recordIngestMetrics は転送結果のバイト数をメトリクスへ記録する。
func recordIngestMetrics(offset int64) {
	metrics.IngestBytes.Add(float64(offset))
}

// enqueueIngestFollowups はコミット済み ingest の encode / thumbnail 対象 reconcile と
// mirakc record の削除を行う。補助処理の失敗はログに記録して本処理を成功扱いにする。
//
// encode 投入は対象録画を絞った reconcile。判定は定期パスと同じ query を使う。
//
// thumbnail 投入も対象録画を絞った reconcile で、poster・再選択・seek_tiles を
// 定期パスと同じ query で判定する。
// TS scan もヒント。候補の真実は periodic reconcile が DB から取り直す。
// River クライアントが無いテスト経路では黙ってスキップする。
func (w *IngestWorker) enqueueIngestFollowups(ctx context.Context, client *mirakc.Client, recordID string, recordingID int64, log *slog.Logger) {
	enqueueEncodeReconcileFromContext(ctx, recordingID)
	if riverClient, clientErr := river.ClientFromContextSafely[pgx5.Tx](ctx); clientErr == nil {
		if enqueueErr := EnqueueCMDetectionIfNeeded(ctx, w.Pool, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue CM detection job", "recording_id", recordingID, "err", enqueueErr)
		}
		if _, enqueueErr := riverClient.Insert(ctx, jobs.ThumbnailReconcileArgs{RecordingID: recordingID}, nil); enqueueErr != nil {
			log.Error("ingest: failed to enqueue thumbnail reconcile", "recording_id", recordingID, "err", enqueueErr)
		}
		if enqueueErr := tsscan.EnqueueScan(ctx, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue TS scan", "recording_id", recordingID, "err", enqueueErr)
		}
	}
	if _, err := client.DeleteRecord(ctx, recordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (committed OK)", "err", err)
	}
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
// media_asset があれば、その recording_id を返す（無ければ 0, nil）。これは
// 転送前の安価なヒントであり、同時 ingest の決着ではない。採用の根拠は commit
// 内の media_assets INSERT と部分一意索引である。
//
// delete_reconcile も canonical orphan の unlink 前に同じ rel_path の filesystem
// lock と transaction-level advisory lock を取得するため、公開・回収の確定区間は
// この SELECT と独立に直列化される。ただしこの関数自体は転送前の安価な
// ヒントであり、ingest 同士の決着は commit 内の lock と media_assets の一意索引に
// 任せる。ここを一意性の最終判定に使わない。
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
// 通ってしまい、一時ファイル作成が "{media_dir}/sites/{site}" を通常ファイルとして
// 作ってしまう（以後その site 配下の ingest が全て MkdirAll で
// "not a directory" になる。docs/storage/contract.md §rel_path の名前空間
// 参照）。前置前に弾く（下記）。
func (w *IngestWorker) determineRelPath(ctx context.Context, args jobs.IngestJobArgs, client *mirakc.Client) (relPath, fullPath string, record *mirakc.Record, err error) {
	record, err = client.GetRecord(ctx, args.RecordID)
	if err != nil {
		return "", "", nil, fmt.Errorf("getting mirakc record: %w", err)
	}
	if cp := record.Recording.Options.ContentPath; cp != nil && *cp != "" {
		relPath = *cp
	} else {
		relPath = filepath.Base(record.Content.Path)
	}
	// 空の rel_path と予約名は、mirakc 由来か移行ライブラリ由来かによらず
	// catalog.SiteRelPath で同じように拒否する。
	relPath, err = catalog.SiteRelPath(args.Site, relPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("building site rel_path for mirakc record %s: %w", args.RecordID, err)
	}
	fullPath, err = mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return "", "", nil, err
	}
	return relPath, fullPath, record, nil
}

// commit は原本の公開プロトコルを 1 回実行する。
//
// DB transaction 内で media_assets の INSERT を先に行うことで、rel_path の unique
// index が同じ宛先への競合を予約する。さらに media root の専用 lock directory にある
// rel_path 固有 filesystem lock を DB transaction より先に取得する。これを先に
// 持つことで、DB セッションが失われても、そのセッション lock の解放後に古い
// goroutine が rename / fsync を続けて orphan cleanup と競合することがない。
// INSERT はまだ他セッションから見えないため、その transaction と filesystem lock
// を保持したまま temp -> canonical の atomic rename と親ディレクトリ fsync を行い、
// 最後にだけ transaction を commit する。
//
// rename 後の fsync / DB commit が失敗した場合は canonical file を消さない。tempPath
// は既に消えているので呼び出し側の cleanup は no-op になり、ファイルは orphan として
// aging 回収される。一方 rename 前の失敗では、呼び出し側が中身の不一致や record の
// cancel / fail と確定した場合だけ tempPath を消し、それ以外は次の試行へ残す。
func (w *IngestWorker) commit(ctx context.Context, recordingID int64, relPath, tempPath, fullPath string, size int64) error {
	fileLock, err := lockMediaRelPathFile(ctx, w.MediaDir, relPath)
	if err != nil {
		return fmt.Errorf("locking canonical file protocol: %w", err)
	}
	defer func() { _ = fileLock.Close() }()

	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMediaRelPathInTransaction(ctx, tx, relPath); err != nil {
		return err
	}

	q := newIngestCommitQueries(tx)

	_, err = q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
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

	// INSERT は一意性の予約であり、公開点ではない。canonical path を作るのは
	// ここからで、失敗時に DB transaction を rollback できる順序を保つ。
	if err := beforeIngestFilePublication(ctx, tx); err != nil {
		return fmt.Errorf("preparing canonical file publication: %w", err)
	}
	if err := renameIngestFile(tempPath, fullPath); err != nil {
		return fmt.Errorf("renaming ingest temporary file into canonical path: %w", err)
	}
	if err := syncIngestParentDir(fullPath); err != nil {
		return fmt.Errorf("syncing canonical parent directory: %w", err)
	}

	if err := commitIngestTransaction(ctx, tx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// resolveAndSnapshotEncodePolicy は「この録画の望ましい最終状態」を
// recording_encode_policy へ凍結する（行の存在そのものが「凍結済み」を
// 意味する。不変条件 3「コミット = DB 行」・不変条件 10「意味を持たない行を
// 作らない」）。呼び出し元の commit が原本 media_asset の INSERT と同じ tx で、
// かつ targeted encode reconcile の投入より必ず先に呼ぶ（順序が
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
	// cut だけになったときのクランプ。ルール単独・override 単独ではそれぞれ
	// live 無効時の規則を満たしていても、マージ結果として cut だけが生成されうる。
	// 原本 HLS を使えない live 無効構成では確認に再生が要り、再生に encode が要り、
	// encode に確認が要る循環になるので、**cut のプロファイルを落とす**
	// （意図は overrides に残るので、ユーザーが cut でないプロファイルを足せば
	// 戻る）。下のクランプと同じ向き（書く前に安全側へ倒す）。
	//
	// **このクランプを下の until_encoded クランプより先に置く。** 落とした結果
	// desired が空になった場合（cut だけのルール）は、下のクランプが同じ
	// tx の中で keepOriginal を安全側へ倒す --- ここで別の分岐を書くと、
	// 「cut を落とした後だけ効く規則」がもう 1 つ増える。
	if err := config.ValidateCutSelection(encodeProfiles, w.CutProfiles, w.LiveEnabled); err != nil {
		slog.Warn("encode policy: frozen profile selection is cut-only; dropping cut profiles",
			"recording_id", recordingID, "encode_profiles", encodeProfiles)
		kept := make([]string, 0, len(encodeProfiles))
		for _, name := range encodeProfiles {
			if _, isCut := w.CutProfiles[name]; !isCut {
				kept = append(kept, name)
			}
		}
		encodeProfiles = kept
	}

	if keepOriginal == "until_encoded" && len(encodeProfiles) == 0 {
		keepOriginal = "always"
	}

	return q.FreezeRecordingEncodePolicy(ctx, sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    recordingID,
		KeepOriginal:   keepOriginal,
		EncodeProfiles: encodeProfiles,
		CmDetect:       w.CMDetect.Enabled,
	})
}
