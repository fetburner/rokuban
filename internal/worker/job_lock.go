package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ingestJobLockKeyPrefix / encodeJobLockKeyPrefix / tsScanJobLockKeyPrefix は、それぞれのジョブの
	// プロセス生存確認用 advisory lock の名前空間。ジョブ ID ごとにキーを分け、
	// 他のロールの advisory lock と衝突しないようにする。
	ingestJobLockKeyPrefix = "rokuban:ingest:job:"
	encodeJobLockKeyPrefix = "rokuban:encode:job:"
	tsScanJobLockKeyPrefix = "rokuban:ts_scan:job:"

	// defaultJobLockTimeout は lock 用コネクションの取得と
	// pg_try_advisory_lock の両方に与える既定の上限。
	defaultJobLockTimeout = 10 * time.Second

	// jobLockHeartbeatTimeout は heartbeat のクエリ 1 回あたりの応答待ち上限。
	// これを超えると pgx が接続を閉じるので、クエリを送った後の DB 側・経路の遅延に
	// 対する境界はこの値である。クエリを送っていない間のクライアント側の停止
	// （CPU throttling・GC・SIGSTOP）には効かず、そちらの境界は
	// jobLockIdleSessionTimeout である。
	jobLockHeartbeatTimeout = 2 * time.Second
)

// jobLockHeartbeatInterval と jobLockIdleSessionTimeout は DB テストが差し替える
// （実時間の待ちを秒未満に縮めるため）ので const ではない。
var (
	// jobLockHeartbeatInterval は lease を更新する間隔。interval ≪
	// jobLockIdleSessionTimeout であることが前提で、この 2 つが「生きているジョブの
	// セッションは切れない / 止まったジョブのセッションは切れる」を決める。
	jobLockHeartbeatInterval = time.Second

	// jobLockIdleSessionTimeout は lock 用セッションにだけ設定する
	// idle_session_timeout。heartbeat が止まってから Postgres がこのセッションを
	// 終了するまでの猶予である。猶予が切れると advisory lock も解放され、回収側
	// （ingest の recoverStaleIngestJobs、encode / cm_detect の reconcile）が旧
	// running 行を回収できるようになる。
	//
	// 30 秒の根拠:
	//   - 生きているクライアントでは、サーバーから見たクエリ間隔の上限はおおよそ
	//     heartbeat 間隔（1 秒）+ 応答待ち上限（2 秒）の約 3 秒である（応答待ちが
	//     上限を超えれば pgx が接続を閉じる）。30 秒はその約 10 倍なので、正常に
	//     動いているセッションがこのタイマーで終了することはない
	//     （TestIngestJobLock_HeartbeatKeepsSessionAlive が timeout の 3 倍の間、
	//     切断を観測しないことを固定している）。
	//   - 短すぎる側の壊れ方: 生きたセッションを誤って終了させると、代替実行が旧実行と
	//     並走する。heartbeat がクエリを送っている時間は周期のごく一部なので、
	//     クライアント側の停止（k8s の CPU limit による throttling・GC・VM の一時停止）は
	//     ほぼ必ずクエリを送っていない間に起き、その耐性はこの値だけで決まる。縮めると
	//     その分だけ短い停止で lease が切れる。並走の帰結は利用者ごとに違う:
	//       - ingest: 壊れない。temp の flock と DB の一意 reservation が採用を決め、
	//         lock 喪失でも転送を cancel しないので、二重 pull の無駄が出るだけである。
	//       - encode: scratch はジョブ ID ごとで、代替（別 ID）とは衝突しない。canonical へは
	//         temp を lock の外で stage し、rel_path の lock と advisory xact lock の中で
	//         行を読み直してから rename する（publishEncoded / planEncodePublish）。
	//         計画時から rel_path が進んだか、既に同じ内容で active なら公開を飛ばす。
	//         並走した 2 本はどちらも ffmpeg を完走する（EncodeWorker の doc コメント）。
	//         xact lock が排他するのは ingest commit と孤児回収だけで、通常削除とは
	//         flock でしか排他されない（RWX 越しの flock は未検証）。
	//       - cm_detect: scratch はジョブ ID ごとで、代替（別 ID）とは衝突しない。
	//         結果は DB の Upsert である。局ロゴの上書きもその job 固有 scratch の
	//         中だけ（writeStationLogo 参照）。
	//   - 長すぎる側の壊れ方: プロセス死の回収が遅れる。ただし回収の tail は
	//     record_sweep（既定 5 分）/ encode・cm_detect の reconcile（既定 15 分）の
	//     周期で決まるので、それより十分短ければ差は出ない。
	//   - 実測（PostgreSQL 17.10）: この値のまま heartbeat を止めると 30.08 秒で
	//     バックエンドが pg_stat_activity から消え、別セッションが同じジョブの
	//     advisory lock を取得できた。SET を外すと同じ状態で 40 秒放置しても
	//     バックエンドは残り、lock は解放されない（クライアントは生きたまま無言なので
	//     TCP keepalive は失敗を報告しない）。pg 側のタイマーだけが根拠である。
	jobLockIdleSessionTimeout = 30 * time.Second
)

