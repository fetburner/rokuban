package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// relPathLockKeyPrefix は advisory lock キーの名前空間。internal/role.lockKey
// （"rokuban:" + role）と 1 引数 pg_try_advisory_lock(bigint) の鍵空間を共有する
// ため、衝突しない接頭辞を付ける。
const relPathLockKeyPrefix = "rokuban:ingest:rel_path:"

// defaultRelPathLockTimeout は IngestWorker.RelPathLockTimeout が未設定
// （0）のときに使う既定値。ロック用コネクションの取得（pool.Acquire）と
// pg_try_advisory_lock の両方に与える上限。
const defaultRelPathLockTimeout = 10 * time.Second

// relPathLockHeartbeatInterval は、長時間の ingest 中もロック用セッションを
// idle にしないための疎通間隔。切断検知の窓もこの間隔を上限の目安にする。
const relPathLockHeartbeatInterval = time.Second

// relPathLockHeartbeatTimeout は heartbeat 1 回の応答を待つ上限。応答を待ち
// 続けている間に後続 ingest が同じ rel_path の書き込みを始めると、古い実行を
// 止められない窓が広がるので、失敗側に倒して転送を中断する。
const relPathLockHeartbeatTimeout = 2 * time.Second

// relPathLockMaxTransientFailures は、checkHeld が一過性のエラー（接続は生きて
// いるがクエリが失敗・タイムアウトした）を返しても、それだけでは markLost しない
// 許容連続回数。checkpoint / failover / pgbouncer によるクエリの数秒のスタック
// 1 回で、99% 進んだ転送を再ダウンロードに戻したくない（issue #679 レビュー）。
// N-1 回までは無視して次の heartbeat を待ち、N 回連続して初めて恒久喪失とみなす。
// 接続断そのもの（isPermanentCheckHeldError）と held=false（ロックを保持して
// いないという確定した事実）はこのカウントの対象外で、常に即座に markLost する。
const relPathLockMaxTransientFailures = 3

