package worker

import (
	"testing"
)

// TestStatDisk_RealTempDir は実ディレクトリに対して statfs が呼べ、値が
// 物理的にあり得る範囲に収まることを確認する。CI ランナーやローカルの
// ディスクサイズは環境依存で決定的な値を主張できないため、絶対値ではなく
// 不変条件（total >= used、avail <= total、total > 0）だけを検査する。
func TestStatDisk_RealTempDir(t *testing.T) {
	dir := t.TempDir()

	u, err := statDisk(dir)
	if err != nil {
		t.Fatalf("statDisk(%q) error: %v", dir, err)
	}

	if u.totalBytes <= 0 {
		t.Errorf("totalBytes = %d, want > 0", u.totalBytes)
	}
	if u.usedBytes < 0 {
		t.Errorf("usedBytes = %d, want >= 0", u.usedBytes)
	}
	if u.availableBytes < 0 {
		t.Errorf("availableBytes = %d, want >= 0", u.availableBytes)
	}
	if u.usedBytes > u.totalBytes {
		t.Errorf("usedBytes = %d > totalBytes = %d", u.usedBytes, u.totalBytes)
	}
	if u.availableBytes > u.totalBytes {
		t.Errorf("availableBytes = %d > totalBytes = %d", u.availableBytes, u.totalBytes)
	}
}

// TestStatDisk_NonexistentPath は存在しないパスに対してエラーを返すことを確認する
// （StorageSyncWorker が「その root の観測に失敗した」と判定する経路）。
func TestStatDisk_NonexistentPath(t *testing.T) {
	_, err := statDisk("/nonexistent-path-for-rokuban-diskusage-test")
	if err == nil {
		t.Fatal("statDisk() on a nonexistent path succeeded, want error")
	}
}
