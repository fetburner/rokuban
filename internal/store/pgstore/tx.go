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

// lockRecording は録画の行をロックし、消去済み・ごみ箱の録画を弾く。
func lockRecording(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	// 書き込み tx の最初のクエリでなければならない。CM 検出 worker の recordings
	// ロックと同じ行を取り、両方の書き込みが互いを観測できるようにする。
	lock, err := q.LockRecording(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("locking recording %d: %w", recordingID, err)
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
