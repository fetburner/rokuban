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

// relPathLockKeyPrefix は rel_path advisory lock キーの名前空間。internal/role.lockKey
// （"rokuban:" + role）と 1 引数 pg_try_advisory_lock(bigint) の鍵空間を共有する
// ため、衝突しない接頭辞を付ける。
const relPathLockKeyPrefix = "rokuban:ingest:rel_path:"

// ingestJobLockKeyPrefix は ingest ジョブのプロセス生存確認用 advisory lock の
// 名前空間。ジョブ ID ごとにキーを分け、rel_path lock とも衝突しないようにする。
// Work の開始から commit まで、rel_path lock と同じ PostgreSQL セッションで保持する。
const ingestJobLockKeyPrefix = "rokuban:ingest:job:"

// defaultRelPathLockTimeout は IngestWorker.RelPathLockTimeout が未設定
// （0）のときに使う既定値。ロック用コネクションの取得（pool.Acquire）と
// pg_try_advisory_lock の両方に与える上限。
const defaultRelPathLockTimeout = 10 * time.Second

// relPathLockHeartbeatInterval は、長時間の ingest 中もロック用セッションを
// idle にしないための疎通間隔。切断検知の窓もこの間隔を上限の目安にする。
const relPathLockHeartbeatInterval = time.Second

// relPathLockHeartbeatTimeout は checkHeld のクエリ 1 回あたりの応答を待つ上限
// （ループでキーごとに個別の budget として使う。checkHeld のコメント参照）。
// 応答を待ち続けている間に後続 ingest が同じ rel_path の書き込みを始めると、
// 古い実行を止められない窓が広がるので、失敗側に倒して転送を中断する。
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

// advisoryLockKey は名前空間と値から pg_try_advisory_lock(bigint) 用のキーを作る。
// Postgres 組み込みの hashtext() ではなく Go 側の hash/fnv を使う ---
// hashtext はドキュメント化された安定 API ではなく、バージョン間の値の安定性が
// 保証されていない（internal/role.lockKey と同じ判断）。
//
// ハッシュ衝突の帰結は「別の rel_path を持つ ingest 同士がロックを取り合い、
// 一方が不要な再試行になる」ことであり、破損ではない --- ロックを取れなかった
// 側は checkRelPathConflict にすら進まず単に再試行するだけなので、安全側に
// 倒れる。衝突確率は測っていないので数値としては断言しない。
func advisoryLockKey(prefix, value string) int64 {
	h := fnv.New64a()
	h.Write([]byte(prefix + value))
	return int64(h.Sum64())
}

// relPathLockKey は relPath から rel_path 用のキーを作る。
func relPathLockKey(relPath string) int64 {
	return advisoryLockKey(relPathLockKeyPrefix, relPath)
}

// ingestJobLockKey は River のジョブ ID から ingest 専用の advisory lock キーを
// 作る。キー値そのものは永続化せず、接頭辞を変えない限りプロセス間で再現できればよい。
func ingestJobLockKey(jobID int64) int64 {
	return advisoryLockKey(ingestJobLockKeyPrefix, strconv.FormatInt(jobID, 10))
}

