// Package role はシングルトンロールのリーダー選出の監督ループ（RunSingletonWith）を提供する。
// 排他ロックの方言の実装（PostgreSQL の advisory lock・MySQL の GET_LOCK）は、サブパッケージ
// pglocker と mysqllocker にある。このパッケージは pgx にも database/sql にも依存しない。
//
// 「ソケットを connect し続ける」形のロール、すなわち watcher（mirakc の SSE を購読し続ける）
// のためにある。listen する側のロール（api / notifier / streamer）は水平にスケールするので
// 選出は不要で、ソケットを持たない仕事（ruler / reconciler / record_sweep / ingest / epg_sync）は
// ジョブとして走る。共通契約（jobq）は排他を保証せず、現行の重複抑止はジョブエンジン adapter の
// UniqueOpts による。ruler の並走による上書きは未解決（docs/data.md §2）。
//
// したがって RunSingletonWith の呼び出し元は cmd/rokuban/server.go の 1 箇所（watcher）だけだが、
// これは削除の根拠ではない（issue #24 M2-20、docs/operations.md §5）。呼び出し元が 1 つに
// 減ったのは機構が 1 つに減ったという成果であり、リーダー選出の失敗モード（heartbeat 喪失 /
// split-brain / フェイルオーバー遅延）を単独でテストできる価値は残る。
//
// なお watcher の singleton 性は「正しさ」の要件ではない。processRecord は record_sync の
// (site, record_id) 行ロックで冪等なので、複数の watcher が同じ record を処理しても recordings は
// 重複しない（docs/recording.md §3.3）。ここで選出しているのは「mirakc に N 本の SSE を張らない」
// という接続数の配慮であって、破れても壊れはしない。
package role

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Locker は方言ごとの排他ロックの実装を表す。Checkout が返す Session は、リーダーである間
// ずっと専有する。ロックは接続（セッション）に結びつき、接続が切れると解放される。
// 実装は pglocker.New（PostgreSQL）と mysqllocker.New（MySQL）。
type Locker interface {
	// Checkout は、ロックに使う接続を 1 本専有して返す。
	Checkout(ctx context.Context) (Session, error)
}

// Session は専有した 1 本の接続上のロック操作。Release は、ロックを解放した後に 1 度だけ呼ぶ。
type Session interface {
	// TryLock は role のロックを待たずに取り、取れたかを返す。
	TryLock(ctx context.Context, role string) (bool, error)
	// Unlock は role のロックを解放する。失敗したら接続を捨て、セッションごとロックを手放す。
	Unlock(ctx context.Context, role string) error
	// Ping は、ロックを保持している接続そのものに疎通確認を送る（heartbeat）。
	Ping(ctx context.Context) error
	// Release は専有した接続を返す。ロックを保持したまま返してはならない。
	Release()
}

// SingletonConfig は RunSingletonWith の設定。
type SingletonConfig struct {
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

func defaultSingletonConfig() SingletonConfig {
	return SingletonConfig{
		PollInterval:      15 * time.Second,
		HeartbeatInterval: 10 * time.Second,
	}
}

const heartbeatTimeout = 5 * time.Second

// RunSingletonWith はシングルトンロールの監督ループを実行する。
// locker のロックを定期試行し、取得したら roleFunc を child context で起動する。
// リーダー中はロックを保持する接続に定期 heartbeat を送り、失敗したらロールを停止して
// 取得ループに戻る。ctx がキャンセルされるまでブロックする。
func RunSingletonWith(ctx context.Context, locker Locker, role string, roleFunc func(ctx context.Context) error, cfg *SingletonConfig) error {
	if cfg == nil {
		d := defaultSingletonConfig()
		cfg = &d
	}

	for {
		sess, err := locker.Checkout(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to acquire connection for leader lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		acquired, err := sess.TryLock(ctx, role)
		if err != nil {
			sess.Release()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("failed to try advisory lock", "role", role, "err", err)
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		if !acquired {
			sess.Release()
			sleepWithJitter(ctx, cfg.PollInterval)
			continue
		}

		slog.Info("acquired leader lock", "role", role)

		runErr := runWithHeartbeat(ctx, sess, role, cfg.HeartbeatInterval, roleFunc)

		_ = sess.Unlock(context.Background(), role)
		sess.Release()
		slog.Info("released leader lock", "role", role)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if runErr != nil {
			slog.Warn("role exited with error, will retry", "role", role, "err", runErr)
		}
	}
}

func runWithHeartbeat(ctx context.Context, sess Session, role string, heartbeatInterval time.Duration, roleFunc func(ctx context.Context) error) error {
	roleCtx, roleCancel := context.WithCancel(ctx)
	defer roleCancel()

	roleDone := make(chan error, 1)
	go func() {
		roleDone <- roleFunc(roleCtx)
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case err := <-roleDone:
			return err

		case <-ticker.C:
			hbCtx, hbCancel := context.WithTimeout(context.Background(), heartbeatTimeout)
			err := sess.Ping(hbCtx)
			hbCancel()
			if err != nil {
				if ctx.Err() != nil {
					roleCancel()
					return <-roleDone
				}
				slog.Error("heartbeat failed, releasing leadership", "role", role, "err", err)
				roleCancel()
				return <-roleDone
			}

		case <-ctx.Done():
			roleCancel()
			return <-roleDone
		}
	}
}

func sleepWithJitter(ctx context.Context, base time.Duration) {
	jitter := time.Duration(rand.Int64N(int64(base/2))) - base/4
	select {
	case <-time.After(base + jitter):
	case <-ctx.Done():
	}
}
