package worker

import (
	"context"
	"fmt"

	pgx5 "github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/medialock"
)

const mediaRelPathLockKeyPrefix = "rokuban:media:rel-path:"

type mediaRelPathFileLock = medialock.FileLock

func lockMediaRelPathFile(ctx context.Context, mediaDir, relPath string) (*mediaRelPathFileLock, error) {
	return medialock.Lock(ctx, mediaDir, relPath)
}
func tryLockMediaRelPathFile(mediaDir, relPath string) (*mediaRelPathFileLock, bool, error) {
	return medialock.TryLock(mediaDir, relPath)
}

// lockMediaRelPathInTransaction は canonical file の公開と orphan 回収が共有する
// transaction-level advisory lock を取得する。転送全体ではなく、DB の media_asset
// INSERT と temp -> canonical rename / orphan unlink の短い確定区間だけを直列化する。
// 同じ rel_path の ingest commit と orphan unlink が同時に進むと、片方がもう片方の
// canonical file を消せるため、両者は必ず同じキーを使う。
func lockMediaRelPathInTransaction(ctx context.Context, tx pgx5.Tx, relPath string) error {
	key := advisoryLockKey(mediaRelPathLockKeyPrefix, relPath)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("acquiring media rel_path advisory lock: %w", err)
	}
	return nil
}

// tryLockMediaRelPathInTransaction は orphan 回収用の非 blocking 版。ingest の
// commit が同じ rel_path を公開中なら、回収側は待ち続けず次の reconcile pass に
// 委ねる。
func tryLockMediaRelPathInTransaction(ctx context.Context, tx pgx5.Tx, relPath string) (bool, error) {
	key := advisoryLockKey(mediaRelPathLockKeyPrefix, relPath)
	var acquired bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("trying media rel_path advisory lock: %w", err)
	}
	return acquired, nil
}
