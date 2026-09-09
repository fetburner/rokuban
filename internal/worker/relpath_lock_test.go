package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestRelPathLock_SecondAcquireFailsAndReleaseFrees は
// acquireRelPathLockWithHeartbeat がセッションレベルの排他になっていることを
// 固定する（internal/role/leader_test.go の TestTryAcquire_Exclusive /
// _ReleaseAndReacquire と同じ、独立した 2 プールを使う形）。
//
// **release / pool.Close の登録順序に注意する**（レビュー指摘の教訓を
// このテストにも適用する）。t.Cleanup は LIFO で実行され、かつ t.Fatal は
// その場でテスト関数の goroutine を Goexit するので、通常の `defer` で
// pool.Close を登録していると、release より先（Goexit のスタック巻き戻し中）
// に走ってしまい、保持中のコネクションを待ってハングする。pool.Close を
// 先に t.Cleanup 登録し、release 系を後から t.Cleanup 登録することで、
// どの t.Fatal 経路でも「release が先、Close が後」を保証する。
//
// **どの acquireRelPathLockWithHeartbeat 呼び出しの戻り値も、結果を見る前に
// 必ず t.Cleanup へ登録する。** 「失敗するはず」の呼び出しでも、`acquired` が
// 変異で意図せず true になった場合は本物のコネクションを握ったままになり、
// release を捨てると pool.Close が同じ理由でハングする（このテストを書く
// 過程で実際に踏んだ: pool2 の 2 回目の呼び出しの戻り値を `_` で捨てていたら、
// 壊し方 (SELECT true) を入れたときに pool2.Close がハングした）。
//
// 壊し方: relpath_lock.go の `pg_try_advisory_lock($1)` を `SELECT true` に
// 置き換えると、pool2 が pool1 の保持中でもロックを取得できてしまい、
// 「expected pool2 to NOT acquire」で落ちる（コンパイルは通る変異）。
func TestIngestRelPathLock_SecondAcquireFailsAndReleaseFrees(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	t.Cleanup(pool1.Close)

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	t.Cleanup(pool2.Close)

	const relPath = "sites/default/test/lock-exclusive.m2ts"

	// safeRelease は release（nil のこともある）を、二重解放しない・nil でも
	// panic しない形にラップする。
	safeRelease := func(release func()) func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				if release != nil {
					release()
				}
			})
		}
	}
	releaseFunc := func(lock *relPathLock) func() {
		if lock == nil {
			return nil
		}
		return lock.release
	}

	lock1, acquired1, err := acquireRelPathLockWithHeartbeat(ctx, pool1, relPath, time.Second)
	safeRelease1 := safeRelease(releaseFunc(lock1))
	t.Cleanup(safeRelease1) // pool1.Close より後に登録する（LIFO で先に走る）。
	if err != nil {
		t.Fatalf("acquireRelPathLockWithHeartbeat pool1: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected pool1 to acquire the lock")
	}

	lock2, acquired2, err := acquireRelPathLockWithHeartbeat(ctx, pool2, relPath, time.Second)
	t.Cleanup(safeRelease(releaseFunc(lock2))) // pool2.Close より後に登録する。
	if err != nil {
		t.Fatalf("acquireRelPathLockWithHeartbeat pool2: %v", err)
	}
	if acquired2 {
		t.Fatal("expected pool2 to NOT acquire the lock (already held by pool1)")
	}

	safeRelease1()

	lock3, acquired3, err := acquireRelPathLockWithHeartbeat(ctx, pool2, relPath, time.Second)
	t.Cleanup(safeRelease(releaseFunc(lock3))) // pool2.Close より後に登録する。
	if err != nil {
		t.Fatalf("acquireRelPathLockWithHeartbeat pool2 after release: %v", err)
	}
	if !acquired3 {
		t.Fatal("expected pool2 to acquire the lock after pool1 released")
	}
}

