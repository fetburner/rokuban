package worker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// ProbeIngestStorage は ingest の確定プロトコルが要求する操作列
// （media_dir 配下へのファイル作成 → fsync → Close → 同一ディレクトリ内の
// atomic rename → 親ディレクトリ fsync）を起動時に 1 回通す。パス文字列から
// ファイルシステムの種類を推測しない。probe が 1 回成功したことは、この操作列が
// 動くことの証拠に過ぎず、rename/fsync の意味論を信頼できないファイルシステムを
// 設定契約が除外する判断そのものは変わらない。
//
// rename と親ディレクトリ fsync は commit と同じ renameIngestFile /
// syncIngestParentDir フックを経由する。権威を 1 つにすることで、probe が
// commit と異なる経路を検査してしまう（rename/fsync を落とす変異が probe の
// テストだけをすり抜ける）事態を防ぐ。
//
// 書き込みを検査するのは media_dir の root ではなく、determineRelPath が原本を
// 置く名前空間（catalog.SiteRelPathPrefix）配下である。k8s では fsGroup が root
// にしか効かず sites/ 配下だけ書ける構成があり得るので
// （deploy/k8s/base/media-pvc.yaml の「未解決: マウントの所有権」）、ingest が
// 触らない root の書き込み権限を要求すると偽陽性で worker を起動不能にする。
func ProbeIngestStorage(mediaDir string) error {
	info, err := os.Stat(mediaDir)
	if err != nil {
		return fmt.Errorf("stating storage.media_dir %q: %w", mediaDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("storage.media_dir %q is not a directory", mediaDir)
	}

	probeDir := filepath.Join(mediaDir, catalog.SiteRelPathPrefix)
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return fmt.Errorf("creating ingest storage probe directory %q: %w", probeDir, err)
	}

	tempPath, err := os.CreateTemp(probeDir, mediapath.IngestTempFilePrefix+"probe-*")
	if err != nil {
		return fmt.Errorf("creating ingest storage probe file: %w", err)
	}
	tempName := tempPath.Name()
	finalPath := filepath.Join(probeDir, mediapath.IngestTempFilePrefix+"probe-"+uuid.NewString())
	cleanup := func() {
		_ = os.Remove(tempName)
		_ = os.Remove(finalPath)
	}
	defer cleanup()

	if err := tempPath.Sync(); err != nil {
		_ = tempPath.Close()
		return fmt.Errorf("fsyncing ingest storage probe file: %w", err)
	}
	if err := tempPath.Close(); err != nil {
		return fmt.Errorf("closing ingest storage probe file: %w", err)
	}
	if err := renameIngestFile(tempName, finalPath); err != nil {
		return fmt.Errorf("renaming ingest storage probe file: %w", err)
	}
	if err := syncIngestParentDir(finalPath); err != nil {
		return fmt.Errorf("fsyncing ingest storage probe parent directory: %w", err)
	}
	return nil
}