// relPathLock は ingest が使う Postgres **セッションレベル** advisory lock の集合と、
// rel_path lock を保持する場合の heartbeat を所有する。heartbeat が接続断または
// セッション上のロック喪失を検知すると lost を閉じ、ingest 側の context を
// キャンセルさせる。ジョブ lock と rel_path lock は同じ接続へ積む。
//
// keys は保持している advisory lock の唯一の権威（取得順。production は
// [job, rel_path] の 2 本）。個別フィールドで重複して持たない。
type relPathLock struct {
	conn *pgxpool.Conn
	keys []heldAdvisoryKey

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

type heldAdvisoryKey struct {
	key   int64
	label string
}

func newRelPathLock(conn *pgxpool.Conn, key int64, label string) *relPathLock {
	return &relPathLock{
		conn: conn,
		keys: []heldAdvisoryKey{{key: key, label: label}},
		lost: make(chan struct{}),
	}
}

// advisoryKeys はこのセッションが保持している全キーを返す（keys が唯一の権威）。
func (l *relPathLock) advisoryKeys() []heldAdvisoryKey {
	return l.keys
}

// label はログ用の代表ラベル。最後に取得した（＝最も具体的な）キーのラベルを使う
// --- production では rel_path lock を追加した後は rel_path、ジョブ lock しか
// 持っていない間は job のラベルになる。
func (l *relPathLock) label() string {
	return l.keys[len(l.keys)-1].label
}

// startHeartbeat は heartbeat goroutine を起動する。呼び出し元は常に
// acquireRelPath という単一の goroutine（ingest の Work 自身）からしか呼ばない
// ため、複数 goroutine からの競合を気にする sync.Once は要らない --- 二重起動を
// 防ぎたいだけなら nil チェックで足りる。
func (l *relPathLock) startHeartbeat() {
	if l.stopHeartbeat != nil {
		return
	}
	l.stopHeartbeat = make(chan struct{})
	l.heartbeatDone = make(chan struct{})
	go l.heartbeatLoop()
}

// acquireRelPath は既にジョブ advisory lock を保持している同じセッションへ
// rel_path lock を追加する。こうして ingest の 2 つの排他を同一接続で保持する。
func (l *relPathLock) acquireRelPath(ctx context.Context, relPath string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = defaultRelPathLockTimeout
	}
	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	key := relPathLockKey(relPath)
	var acquired bool
	if err := l.conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("trying rel_path advisory lock: %w", err)
	}
	if !acquired {
		return false, nil
	}

	l.keys = append(l.keys, heldAdvisoryKey{key: key, label: relPath})
	l.startHeartbeat()
	return true, nil
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
			slog.Warn("ingest: rel_path advisory lock heartbeat failed", "rel_path", l.label(), "err", err)
			l.markLost()
			return true
		}
		*consecutiveFailures++
		if *consecutiveFailures >= relPathLockMaxTransientFailures {
			slog.Warn("ingest: rel_path advisory lock heartbeat failed", "rel_path", l.label(), "err", err, "consecutive_failures", *consecutiveFailures)
			l.markLost()
			return true
		}
		slog.Warn("ingest: rel_path advisory lock heartbeat check failed transiently, retrying",
			"rel_path", l.label(), "err", err, "consecutive_failures", *consecutiveFailures)
		return false
	}

	*consecutiveFailures = 0
	if !held {
		slog.Warn("ingest: rel_path advisory lock was lost during transfer", "rel_path", l.label())
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

// checkHeld はセッションが保持している全キー（production では job lock と
// rel_path lock の 2 本）を 1 本ずつ順に確認する。**timeout はキーごとの
// budget**（relPathLockHeartbeatTimeout）で、ループ全体では共有しない ---
// 共有すると 2 本目のクエリは 1 本目が食った残り時間しか使えず、一過性失敗の
// 頻度が上がって issue #679 が避けようとした側（99% 進んだ転送を 0 バイトから
// 再試行させる）へ寄ってしまう。いずれかのキーが held=false ならその時点で
// 打ち切って false を返す（確定した喪失なので残りのキーを見ても意味がない）。
func (l *relPathLock) checkHeld() (bool, error) {
	for _, lockKey := range l.advisoryKeys() {
		ctx, cancel := context.WithTimeout(context.Background(), relPathLockHeartbeatTimeout)
		var held bool
		err := l.conn.QueryRow(ctx, relPathLockHeldQuery, lockKey.key).Scan(&held)
		cancel()
		if err != nil {
			return false, err
		}
		if !held {
			return false, nil
		}
	}
	return true, nil
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
		if l.stopHeartbeat != nil {
			close(l.stopHeartbeat)
			<-l.heartbeatDone
		}
		if l.conn == nil {
			return
		}

		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), defaultRelPathLockTimeout)
		defer unlockCancel()

		keys := l.advisoryKeys()
		for i := len(keys) - 1; i >= 0; i-- {
			lockKey := keys[i]
			var stillHeld bool
			if err := l.conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey.key).Scan(&stillHeld); err != nil {
				slog.Warn("ingest: failed to release advisory lock", "lock", lockKey.label, "err", err)
			} else if !stillHeld {
				// pgxpool の Release() がセッション状態（advisory lock）を暗黙に
				// リセットするとは仮定しない --- 明示的に unlock し、戻り値
				// （pg_advisory_unlock は「保持していなかった」場合 false を返す）
				// を見る。false は heartbeat が転送中にロック喪失を検知したか、
				// それ以外の理由でセッションのロックが解放されていたことを示す。
				slog.Warn("ingest: advisory lock was already lost before release", "lock", lockKey.label)
			}
		}
		l.conn.Release()
	})
}

