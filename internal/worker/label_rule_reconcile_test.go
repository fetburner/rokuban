package worker

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobs"
)

// waitTopic は 'rokuban' チャネルの次の通知ペイロードを返す。
func waitTopic(t *testing.T, conn *pgxpool.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("waiting for a notification: %v", err)
	}
	return n.Payload
}

// assertNoNotification は 300 ms のあいだ通知が来ないことを確かめる。
//
// 通知は非同期に届くので、届かないことの判定は「待って来なかった」でしか作れない。
func assertNoNotification(t *testing.T, conn *pgxpool.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(ctx)
	if err == nil {
		t.Fatalf("unexpected notification %q (nothing changed)", n.Payload)
	}
}

// drainNotifications は溜まっている通知を捨てる。
//
// **測定の前に必ず呼ぶ。** recordings と label_rules には行トリガーが付いており、
// テストの下ごしらえ（録画・分類ルールの INSERT）でも 'recordings' が飛ぶ。
// 捨てずに測ると、トリガーの通知をワーカーの通知と誤認する。
func drainNotifications(t *testing.T, conn *pgxpool.Conn) {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := conn.Conn().WaitForNotification(ctx)
		cancel()
		if err != nil {
			return
		}
	}
}

// 全件再評価のワーカーは、変化があったときだけ recordings トピックへ 1 回通知する。
//
// **差分適用をやめて全件を DELETE → INSERT すると、変化が無くても通知が出る**
// （docs/data/series.md §8「評価結果の持ち方」）。この test は両方向を見る:
// 変化なしで通知が出ないこと、変化ありで出ること。
func TestLabelRuleReconcileWorker_NotifiesOnlyOnChange(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()

	listenConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, "LISTEN rokuban"); err != nil {
		t.Fatal(err)
	}

	w := &LabelRuleReconcileWorker{Pool: pool}
	job := &river.Job[jobs.LabelRuleReconcileArgs]{}

	// **ルールを先に入れる。** 録画の INSERT に付けたトリガーが当たりを張るので、
	// この順でないと「最初の 1 パスが作る差分」を測ってしまう。
	if _, err := pool.Exec(ctx, `
INSERT INTO label_rules (key, value, keyword) VALUES ('series', '日本史', '日本史')`); err != nil {
		t.Fatalf("seeding label rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name,
                        channel_type, channel, title, program_start_at, program_duration_ms, status)
VALUES ('manual', 'default', 1, 1, 1, 'test', 'GR', '1',
        'NHK高校講座　日本史　第1回', now(), 1000, 'finished')`); err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	// 行トリガーが当たりを既に張っている。ここで溜まった通知を捨てる。
	drainNotifications(t, listenConn)

	// 変化は無い（当たりはトリガーが正しい状態にしている）。通知しない。
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	assertNoNotification(t, listenConn)

	// 当たりを落とす。分類ルールの編集をまたいでコミットした録画など、
	// トリガーが追随できなかった行の形である（label_rule_hits にトリガーは
	// 無いので、この DELETE 自体は通知を出さない）。
	if _, err := pool.Exec(ctx, "DELETE FROM label_rule_hits"); err != nil {
		t.Fatalf("dropping the hits: %v", err)
	}
	drainNotifications(t, listenConn)

	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := waitTopic(t, listenConn); got != "recordings" {
		t.Fatalf("notification = %q, want recordings", got)
	}

	// もう一度走らせても当たりは変わらない。通知は来ない。
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	assertNoNotification(t, listenConn)
}

// ジョブは一意化しない。ByState を使うと、実行中に来た 2 本目の編集が
// 投入されずに捨てられ、次の定期再評価（15 分）まで反映されない。
//
// **River は running を外した ByState を挿入時にエラーにする**ので、
// 「実行中を除いた状態集合」で代用はできない（river@v0.47.0 の
// requiredV3states）。docs/data/series.md §8。
func TestLabelRuleReconcileArgs_NotUnique(t *testing.T) {
	opts := jobs.LabelRuleReconcileArgs{}.InsertOpts()
	if opts.Queue != jobs.RulerQueue {
		t.Errorf("queue = %q, want %q", opts.Queue, jobs.RulerQueue)
	}
	if opts.UniqueOpts.ByArgs || len(opts.UniqueOpts.ByState) != 0 {
		t.Errorf("UniqueOpts = %+v, want the zero value (a second edit must not be dropped)",
			opts.UniqueOpts)
	}
}