// TestIngestJobAndRelPathLocksShareSession は、ingest のジョブ lock と rel_path
// lock が同じ PostgreSQL セッションに積まれることを固定する。別セッションで
// rel_path lock を取っている実装では、pool2 がジョブ lock だけでなく rel_path
// lock も取得できてしまう。
func TestIngestJobAndRelPathLocksShareSession(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	t.Cleanup(pool1.Close)
	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	t.Cleanup(pool2.Close)

	const jobID int64 = 690001
	const relPath = "sites/default/test/job-and-rel-path-lock.m2ts"

	lock1, acquired, err := acquireIngestJobLock(ctx, pool1, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock: %v", err)
	}
	if !acquired {
		t.Fatal("expected pool1 to acquire the job lock")
	}
	t.Cleanup(lock1.release)

	acquired, err = lock1.acquireRelPath(ctx, relPath, time.Second)
	if err != nil {
		t.Fatalf("adding rel_path lock to job session: %v", err)
	}
	if !acquired {
		t.Fatal("expected the job session to acquire the rel_path lock")
	}

	lock2, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock from pool2: %v", err)
	}
	if lock2 != nil {
		t.Cleanup(lock2.release)
	}
	if acquired {
		t.Fatal("pool2 acquired the job lock while pool1 was holding it")
	}

	lock2, acquired, err = acquireRelPathLockWithHeartbeat(ctx, pool2, relPath, time.Second)
	if err != nil {
		t.Fatalf("acquiring rel_path lock from pool2: %v", err)
	}
	if lock2 != nil {
		t.Cleanup(lock2.release)
	}
	if acquired {
		t.Fatal("pool2 acquired the rel_path lock while pool1 was holding it")
	}

	lock1.release()
	lock3, acquired, err := acquireIngestJobLock(ctx, pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("reacquiring job lock after release: %v", err)
	}
	if !acquired {
		t.Fatal("pool2 did not acquire the job lock after release")
	}
	t.Cleanup(lock3.release)
	acquired, err = lock3.acquireRelPath(ctx, relPath, time.Second)
	if err != nil {
		t.Fatalf("reacquiring rel_path lock after release: %v", err)
	}
	if !acquired {
		t.Fatal("pool2 did not acquire the rel_path lock after release")
	}
}

// TestIngestRelPathLock_HeartbeatDetectsJobKeyLossWhileRelPathKeyStillHeld は、
// production 経路（acquireIngestJobLock → acquireRelPath で同一セッションに
// job lock と rel_path lock の 2 本を積む）で作った relPathLock に対し、
// job 側のキーだけを同一セッションから外部に pg_advisory_unlock したとき、
// rel_path 側のキーはまだ保持されているにもかかわらず heartbeat が lost を
// 閉じることを固定する。grep でわかるとおり acquireRelPathLockWithHeartbeat の
// production の呼び手はいない（テストのみ）ため、既存の heartbeat 回帰テストは
// すべて 1 キー構成でしか checkHeld のループを通していなかった --- production の
// 2 キー構成でループが全キーを見ることは、このテストが無いと固定されていない。
//
// job キーの unlock は **acquireRelPath（＝ startHeartbeat）より前**に行う。
// heartbeat が起動した後に同じ lock.conn へ直接クエリを投げると、heartbeat
// goroutine の checkHeld と test goroutine の unlock クエリが同じ
// *pgxpool.Conn（pgx はコネクション単位で goroutine-safe ではない）を同時に
// 使ってしまい、データレースになる（-race で検出）。acquireIngestJobLock 直後
// はまだ heartbeat が存在しないため、この窓で unlock すれば conn の同時使用が
// 起きない。その後で acquireRelPath を呼んで rel_path キーを追加し、そこで
// 初めて heartbeat を起動する --- 「job 側のキーは失われ、rel_path 側の
// キーはまだ保持されている」という検証対象の状態そのものは変わらない。
//
// 壊し方: checkHeld のループを l.advisoryKeys() の末尾（rel_path 側）だけを見る
// よう弱める（production 導入前の単一キー実装への後退。当時は rel_path lock しか
// 存在しなかった）と、job 側のキー喪失を無視し、このテストは lost が閉じられず
// タイムアウトで落ちる。
func TestIngestRelPathLock_HeartbeatDetectsJobKeyLossWhileRelPathKeyStillHeld(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	const jobID int64 = 690006
	const relPath = "sites/default/test/lock-heartbeat-job-key-loss.m2ts"

	lock, acquired, err := acquireIngestJobLock(ctx, pool, jobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring job lock: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire the job lock")
	}
	t.Cleanup(lock.release)

	// heartbeat はまだ起動していない（acquireRelPath 呼び出し前）ので、この
	// 接続を他の goroutine と competing せずに直接使える。job 側のキーだけを
	// 同一セッションからアンロックする。rel_path 側のキーはまだ存在すらしない
	// --- 後で acquireRelPath が追加してから heartbeat が初めて動き出す。
	jobKey := ingestJobLockKey(jobID)
	var stillHeld bool
	if err := lock.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", jobKey).Scan(&stillHeld); err != nil {
		t.Fatalf("unlocking job key out of band: %v", err)
	}
	if !stillHeld {
		t.Fatal("job key was not held before the out-of-band unlock")
	}

	acquired, err = lock.acquireRelPath(ctx, relPath, time.Second)
	if err != nil {
		t.Fatalf("adding rel_path lock to the job session: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire the rel_path lock in the same session")
	}

	select {
	case <-lock.lost:
	case <-time.After(relPathLockHeartbeatInterval + relPathLockHeartbeatTimeout + 3*time.Second):
		t.Fatal("heartbeat did not close lost after the job advisory lock key was released out of band while the rel_path key was still held")
	}
}