const jobLockHeldQuery = `
SELECT EXISTS (
    SELECT 1
    FROM pg_locks
    WHERE locktype = 'advisory'
      AND pid = pg_backend_pid()
      AND classid = (($1::bigint >> 32) & 4294967295)::oid
      AND objid = ($1::bigint & 4294967295)::oid
      AND objsubid = 1
      AND granted
)`

// advisoryLockKey は名前空間と値から pg_try_advisory_lock(bigint) 用のキーを作る。
// Postgres の hashtext() は安定性を保証された API ではないため、Go 側の FNV-1a
// を使う。衝突しても不要な再試行になるだけで、DB の一意制約による採用結果は
// 変わらない。
func advisoryLockKey(prefix, value string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix + value))
	return int64(h.Sum64())
}

func jobLockKey(prefix string, jobID int64) int64 {
	return advisoryLockKey(prefix, strconv.FormatInt(jobID, 10))
}

// jobLock はジョブの process-death 検出用セッション lock と、そのセッションの
// lease を更新する heartbeat を所有する。
//
// 生存の表現は「最後のクエリから jobLockIdleSessionTimeout 以内」である。heartbeat
// が止まったセッションは Postgres が終了させ、advisory lock も外れる。回収側
// （jobLock の取得に失敗した側）はこれをプロセス死の根拠にする。
//
// lock の利用目的（転送先や出力の排他など）は各ワーカー側で定義する。heartbeat
// が lock 喪失を検知しても、長時間処理の context をキャンセルする責務は持たない。
type jobLock struct {
	conn  *pgxpool.Conn
	key   int64
	label string

	stopHeartbeat chan struct{}
	heartbeatDone chan struct{}
	releaseOnce   sync.Once

	// checkHeldFunc は heartbeat の一過性/恒久エラーを注入するテストフック。
	// nil の場合は実 DB を照会する。
	checkHeldFunc func() (held, permanent bool, err error)
}

func newJobLock(conn *pgxpool.Conn, key int64, label string) *jobLock {
	return &jobLock{conn: conn, key: key, label: label}
}

func (l *jobLock) startHeartbeat() {
	if l.stopHeartbeat != nil {
		return
	}
	l.stopHeartbeat = make(chan struct{})
	l.heartbeatDone = make(chan struct{})
	go l.heartbeatLoop()
}

func (l *jobLock) heartbeatLoop() {
	defer close(l.heartbeatDone)

	ticker := time.NewTicker(jobLockHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopHeartbeat:
			return
		case <-ticker.C:
			if l.heartbeatTick() {
				return
			}
		}
	}
}

// heartbeatTick は heartbeat 1 回分の判定を行う。true を返した場合は、以後の
// heartbeat を止める。ただし長時間処理や recovery の transaction をキャンセルする
// 責務は持たない。
//
// このループの唯一の仕事は job lock の lease を更新することである（型の doc
// コメント、docs/recording/ingest.md 参照）。一過性の DB エラーではループを
// 止めない --- 止めると lease が切れ、セッションが Postgres に終了されて advisory
// lock が解放される。生存中のジョブをプロセス死と誤認すると、回収側が古い running
// 行を discard して代替ジョブを投入し、処理を二重実行することになる。止めるのは
// permanent（コネクション切断）と !held（lock 喪失の確定）のときだけ。
func (l *jobLock) heartbeatTick() bool {
	check := l.checkHeldFunc
	if check == nil {
		check = l.checkHeldAndClassify
	}

	held, permanent, err := check()
	if err != nil {
		if permanent {
			slog.Warn("job advisory lock heartbeat connection lost", "job", l.label, "err", err)
			return true
		}
		slog.Warn("job advisory lock heartbeat check failed transiently; continuing", "job", l.label, "err", err)
		return false
	}

	if !held {
		slog.Warn("job advisory lock was lost; continuing with worker-specific protocol", "job", l.label)
		return true
	}
	return false
}

