package mysqlsource_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/notifier"
	"github.com/fetburner/rokuban/internal/notifier/mysqlsource"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/mysqlstore"
)

// readySource は mysqlsource.Source の Listen を包み、購読の確立を外から待てるようにする。
// EventHub の waitListening は非公開なので、テストはここで確立を検知する。
type readySource struct {
	src   *mysqlsource.Source
	once  sync.Once
	ready chan struct{}
}

func (r *readySource) Listen(ctx context.Context, ready func(), out chan<- string) error {
	return r.src.Listen(ctx, func() {
		ready()
		r.once.Do(func() { close(r.ready) })
	}, out)
}

// startHub は db を読む EventHub を起動し、購読者を返す。LISTEN 確立まで待つ。
func startHub(t *testing.T, db *sql.DB) <-chan string {
	t.Helper()
	hub := notifier.NewEventHub()
	sub, unsubscribe := hub.Subscribe()
	t.Cleanup(unsubscribe)
	src := &readySource{src: mysqlsource.New(db), ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.Run(ctx, src)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-src.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the MySQL source to listen")
	}
	return sub
}

// expectTopic は want が次に届くことを待つ。
func expectTopic(t *testing.T, sub <-chan string, want string) {
	t.Helper()
	select {
	case got := <-sub:
		if got != want {
			t.Fatalf("topic = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for topic %q", want)
	}
}

// expectNoTopic は poll 数回ぶんの時間に通知が届かないことを確かめる。
// 重複除去を外すと、読み直しのたびに同じ行が再送されてここで落ちる。
func expectNoTopic(t *testing.T, sub <-chan string) {
	t.Helper()
	select {
	case got := <-sub:
		t.Fatalf("unexpected topic %q", got)
	case <-time.After(2500 * time.Millisecond):
	}
}

// TestCircuitBreakerTriggersReachHub は、circuit_breakers の 3 操作がそれぞれ
// breakers のイベントとして EventHub に届くことを検査する。
func TestCircuitBreakerTriggersReachHub(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	sub := startHub(t, db)
	ctx := context.Background()
	breakers := mysqlstore.NewBreakers(db)

	if _, err := breakers.Trip(ctx, "site", "a", 3, store.BreakerSample{Total: 1}); err != nil {
		t.Fatalf("Trip insert: %v", err)
	}
	expectTopic(t, sub, "breakers")
	if _, err := breakers.Trip(ctx, "site", "a", 3, store.BreakerSample{Total: 2}); err != nil {
		t.Fatalf("Trip update: %v", err)
	}
	expectTopic(t, sub, "breakers")
	if err := breakers.Resume(ctx, "site", "a"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	expectTopic(t, sub, "breakers")
}

// TestRolledBackNotifyDoesNotReachHub は、rollback した tx の NotifyTopic が届かないことを検査する。
// 後から届いた rules が、先に書かれて rollback された recordings より前に来ないことを見る。
func TestRolledBackNotifyDoesNotReachHub(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	sub := startHub(t, db)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mysqlstore.NotifyTopic(ctx, tx, "recordings"); err != nil {
		t.Fatalf("NotifyTopic in tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mysqlstore.NotifyTopic(ctx, db, "rules"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	// recordings と rules は同じ flush に入りうる。順序はランダムなので、rules を受けた後に
	// 残りを見て、rollback した recordings が届かないことを決定的に確かめる。
	expectTopic(t, sub, "rules")
	expectNoTopic(t, sub)
}

// TestOutOfOrderCommitIsDelivered は、番号の小さい行が後からコミットされても届くことを検査する。
// tx A は先に insert（小さい id）して保持し、tx B は後から insert してコミットする。
// B が届いてから A をコミットし、A の "recordings" が遡る時間の中で届くことを見る。
// 「最後に読んだ id より後」で読む実装は、A を飛ばして落ちる。
func TestOutOfOrderCommitIsDelivered(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	sub := startHub(t, db)
	ctx := context.Background()

	txA, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	t.Cleanup(func() { _ = txA.Rollback() })
	if err := mysqlstore.NotifyTopic(ctx, txA, "recordings"); err != nil {
		t.Fatalf("NotifyTopic A: %v", err)
	}
	txB, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	if err := mysqlstore.NotifyTopic(ctx, txB, "rules"); err != nil {
		t.Fatalf("NotifyTopic B: %v", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("commit B: %v", err)
	}
	expectTopic(t, sub, "rules")

	if err := txA.Commit(); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	expectTopic(t, sub, "recordings")
}

// TestSentRowsAreNotResent は、遡る時間の中の行が読み直しで再送されないことを検査する。
func TestSentRowsAreNotResent(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	sub := startHub(t, db)
	if err := mysqlstore.NotifyTopic(context.Background(), db, "rules"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	expectTopic(t, sub, "rules")
	expectNoTopic(t, sub)
}

// TestTwoReplicasReceiveSameEvent は、2 つの notifier レプリカが同じ通知を両方受けることを検査する。
func TestTwoReplicasReceiveSameEvent(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	subA := startHub(t, db)
	subB := startHub(t, db)
	if err := mysqlstore.NotifyTopic(context.Background(), db, "reservations"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	expectTopic(t, subA, "reservations")
	expectTopic(t, subB, "reservations")
}

// TestStartupDoesNotReplayExistingRows は、起動前の行を既読にして配り直さないことを検査する。
func TestStartupDoesNotReplayExistingRows(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	if err := mysqlstore.NotifyTopic(ctx, db, "rules"); err != nil {
		t.Fatalf("NotifyTopic before startup: %v", err)
	}
	sub := startHub(t, db)
	expectNoTopic(t, sub)
	if err := mysqlstore.NotifyTopic(ctx, db, "recordings"); err != nil {
		t.Fatalf("NotifyTopic after startup: %v", err)
	}
	expectTopic(t, sub, "recordings")
}

// TestEncodeProgressCoalescesByRecordingAndProfile は、MySQL 構成でも進捗が
// recording と profile の単位で合流し、最新の値だけが届くことを検査する。
func TestEncodeProgressCoalescesByRecordingAndProfile(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	sub := startHub(t, db)
	ctx := context.Background()

	first := `{"type":"encode-progress","recordingId":42,"profile":"mobile","progress":0.25}`
	latest := `{"type":"encode-progress","recordingId":42,"profile":"mobile","progress":0.75}`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mysqlstore.NotifyTopic(ctx, tx, first); err != nil {
		t.Fatalf("NotifyTopic first: %v", err)
	}
	if err := mysqlstore.NotifyTopic(ctx, tx, latest); err != nil {
		t.Fatalf("NotifyTopic latest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	expectTopic(t, sub, latest)
	expectNoTopic(t, sub)
}