// TestIngestRelPathLock_HeartbeatPreservesHeldSessionAlive は、ロック保持中の
// heartbeat が正常なセッションを誤って lost 扱いしないことを固定する。
// `pg_locks` の bigint key 分解や objsubid 条件を壊す変異は、heartbeat 1 回後に
// isLost が true になって落ちる。
func TestIngestRelPathLock_HeartbeatPreservesHeldSessionAlive(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	lock, acquired, err := acquireRelPathLockWithHeartbeat(ctx, pool, "sites/default/test/lock-heartbeat.m2ts", time.Second)
	if err != nil {
		t.Fatalf("acquireRelPathLockWithHeartbeat: %v", err)
	}
	if !acquired {
		t.Fatal("expected heartbeat test to acquire the lock")
	}
	t.Cleanup(lock.release)

	time.Sleep(relPathLockHeartbeatInterval + 250*time.Millisecond)
	if lock.isLost() {
		t.Fatal("heartbeat marked a healthy rel_path lock as lost")
	}
}

// TestIngestWorker_RelPathLockTimeoutDoesNotHang は、プールが枯渇していても
// acquireRelPathLockWithHeartbeat がハングせず期限内にエラーで返ることを固定する
// （ingest の River タイムアウトは無効なので、これが唯一の歯止め）。
//
// MaxConns=1 のプールの唯一のコネクションを別途保持した状態で
// RelPathLockTimeout=100ms 相当の呼び出しを行う。ジョブ側の相当物である
// acquireRelPathLockWithHeartbeat の戻りは goroutine + select で 2 秒の期限
// 付きに受け、期限超過はハングではなく t.Fatal（アサーション失敗）で検出する。
//
// 壊し方: acquireRelPathLockWithHeartbeat 内で pool.Acquire に渡す ctx を
// `acquireCtx`（期限付き）から素の `ctx` に戻すと、2 秒の期限を超えて
// 「did not return within 2s」で落ちる（コンパイルは通る変異）。
func TestIngestWorker_RelPathLockTimeoutDoesNotHang(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parsing pool config: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("creating MaxConns=1 pool: %v", err)
	}
	defer pool.Close()

	// この 1 本きりのコネクションを別途保持し、acquireRelPathLock の
	// pool.Acquire がプール枯渇でハングしうる状況を作る。
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the only connection: %v", err)
	}
	defer held.Release()

	type result struct {
		acquired bool
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		_, acquired, err := acquireRelPathLockWithHeartbeat(context.Background(), pool, "sites/default/test/lock-timeout.m2ts", 100*time.Millisecond)
		resultCh <- result{acquired: acquired, err: err}
	}()

	select {
	case r := <-resultCh:
		if r.err == nil {
			t.Fatalf("acquireRelPathLockWithHeartbeat err = nil (acquired=%v), want a timeout error (pool exhausted)", r.acquired)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireRelPathLockWithHeartbeat did not return within 2s; pool.Acquire hung instead of timing out")
	}
}