func (l *jobLock) checkHeldAndClassify() (held, permanent bool, err error) {
	held, err = l.checkHeld()
	if err == nil {
		return held, false, nil
	}
	return false, l.isPermanentCheckHeldError(), err
}

func (l *jobLock) isPermanentCheckHeldError() bool {
	return l.conn == nil || l.conn.Conn().IsClosed()
}

func (l *jobLock) checkHeld() (bool, error) {
	if l.conn == nil {
		return false, fmt.Errorf("job advisory lock has no connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobLockHeartbeatTimeout)
	defer cancel()
	var held bool
	if err := l.conn.QueryRow(ctx, jobLockHeldQuery, l.key).Scan(&held); err != nil {
		return false, err
	}
	return held, nil
}

// stopHeartbeatLoop は recovery のように lock 用セッション自身で短い DB transaction
// を実行する呼び出し側が、同じ pgx connection への並行利用を避けるために使う。
func (l *jobLock) stopHeartbeatLoop() {
	if l.stopHeartbeat == nil {
		return
	}
	close(l.stopHeartbeat)
	<-l.heartbeatDone
	l.stopHeartbeat = nil
	l.heartbeatDone = nil
}

func (l *jobLock) release() {
	l.releaseOnce.Do(func() {
		l.stopHeartbeatLoop()
		if l.conn == nil {
			return
		}

		unlockCtx, cancel := context.WithTimeout(context.Background(), defaultJobLockTimeout)
		defer cancel()
		var stillHeld bool
		if err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&stillHeld); err != nil {
			slog.Warn("failed to release job advisory lock", "job", l.label, "err", err)
		} else if !stillHeld {
			// 明示 unlock の結果を確認する（ここまでのどこかで lock を失っている）。
			slog.Warn("job advisory lock was already lost before release", "job", l.label)
		}
		// プールへ返さずに捨てる。このセッションには idle_session_timeout が付いて
		// いて、unlock に失敗した場合の状態も信用できないため、他ジョブへ再利用させ
		// ない。puddle は枠（MaxConns）と統計を戻すのでリークはしない。
		_ = l.conn.Hijack().Close(context.Background())
	})
}

// acquireJobLock は Work の開始時にジョブ ID のセッションレベル advisory lock を
// 取得し、取得したセッションを lease 付きで返す。回収側も同じ prefix のキーを
// pg_try_advisory_lock で試し、取得できた場合に限って元プロセスが死んでセッション
// が切れたと確定する。
func acquireJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration, prefix, label string) (*jobLock, bool, error) {
	if timeout <= 0 {
		timeout = defaultJobLockTimeout
	}

	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring connection for %s advisory lock: %w", label, err)
	}

	key := jobLockKey(prefix, jobID)
	var acquired bool
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying %s advisory lock: %w", label, err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	// lock を取れたセッションにだけ lease の期限を設定する。pg_try_advisory_lock と
	// 同じ往復にはしない: !acquired / エラーの経路でも設定が付き、そのままプールへ
	// 戻ったコネクションが無関係のクエリを切られてしまう。SET は変数を取れないので
	// 値を埋め込む（値はこのパッケージの変数で、外から来ない）。
	if _, err := conn.Exec(acquireCtx, fmt.Sprintf("SET idle_session_timeout = '%dms'", jobLockIdleSessionTimeout.Milliseconds())); err != nil {
		// 設定できなかったセッションの状態は信用せず、lock を保持したままプールへ
		// 戻さずに捨てる。
		_ = conn.Hijack().Close(context.Background())
		return nil, false, fmt.Errorf("setting idle_session_timeout for %s advisory lock: %w", label, err)
	}

	lock := newJobLock(conn, key, label)
	lock.startHeartbeat()
	return lock, true, nil
}

// acquireIngestJobLock は ingest 用の job-id advisory lock を取得する。
func acquireIngestJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*jobLock, bool, error) {
	return acquireJobLock(ctx, pool, jobID, timeout, ingestJobLockKeyPrefix, fmt.Sprintf("ingest job %d", jobID))
}

// acquireEncodeJobLock は encode 用の job-id advisory lock を取得する。
func acquireEncodeJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*jobLock, bool, error) {
	return acquireJobLock(ctx, pool, jobID, timeout, encodeJobLockKeyPrefix, fmt.Sprintf("encode job %d", jobID))
}

// acquireTSScanJobLock は TS scan 用の job-id advisory lock を取得する。
func acquireTSScanJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*jobLock, bool, error) {
	return acquireJobLock(ctx, pool, jobID, timeout, tsScanJobLockKeyPrefix, fmt.Sprintf("TS scan job %d", jobID))
}
