package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
)

func TestProbeIngestStorage_CompletesOperationSequenceAndCleansUp(t *testing.T) {
	mediaDir := t.TempDir()

	if err := ProbeIngestStorage(mediaDir); err != nil {
		t.Fatalf("ProbeIngestStorage: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(mediaDir, "sites"))
	if err != nil {
		t.Fatalf("reading probe directory: %v", err)
	}
	for _, entry := range entries {
		if mediapath.IsIngestTempFile(entry.Name()) {
			t.Errorf("probe left temporary file %q", entry.Name())
		}
	}
}

// TestProbeIngestStorage_WritesUnderSitesNotRoot は probe が media_dir の root では
// なく ingest が実際に書く sites/ 配下へ書き込むことを固定する。root が
// 実行ユーザーで書けず sites/ 配下だけ書ける k8s 構成（fsGroup が root にしか
// 効かない CSI ドライバ）で、この境界を間違えると fail-fast そのものが偽陽性で
// worker を CrashLoop させる。
func TestProbeIngestStorage_WritesUnderSitesNotRoot(t *testing.T) {
	mediaDir := t.TempDir()

	if err := ProbeIngestStorage(mediaDir); err != nil {
		t.Fatalf("ProbeIngestStorage: %v", err)
	}

	rootEntries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("reading media_dir: %v", err)
	}
	for _, entry := range rootEntries {
		if mediapath.IsIngestTempFile(entry.Name()) {
			t.Errorf("probe left temporary file directly under media_dir root: %q", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "sites")); err != nil {
		t.Fatalf("probe did not create the sites/ directory it writes under: %v", err)
	}
}

// TestProbeIngestStorage_UsesSharedRenameAndFsyncHooks は probe が commit と同じ
// renameIngestFile / syncIngestParentDir フックを経由することを固定する。probe が
// 独自に os.Rename / syncIngestDirectory を直呼びする実装に戻すと、rename や
// 親ディレクトリ fsync が壊れていても probe だけ気付かず起動できてしまう。
// 実測: rename と親ディレクトリ fsync を ProbeIngestStorage から落とす変異は、
// 中身を見ない旧テストでは検出できず緑のまま通っていた。下の 2 subtest は
// フックへ失敗を注入するので、直呼びに戻すと注入が届かず probe が nil を
// 返して両方とも落ちる。
func TestProbeIngestStorage_UsesSharedRenameAndFsyncHooks(t *testing.T) {
	t.Run("rename failure surfaces", func(t *testing.T) {
		mediaDir := t.TempDir()
		original := renameIngestFile
		t.Cleanup(func() { renameIngestFile = original })
		renameIngestFile = func(string, string) error {
			return errors.New("injected probe rename failure")
		}

		err := ProbeIngestStorage(mediaDir)
		if err == nil {
			t.Fatal("ProbeIngestStorage returned nil despite the injected rename failure")
		}
		if !strings.Contains(err.Error(), "injected probe rename failure") {
			t.Errorf("err = %v, want it to surface the injected rename failure", err)
		}
	})

	t.Run("parent fsync failure surfaces", func(t *testing.T) {
		mediaDir := t.TempDir()
		original := syncIngestParentDir
		t.Cleanup(func() { syncIngestParentDir = original })
		syncIngestParentDir = func(path string) error {
			if err := original(path); err != nil {
				return err
			}
			return errors.New("injected probe parent fsync failure")
		}

		err := ProbeIngestStorage(mediaDir)
		if err == nil {
			t.Fatal("ProbeIngestStorage returned nil despite the injected parent fsync failure")
		}
		if !strings.Contains(err.Error(), "injected probe parent fsync failure") {
			t.Errorf("err = %v, want it to surface the injected parent fsync failure", err)
		}
	})
}

func TestProbeIngestStorage_RejectsMissingOrNonDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := ProbeIngestStorage(missing); err == nil {
		t.Fatal("ProbeIngestStorage(missing) returned nil")
	} else if !strings.Contains(err.Error(), "storage.media_dir") {
		t.Errorf("error = %q, want storage.media_dir context", err)
	}

	filePath := filepath.Join(t.TempDir(), "media-file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("writing non-directory probe target: %v", err)
	}
	if err := ProbeIngestStorage(filePath); err == nil {
		t.Fatal("ProbeIngestStorage(file) returned nil")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want not-a-directory context", err)
	}
}