const relPathLockHeldQuery = `
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

// relPathLockKey は relPath から pg_try_advisory_lock(bigint) 用のキーを作る。
// Postgres 組み込みの hashtext() ではなく Go 側の hash/fnv を使う ---
// hashtext はドキュメント化された安定 API ではなく、バージョン間の値の安定性が
// 保証されていない（internal/role.lockKey と同じ判断）。
//
// ハッシュ衝突の帰結は「別の rel_path を持つ ingest 同士がロックを取り合い、
// 一方が不要な再試行になる」ことであり、破損ではない --- ロックを取れなかった
// 側は checkRelPathConflict にすら進まず単に再試行するだけなので、安全側に
// 倒れる。衝突確率は測っていないので数値としては断言しない。
func relPathLockKey(relPath string) int64 {
	h := fnv.New64a()
	h.Write([]byte(relPathLockKeyPrefix + relPath))
	return int64(h.Sum64())
}

// relPathLock は rel_path の Postgres **セッションレベル** advisory
// lock と、そのセッションの heartbeat を所有する。heartbeat が接続断または
// セッション上のロック喪失を検知すると lost を閉じ、ingest 側の context を
// キャンセルさせる。
type relPathLock struct {
	conn    *pgxpool.Conn
	key     int64
	relPath string

	stopHeartbeat chan struct{}
	heartbeatDone chan struct{}
	lost          chan struct{}
	releaseOnce   sync.Once
	lostOnce      sync.Once

	// checkHeldFunc は heartbeatTick が呼ぶフック。nil なら
	// l.checkHeldAndClassify を使う（本番の既定）。テストが実 DB 無しで
	// 一過性/恒久エラーを注入するために差し替える（openIngestFile と同じ形）。
	checkHeldFunc func() (held, permanent bool, err error)
}

func (l *relPathLock) markLost() {
	l.lostOnce.Do(func() { close(l.lost) })
}

func (l *relPathLock) heartbeatLoop() {
	defer close(l.heartbeatDone)

	ticker := time.NewTicker(relPathLockHeartbeatInterval)
	defer ticker.Stop()

	var consecutiveFailures int
	for {
		select {
		case <-l.stopHeartbeat:
			return
		case <-ticker.C:
			if l.heartbeatTick(&consecutiveFailures) {
				return
			}
		}
	}
}

// heartbeatTick は heartbeat 1 回分の判定を行い、markLost して heartbeatLoop を
// 止めるべきなら true を返す。consecutiveFailures は heartbeatLoop がループを
// 跨いで保持する、一過性エラーの連続回数のカウンタ。
func (l *relPathLock) heartbeatTick(consecutiveFailures *int) bool {
	check := l.checkHeldFunc
	if check == nil {
		check = l.checkHeldAndClassify
	}

	held, permanent, err := check()
	if err != nil {
		if permanent {
			slog.Warn("ingest: rel_path advisory lock heartbeat failed", "rel_path", l.relPath, "err", err)
			l.markLost()
			return true
		}
		*consecutiveFailures++
		if *consecutiveFailures >= relPathLockMaxTransientFailures {
			slog.Warn("ingest: rel_path advisory lock heartbeat failed", "rel_path", l.relPath, "err", err, "consecutive_failures", *consecutiveFailures)
			l.markLost()
			return true
		}
		slog.Warn("ingest: rel_path advisory lock heartbeat check failed transiently, retrying",
			"rel_path", l.relPath, "err", err, "consecutive_failures", *consecutiveFailures)
		return false
	}

	*consecutiveFailures = 0
	if !held {
		slog.Warn("ingest: rel_path advisory lock was lost during transfer", "rel_path", l.relPath)
		l.markLost()
		return true
	}
	return false
}

// checkHeldAndClassify は checkHeld を呼び、エラーを一過性/恒久で分類する
// （checkHeldFunc の本番既定）。
func (l *relPathLock) checkHeldAndClassify() (held, permanent bool, err error) {
	held, err = l.checkHeld()
	if err == nil {
		return held, false, nil
	}
	return false, l.isPermanentCheckHeldError(), err
}

// isPermanentCheckHeldError は checkHeld のエラーが接続断など恒久的な喪失を
// 示すかを判定する。conn.Conn().IsClosed() は、直前のクエリが失敗した時点で
// pgx が接続を既に破棄済みか（pg_terminate_backend やネットワーク断は検出後
// ただちにそうなる）を見る。false ならクエリのタイムアウト等の一過性とみなし、
// relPathLockMaxTransientFailures 回までは次の heartbeat に委ねる。
func (l *relPathLock) isPermanentCheckHeldError() bool {
	return l.conn.Conn().IsClosed()
}

func (l *relPathLock) checkHeld() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), relPathLockHeartbeatTimeout)
	defer cancel()

	var held bool
	if err := l.conn.QueryRow(ctx, relPathLockHeldQuery, l.key).Scan(&held); err != nil {
		return false, err
	}
	return held, nil
}

func (l *relPathLock) isLost() bool {
	select {
	case <-l.lost:
		return true
	default:
		return false
	}
}

func (l *relPathLock) release() {
	l.releaseOnce.Do(func() {
		close(l.stopHeartbeat)
		<-l.heartbeatDone

		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), defaultRelPathLockTimeout)
		defer unlockCancel()

		var stillHeld bool
		if err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&stillHeld); err != nil {
			slog.Warn("ingest: failed to release rel_path advisory lock", "rel_path", l.relPath, "err", err)
		} else if !stillHeld {
			// pgxpool の Release() がセッション状態（advisory lock）を暗黙に
			// リセットするとは仮定しない --- 明示的に unlock し、戻り値
			// （pg_advisory_unlock は「保持していなかった」場合 false を返す）
			// を見る。false は heartbeat が転送中にロック喪失を検知したか、
			// それ以外の理由でセッションのロックが解放されていたことを示す。
			slog.Warn("ingest: rel_path advisory lock was already lost before release", "rel_path", l.relPath)
		}
		l.conn.Release()
	})
}

// acquireRelPathLockWithHeartbeat は rel_path の Postgres **セッションレベル** advisory
// lock を **`pg_try_advisory_lock`（ノンブロッキング）** で試行する。
// internal/role.TryAcquire と同じ形（`pool.Acquire` したコネクションを保持し
// 続ける限りロックが維持され、コネクション切断で自動解放される）。
//
// **セッションレベルであってトランザクションレベル（`pg_advisory_xact_lock`）
// ではない。** ingest の転送は数時間かかりうる。xact ロックだと同じ長さの
// トランザクションを開いたまま転送することになり、HEAD 照合や commit まで
// その 1 トランザクションに縛られる。セッションロックはコネクションの生存期間
// にだけ紐づくので、commit は別の短命なトランザクションとして自由に行える。
//
// **ノンブロッキング（`pg_try_advisory_lock`）であってブロッキング版
// （`pg_advisory_lock`）でもない。** ブロッキング版だと、ingest のキュー枠
// （site あたり 1〜2、docs/recording/ingest.md §5.4）を「待ち」で丸ごと
// 塞いでしまう。`pg_try_*` で即座に負けを確定させ、River のバックオフに
// 委ねる方が安全（他の rel_path を待っている転送を無関係に足止めしない）。
//
// ingest の River タイムアウトは無効（IngestWorker.Timeout が -1）なので、
// `pool.Acquire` と `pg_try_advisory_lock` は timeout 付きの ctx の下で行う
// --- 素の ctx のままだとプール枯渇時に無期限に待ち、ジョブが二度と終わらずに
// ハングする（internal/db/db.go の roleConnBudget コメント参照。実行中の
// ingest 1 本ごとにこのロック用コネクションを 1 本、転送が終わるまで長期保持する）。
//
// acquired=false はロック取得の失敗（既に別の ingest ジョブが同じ rel_path を
// 転送中）を示す通常の敗北であり、err ではない。
//
// heartbeat は転送中もこの接続へ定期的にクエリを送り、接続断または advisory
// lock の喪失を検知したら lost を閉じる。ingest は lost を見て転送 context を
// キャンセルし、DB commit とエッジ record の削除へ進まない。
//
// **一過性のクエリ失敗（checkpoint / failover / pgbouncer によるスタック、
// context.DeadlineExceeded を含む）だけでは lost にしない。** 接続断・ロック
// 喪失（held=false）は確定した事実として即座に lost にするが、それ以外の
// エラーは relPathLockMaxTransientFailures 回連続するまでリトライに委ねる ---
// そうしないと DB の 2 秒のレイテンシ 1 回で、99% 進んだ転送が 0 バイトから
// 再試行になる（issue #679 レビュー）。
//
// heartbeat による検知までの窓は残るため、これだけで相互排除の絶対的な証明に
// はならない。ただし「接続が切れたらロックは解放されるだけ」と放置せず、旧実行
// が後続実行と同じ宛先へ書き続ける時間を限定し、通常時は heartbeat 自体が idle
// timeout による切断を防ぐ。残る窓の判断は docs/recording/ingest.md §5.3 に記録する。
func acquireRelPathLockWithHeartbeat(ctx context.Context, pool *pgxpool.Pool, relPath string, timeout time.Duration) (*relPathLock, bool, error) {
	if timeout <= 0 {
		timeout = defaultRelPathLockTimeout
	}

	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring connection for rel_path lock: %w", err)
	}

	key := relPathLockKey(relPath)
	var acquired bool
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying rel_path advisory lock: %w", err)
	}

	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	lock := &relPathLock{
		conn:          conn,
		key:           key,
		relPath:       relPath,
		stopHeartbeat: make(chan struct{}),
		heartbeatDone: make(chan struct{}),
		lost:          make(chan struct{}),
	}
	go lock.heartbeatLoop()
	return lock, true, nil
}
