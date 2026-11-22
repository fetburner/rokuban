package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/store"
)

// runTx は store の書き込みメソッドに 1 つの transaction を与える。
// TestRunTxRollsBackAfterStatementFailure は、文の失敗時に rollback してから返すことを検査する。
// デッドロックとロック待ちタイムアウトは自動再試行しない。任意のメソッドを再実行するには
// Go 側に副作用が無い必要があるが、共通ヘルパーからは各処理が条件を満たすか分からない。
func runTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning MySQL transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rolling back MySQL transaction: %w", rollbackErr))
		}
		return translateError(err)
	}
	if err := tx.Commit(); err != nil {
		return translateError(fmt.Errorf("committing MySQL transaction: %w", err))
	}
	return nil
}

func translateError(err error) error {
	if mysqldb.IsUniqueViolation(err) {
		return fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	return err
}