// acquireAdvisoryLock はセッションレベル advisory lock を 1 個取得し、取得した
// コネクションを relPathLock として返す。返り値の型は、ジョブ lock に rel_path
// lock を追加して同一セッションを使うために共有している。
func acquireAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64, label string, timeout time.Duration) (*relPathLock, bool, error) {
	if timeout <= 0 {
		timeout = defaultRelPathLockTimeout
	}

	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := pool.Acquire(acquireCtx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring connection for %s advisory lock: %w", label, err)
	}

	var acquired bool
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying %s advisory lock: %w", label, err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	return newRelPathLock(conn, key, label), true, nil
}

// acquireIngestJobLock は ingest Work の開始時にジョブ ID のセッションロックを
// 取得し、commit まで保持する（呼び出し元が defer release する）。record_sweep の
// 回収側も同じキーを pg_try_advisory_lock で試し、取得できた場合に限って元の
// プロセスが死んでいる（セッションが切れてロックが自動解放された）と確定する。
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
// ハングする（internal/db/db.go の roleConnBudget コメント参照。この lock 用の
// コネクションは Work の冒頭から commit まで、rel_path lock 追加後も同じ 1 本を
// 長期保持し続ける）。
//
// acquired=false はロック取得の失敗（既に別セッション --- 実行中の同じジョブか、
// record_sweep の回収側が一瞬 try している最中 --- がこのジョブ ID のロックを
// 保持中）を示す通常の敗北であり、err ではない。
func acquireIngestJobLock(ctx context.Context, pool *pgxpool.Pool, jobID int64, timeout time.Duration) (*relPathLock, bool, error) {
	return acquireAdvisoryLock(ctx, pool, ingestJobLockKey(jobID), fmt.Sprintf("ingest job %d", jobID), timeout)
}

// acquireRelPathLockWithHeartbeat は rel_path の advisory lock を単独で（ジョブ
// lock を経由せず）取得する。**production の呼び手はいない**（唯一の呼び手は
// テスト --- `grep -rn acquireRelPathLockWithHeartbeat internal/` で確認できる）。
// production は必ず `acquireIngestJobLock` → `relPathLock.acquireRelPath` の
// 2 段で同じセッションへ両方のキーを積む（`IngestWorker.Work` 参照）。
//
// 契約（セッションレベル・ノンブロッキング・タイムアウトでハングを防ぐ理由）の
// 記述は `acquireIngestJobLock` の doc コメントに、heartbeat の契約（一過性
// 失敗の許容・残る検出窓）は `acquireRelPath` 呼び出し後にこの関数がしている
// ことと同じなので下記に集約する。この関数自体は、1 プロセス相当の単独取得を
// テストで組み立てやすくするための入口として残す。
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
	key := relPathLockKey(relPath)
	lock, acquired, err := acquireAdvisoryLock(ctx, pool, key, "rel_path", timeout)
	if err != nil || !acquired {
		return lock, acquired, err
	}
	lock.keys[0].label = relPath
	lock.startHeartbeat()
	return lock, true, nil
}
