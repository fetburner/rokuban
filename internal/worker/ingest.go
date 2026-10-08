package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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
	"github.com/fetburner/rokuban/internal/tsstat"
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

// ingestSHA256BytesPerSecond は finished 後に mirakc が content.sha256 を非同期計算する
// 速度（バイト/秒）の見積もり。待ちの上限は時間ではなくこの速度で固定する ---
// 計算量は録画サイズに比例するので、時間の固定値では大きい録画ほどほぼ必ず
// timeout_skipped になる。報告例の実測は 2.35 GB を約 4 分（約 9.8 MB/s）で、
// 25% の余裕を含めて 7.8 MB/s とする。計測点が 1 件のため設定キーにはしない。
// 地デジ 30 分（約 3.8 GB）で約 8 分、BS 2 時間（約 20 GB）で約 43 分になる
// （この除算の結果であり、実機での待ち時間の測定ではない）。
var ingestSHA256BytesPerSecond int64 = 7_800_000

// ingestSHA256MinWait は小さい録画でも mirakc がハッシュ計算を始める猶予として待つ下限。
const ingestSHA256MinWait = 10 * time.Second

// ingestSHA256WaitFor は転送済みバイト数 size の SHA-256 を待つ上限を返す。size は
// HEAD の長さが不明（-1）でも常に既知の書き込みバイト数を渡す。
//
// 待ちは 1 回の snooze で行う。短い刻みで複数回 snooze すると、再開ごとに temp 全体を
// replay してハッシュを復元し直すことになり、数十 GB で読み直しが何十回にもなる。
// 代償は、ハッシュが上限より早く届いても再開まで commit が遅れること。
func ingestSHA256WaitFor(size int64) time.Duration {
	if size < 0 {
		size = 0
	}
	seconds := (size + ingestSHA256BytesPerSecond - 1) / ingestSHA256BytesPerSecond
	return max(time.Duration(seconds)*time.Second, ingestSHA256MinWait)
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

// replayIngestTempFile は既存 temp の全バイトを新しい hasher / tsstat.Counter に
// 通し、再開後にもファイル全体のハッシュ・ドロップ統計・PCR 基準が残るようにする。
// sink は replay 中だけ io.Discard を向き、呼び出し元が replay 後に実ファイルへ
// 切り替える。読み出しは context のキャンセルを chunk 境界で検知し、temp は残す。
func replayIngestTempFile(ctx context.Context, path string, counter *tsstat.Counter) (int64, error) {
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
	n, copyErr := io.Copy(counter, &ingestReplayReader{ctx: ctx, r: f})
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
	// クランプ（resolveAndSnapshotEncodePolicy）と、完了後のヒント投入
	// （enqueueMissingEncodesFromContext）が使う。
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

// Timeout は River の総時間タイムアウトを無効化する。
//
// ingest は数百 MB〜数十 GB のバイト転送で、所要時間は録画長と回線速度で決まる。
// River の既定（JobTimeoutDefault = 1 分）では実際の録画がまず完走しない。
//
// 総時間で切らない代わりに、進捗が止まったことを RecordFollowReader が検知して切り直す
// （StallTimeout）。「タイムアウトは総時間でなくストール検知」という M1-5-2 の
// 設計はこれが揃って初めて成立する。
//
// -1 は Work の defer が中断を判定する前提でもある（正にするとタイムアウトが
// 中断と見分けられず、結果メトリクスから落ちる）。
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

// ingestJobSnoozeCount は River metadata に記録された、このジョブの snooze 回数を返す。
// 現在 ingest で snooze するのは SHA-256 待ちの 1 回だけ（ingestSHA256WaitFor が
// サイズから決めた長さ）なので、snooze 済みの再開でハッシュが null なら commit 時の照合を skip する。
// 上限前の再開は watcher（watcher.processRecord）がハッシュ非 nil のときしか起こさないので、
// null での再開は上限を過ぎた再開に限られる。この前提は watcher の起こす条件に依存する。
func ingestJobSnoozeCount(metadata []byte) (int, error) {
	if len(metadata) == 0 {
		return 0, nil
	}
	var state struct {
		Snoozes int `json:"snoozes"`
	}
	if err := json.Unmarshal(metadata, &state); err != nil {
		return 0, fmt.Errorf("decoding River job metadata: %w", err)
	}
	if state.Snoozes < 0 {
		return 0, fmt.Errorf("invalid River snooze count %d", state.Snoozes)
	}
	return state.Snoozes, nil
}

// Work は ingest ジョブを実行する。ストリーム取得・TS 統計収集・DB コミット・エッジ削除を行う。
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
	defer func() {
		// result は success / failure / canceled の 3 値。**この 3 値の外に増やさない**
		// （低カーディナリティが前提。record id や理由は入れない）。
		//
		// **River の soft stop（graceful stop）は数えない。** 中断はジョブの結末では
		// ない --- River は attempt を消費せず行を available に戻し、次のプロセスが
		// 再開して、そこで結末を 1 回だけ数える。ここで数えると 1 ジョブが「中断 +
		// 再開後の結末」の 2 回で数えられる。中断が繰り返されているかは
		// rokuban_uningested_records / _bytes に積まれる。
		//
		// 判定は River の isSoftStopCancelError（internal/jobexecutor。internal
		// パッケージなので import できない）と同じ材料で行う。条件は work ctx の cause が
		// セットされていて、かつ戻り値が context.Canceled か cause そのものを包むこと。
		// cause は 2 つある:
		//
		//   - Stop / StopAndCancel / soft stop timer が撃つ ErrStop
		//   - Client.JobCancel の rivertype.ErrJobCancelledRemotely（rokuban に
		//     呼び出し元は無いが、区別しないと下の err == nil の判断が崩れる）
		//
		// **2 つに限られるのは 2 つの前提による。** SoftStopTimeout > 0
		// （resolveSoftStopTimeout が強制）なので work ctx は start ctx の Canceled を
		// 継がず、Timeout が -1 なので DeadlineExceeded も来ない。Timeout を正にすると
		// タイムアウトが errors.Is(err, cause) に掛かって数えられなくなる（River は
		// attempt を消費するのに failure に乗らない）。
		//
		// **errors.Is(err, cause) の項が要る。** Go の net/http は ctx が取り消されると
		// ctx.Err() ではなく context.Cause(ctx) を返す（transport.go）。Stop の瞬間に
		// mirakc への HTTP 要求が飛んでいると、返る err は ErrStop を包むが
		// context.Canceled を包まない。
		//
		// 未解決（未測定の窓）: ctx 由来でない context.Canceled を err が包む場合、
		// defer の評価と River の評価の間（μs 単位）に soft stop が重なると failure が
		// 1 回余分に乗りうる（実例: stall 検知の cancel が再試行予算の超過で返るとき）。
		// Work 側では塞げず、River も同じ後読みをしている。
		cause := context.Cause(ctx)
		var snoozeErr *river.JobSnoozeError
		if errors.As(err, &snoozeErr) {
			// SHA-256 待ちは最終結果ではない。River は attempt を消費せず再開するため、
			// 成功・失敗の件数や処理時間にも数えない。
			return
		}
		remote := errors.Is(cause, river.ErrJobCancelledRemotely)
		if cause != nil && !remote &&
			(errors.Is(err, context.Canceled) || errors.Is(err, cause)) {
			return
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

	// Work の開始から commit まで、ジョブ ID 固有の advisory lock を保持する。
	// record_sweep の回収側が同じキーを pg_try できた場合だけ、元プロセスが死んで
	// セッションが解放されたと確定できる。セッションには idle_session_timeout が
	// 付いており、heartbeat が lease を更新し続ける限り切れない。heartbeat は
	// canonical file の排他には使わない。
	jobLock, acquired, err := acquireIngestJobLock(ctx, w.Pool, job.ID, defaultJobLockTimeout)
	if err != nil {
		return fmt.Errorf("acquiring ingest job lock: %w", err)
	}
	if !acquired {
		// 断定はしない: この分岐には、別プロセスが本当に実行中の場合だけでなく、
		// record_sweep の回収側が同じキーを一瞬 try して保持している場合も落ちる。
		log.Warn("ingest: job advisory lock is held by another session, deferring", "job_id", job.ID)
		return fmt.Errorf("ingest: job %d advisory lock is held by another session; deferring", job.ID)
	}
	defer jobLock.release()

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
	snoozeCount, err := ingestJobSnoozeCount(job.Metadata)
	if err != nil {
		return err
	}

	if err := w.ingestResolvedRecord(ctx, client, args, recordingID, expectedBytes, snoozeCount, log, &result); err != nil {
		return err
	}
	return nil
}

func (w *IngestWorker) ingestResolvedRecord(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, expectedBytes *int64, snoozeCount int, log *slog.Logger, result *string) error {
	relPath, fullPath, err := w.determineRelPath(ctx, args, client)
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
		}
		_ = f.Close()
		// openIngestFile のテスト差し替えが underlying fd を閉じずに失敗
		// する場合にもロックを残さない。
		_ = tempLock.Close()
	}()

	// 一時ファイル方式では canonical path を転送中に一度も触らない。したがって
	// job lock の heartbeat がセッション喪失を検知しても、古い転送を context
	// cancel する必要はない。同じ record の別試行は temp の flock で直列化され、
	// 異なる record の競合は DB の unique reservation が採用を一つに決める。
	ingestCtx := ctx

	hasher := sha256.New()
	// Counter は replay と新規転送で同じインスタンスを使う。replay 中はファイルへ
	// 書き戻さず、既存バイトを hash / 統計へだけ通してから sink を f に切り替える。
	sink := &hashingWriter{w: io.Discard, h: hasher}
	counter := tsstat.NewCounter(sink)
	offset, err := replayIngestTempFile(ingestCtx, tempPath, counter)
	if err != nil {
		return err
	}
	sink.w = f

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
	progress.start(ingestCtx, offset)
	// progressWriter は counter の外側に置く（io.Copy → progressWriter →
	// counter → hashingWriter → f）。TS 統計は counter が数え、SHA-256 は
	// hashingWriter がファイルに受理された同じ転送バイト列を 1 パスで受け取る。
	dst := &progressWriter{
		w:       counter,
		written: offset,
		onWrite: func(written int64) { progress.report(ingestCtx, written) },
	}

	var expectedSHA256 *string
	offset, expectedSHA256, err = w.transferIngestRecord(ingestCtx, client, args.RecordID, dst, progress, offset)
	if err != nil {
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
	expectedLen, err := client.HeadRecordStream(ingestCtx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		removeTemp = true
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}
	sha256Verification := "skipped"
	if expectedSHA256 == nil && snoozeCount == 0 {
		// Range 転送が finished 後の追記まで drain されてから待つ。snooze で Work を
		// 終えても temp は残り、次の試行が replay して hasher と TS 統計を復元する。
		// River metadata の snoozes が 1 になった再開でも null なら、旧 mirakc または
		// 計算失敗として timeout_skipped で commit する（API から両者を区別できない）。
		wait := ingestSHA256WaitFor(offset)
		log.Info("ingest: content sha256 is pending; snoozing before commit",
			"sha256_verification", "pending", "wait", wait)
		return river.JobSnooze(wait)
	}
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
	// 失敗したら DB へ登録せず、mirakc の record を保持して再試行させる。途中の
	// 定期 fsync は行わない（S3 系 FUSE 上で転送途中の実体化を増やさないため）。
	syncStarted := time.Now()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}

	// pid_type_changes > 0 は録画中に PMT が PID を付け替えたということ。
	// 種別は最後に見たものを採用するので、変化そのものはここにしか残らない
	// （docs/recording.md §1「例外の境界」）。
	log.Info("ingest: transfer complete", "bytes", offset,
		"drops", counter.TotalDrops(), "errors", counter.TotalErrors(),
		"scrambled", counter.TotalScrambled(),
		"pid_type_changes", counter.TypeChanges(),
		"sha256_verification", sha256Verification,
		"fsync_duration", time.Since(syncStarted))

	recordIngestMetrics(offset, counter)

	if err := w.commit(ingestCtx, recordingID, relPath, tempPath, fullPath, offset, counter); err != nil {
		return fmt.Errorf("committing ingest: %w", err)
	}

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	*result = "success"

	w.enqueueIngestFollowups(ctx, client, args.RecordID, recordingID, log)

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
	enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID, w.CutProfiles)
	if _, err := client.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (already committed)", "err", err)
	}
}

