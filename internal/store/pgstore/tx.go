package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// lockRecordingState は録画の行をロックし、ごみ箱・消去済みかどうかを問わず状態を返す。
// ごみ箱の録画も扱うメソッド（SoftDelete / Restore / RequestPurge）の最初の文に使う。
// 録画が無ければ ErrNotFound を返す。
func lockRecordingState(ctx context.Context, q *sqlcgen.Queries, recordingID int64) (sqlcgen.LockRecordingRow, error) {
	// 録画の行に触れる最初の文として呼ぶ。
	lock, err := q.LockRecording(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return lock, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return lock, fmt.Errorf("locking recording %d: %w", recordingID, err)
	}
	return lock, nil
}

// lockRecording は録画の行をロックし、消去済み・ごみ箱の録画を弾く。
// チャプター編集が使う。CM 検出 worker の recordings ロックと同じ行を取り、
// 両方の書き込みが互いを観測できるようにする。
func lockRecording(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	lock, err := lockRecordingState(ctx, q, recordingID)
	if err != nil {
		return err
	}
	if lock.IsPurged {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if lock.IsTrashed {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrTrashed)
	}
	return nil
}

// mapDatabaseError は一意違反と排他制約違反を store.ErrConflict に写す。
func mapDatabaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23P01") {
		return errors.Join(store.ErrConflict, err)
	}
	return err
}

// rollback は tx を捨てる。commit 済みなら何もしない。
func rollback(tx pgx.Tx) {
	_ = tx.Rollback(context.Background())
}
