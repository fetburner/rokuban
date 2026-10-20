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

// lockProgramSnapshot は番組のスナップショットの行をロックする。同じ番組の意図・上書きの書き込みを
// 直列化する点で、既存の上書きを読むより先に呼ぶ。行が無ければ ErrNotFound を返す。
// 射影にある番組では upsert の後に、凍結スナップショットだけの番組では存在確認の代わりに呼ぶ。
// upsert（ON CONFLICT DO UPDATE）の暗黙の行ロックには頼らない。
func lockProgramSnapshot(ctx context.Context, q *sqlcgen.Queries, site string, programID int64) error {
	if _, err := q.LockProgramSnapshot(ctx, sqlcgen.LockProgramSnapshotParams{Site: site, ProgramID: programID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return fmt.Errorf("locking program snapshot %d at site %s: %w", programID, site, err)
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
