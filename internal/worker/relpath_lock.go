package worker

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
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

// acquireRelPathLock は rel_path の Postgres **セッションレベル** advisory
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
// **これは相互排除の絶対的な保証ではない（正直に書く劣化モード）。** 転送中に
// このロック用コネクションが死ぬと、ロックは早期に解放される。その窓は
// ロック導入前と同じ TOCTOU に戻るだけで、新しい壊れ方を作るものではない
// （docs/recording/ingest.md §5.3「劣化モード」）。ロックを「正しさの根拠」と
// 呼べるのは ingest 対 ingest の範囲に限る。
func acquireRelPathLock(ctx context.Context, pool *pgxpool.Pool, relPath string, timeout time.Duration) (release func(), acquired bool, err error) {
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
	if err := conn.QueryRow(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("trying rel_path advisory lock: %w", err)
	}

	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	release = func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), defaultRelPathLockTimeout)
		defer unlockCancel()

		var stillHeld bool
		if err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&stillHeld); err != nil {
			slog.Warn("ingest: failed to release rel_path advisory lock", "err", err)
		} else if !stillHeld {
			// pgxpool の Release() がセッション状態（advisory lock）を暗黙に
			// リセットするとは仮定しない --- 明示的に unlock し、戻り値
			// （pg_advisory_unlock は「保持していなかった」場合 false を返す）
			// を見る。false は転送中にこのコネクションが失われてロックが
			// 早期解放されていた（劣化モード）ことの唯一の事後的な手がかり
			// であり、これ自体は何かを防ぐものではない。
			slog.Warn("ingest: rel_path advisory lock was already lost before release")
		}
		conn.Release()
	}
	return release, true, nil
}