// transferIngestRecord は mirakc の追従 reader から record 固有の一時ファイルへ転送する。
// initialOffset は既存 temp を replay したバイト数で、プロセス再試行時の最初の
// Range 開始点になる。省略時は新規 temp の 0 バイトから始める。
//
// 書き込み先のエラーは mirakc の障害ではないので reader の再試行に入れず、そのまま返す。
func (w *IngestWorker) transferIngestRecord(ctx context.Context, client *mirakc.Client, recordID string, dst io.Writer, progress *ingestProgressReporter, initialOffset ...int64) (int64, *string, error) {
	offset := int64(0)
	if len(initialOffset) > 0 {
		offset = initialOffset[0]
	}
	var expectedSHA256 *string

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
		return nil
	}
	reader := mirakc.NewRecordFollowReader(ctx, client, recordID, offset, nil, mirakc.RecordFollowOptions{
		StallTimeout: w.StallTimeout,
		OnRecord:     onRecord,
		// 正常に終わる Range では flush しない（間引きを無視すると追従中に秒 2 行になる）。
		OnRangeInterrupted: func(currentOffset, _ int64) {
			progress.flush(ctx, currentOffset)
		},
	})
	defer func() { _ = reader.Close() }()

	written, err := io.Copy(dst, reader)
	if err != nil {
		// io.Copy can fail because dst failed. That is a local storage error,
		// not a reason to retry the mirakc request.
		return 0, nil, err
	}
	return offset + written, expectedSHA256, nil
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
	enqueueMissingEncodesFromContext(ctx, w.Pool, recordingID, w.CutProfiles)
	if riverClient, clientErr := river.ClientFromContextSafely[pgx5.Tx](ctx); clientErr == nil {
		if enqueueErr := EnqueueCMDetectionIfNeeded(ctx, w.Pool, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue CM detection job", "recording_id", recordingID, "err", enqueueErr)
		}
		if enqueueErr := EnqueueThumbnailIfNeeded(ctx, w.Pool, riverClient, recordingID); enqueueErr != nil {
			log.Error("ingest: failed to enqueue thumbnail job", "recording_id", recordingID, "err", enqueueErr)
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
	// 空の rel_path と予約名は、mirakc 由来か移行ライブラリ由来かによらず
	// catalog.SiteRelPath で同じように拒否する。
	relPath, err = catalog.SiteRelPath(args.Site, relPath)
	if err != nil {
		return "", "", fmt.Errorf("building site rel_path for mirakc record %s: %w", args.RecordID, err)
	}
	fullPath, err = mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return "", "", err
	}
	return relPath, fullPath, nil
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
func (w *IngestWorker) commit(ctx context.Context, recordingID int64, relPath, tempPath, fullPath string, size int64, counter *tsstat.Counter) error {
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

	statParams := make([]sqlcgen.InsertDropStatParams, 0, len(pids))
	positionParams := make([]sqlcgen.InsertDropPositionParams, 0)
	for _, pid := range pids {
		s := stats[pid]
		// 分類できなかった PID は種別なし（NULL）。空文字を「未分類」という値として
		// 永続化しない（M2-13, issue #24）。
		var pidType *string
		if s.Type != "" {
			t := s.Type
			pidType = &t
		}
		statParams = append(statParams, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          int32(pid),
			Packets:      s.Packets,
			Drops:        s.Drops,
			Errors:       s.Errors,
			Scrambled:    s.Scrambled,
			PidType:      pidType,
		})
		for _, position := range s.Positions {
			positionParams = append(positionParams, sqlcgen.InsertDropPositionParams{
				MediaAssetID: assetID,
				ByteOffset:   position.ByteOffset,
				Pid:          int32(pid),
				ElapsedMs:    position.ElapsedMs,
			})
		}
	}
	// drop_stats と drop_positions はどちらも同じ tx に属する不可逆な観測で、
	// 片方だけ別 transaction にしない。epgBatchSize ごとに chunk するのは
	// epg.go の syncServices / syncPrograms と同じ理由（メモリと 1 バッチあたりの
	// 所要を抑える）。chunks は空スライスに対して 0 回しか yield しないので、
	// 空の batch は pgx に送られない。
	for chunk := range chunks(statParams, epgBatchSize) {
		if err := execBatch(q.InsertDropStat(ctx, chunk)); err != nil {
			return fmt.Errorf("inserting drop_stats batch: %w", err)
		}
	}
	for chunk := range chunks(positionParams, epgBatchSize) {
		if err := execBatch(q.InsertDropPosition(ctx, chunk)); err != nil {
			return fmt.Errorf("inserting drop_positions batch: %w", err)
		}
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