// TestIngestRelPathLock_TransientHeartbeatFailuresDoNotMarkLostUntilThreshold
// は、checkHeld 相当の一過性エラー（接続は生きているがクエリが失敗した）が
// relPathLockMaxTransientFailures 回連続するまでは lost にならず、その回数に
// 達したときだけ lost になることを固定する（issue #679 レビュー: DB の数秒の
// レイテンシ 1 回だけで転送を再ダウンロードに戻さない）。checkHeldFunc を
// 直接差し替え、heartbeatTick を実 DB / 実 heartbeatInterval 無しで呼ぶ。
//
// 壊し方: heartbeatTick の「permanent でなければ即 markLost しない」分岐
// （consecutiveFailures をカウントして relPathLockMaxTransientFailures 未満なら
// return false する部分）を消し、一過性エラーでも常に markLost するようにすると、
// 1 回目の呼び出しで isLost() が true になり
// "isLost() became true after only 1 transient failure" で落ちる。
func TestIngestRelPathLock_TransientHeartbeatFailuresDoNotMarkLostUntilThreshold(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	l := newRelPathLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		return false, false, transientErr
	}

	var consecutiveFailures int
	for i := 1; i < relPathLockMaxTransientFailures; i++ {
		if l.heartbeatTick(&consecutiveFailures) {
			t.Fatalf("heartbeatTick stopped the loop on transient failure %d, want it to keep retrying until %d", i, relPathLockMaxTransientFailures)
		}
		if l.isLost() {
			t.Fatalf("isLost() became true after only %d transient failure(s), want it to stay false until %d", i, relPathLockMaxTransientFailures)
		}
	}

	if !l.heartbeatTick(&consecutiveFailures) {
		t.Fatalf("heartbeatTick did not stop the loop on transient failure %d, want it to give up", relPathLockMaxTransientFailures)
	}
	if !l.isLost() {
		t.Fatalf("isLost() is still false after %d consecutive transient failures, want lost", relPathLockMaxTransientFailures)
	}
}

// TestIngestRelPathLock_TransientHeartbeatFailureResetsOnSuccess は、一過性
// エラーの連続回数が、その後 checkHeld 相当が成功すればリセットされることを
// 固定する（間欠的な失敗が積み上がって誤って lost にならないため）。
//
// 壊し方: heartbeatTick の成功時に `*consecutiveFailures = 0` をしないと、
// このテストは 2 回目の一過性失敗 (relPathLockMaxTransientFailures 回目の
// 「本来ならリセット後の 1 回目」) で誤って isLost()==true になり、
// "isLost() became true" で落ちる。
func TestIngestRelPathLock_TransientHeartbeatFailureResetsOnSuccess(t *testing.T) {
	transientErr := errors.New("simulated transient db latency")
	var succeedNext bool
	l := newRelPathLock(nil, 1, "test")
	l.checkHeldFunc = func() (held, permanent bool, err error) {
		if succeedNext {
			succeedNext = false
			return true, false, nil
		}
		return false, false, transientErr
	}

	var consecutiveFailures int
	// relPathLockMaxTransientFailures - 1 回まで一過性エラーを積み上げる。
	for i := 1; i < relPathLockMaxTransientFailures; i++ {
		if l.heartbeatTick(&consecutiveFailures) {
			t.Fatalf("heartbeatTick stopped the loop early on transient failure %d", i)
		}
	}

	// 1 回成功させてカウンタをリセットさせる。
	succeedNext = true
	if l.heartbeatTick(&consecutiveFailures) {
		t.Fatal("heartbeatTick stopped the loop on a successful check")
	}
	if consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d after a successful check, want 0", consecutiveFailures)
	}

	// リセット後は再び relPathLockMaxTransientFailures 回連続で初めて lost。
	for i := 1; i < relPathLockMaxTransientFailures; i++ {
		if l.heartbeatTick(&consecutiveFailures) {
			t.Fatalf("heartbeatTick stopped the loop on transient failure %d after reset, want it to keep retrying", i)
		}
		if l.isLost() {
			t.Fatalf("isLost() became true after only %d transient failure(s) post-reset", i)
		}
	}
	if !l.heartbeatTick(&consecutiveFailures) {
		t.Fatal("heartbeatTick did not stop the loop after the full threshold post-reset")
	}
	if !l.isLost() {
		t.Fatal("isLost() is still false after the full threshold post-reset")
	}
}
