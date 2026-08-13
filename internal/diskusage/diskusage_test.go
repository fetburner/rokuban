package diskusage

import (
	"testing"
)

// TestStat_RealTempDir は実ディレクトリに対して statfs が呼べ、値が
// 物理的にあり得る範囲に収まることを確認する。CI ランナーやローカルの
// ディスクサイズは環境依存で決定的な値を主張できないため、絶対値ではなく
// 不変条件（total >= used、avail <= total、total > 0）だけを検査する。
func TestStat_RealTempDir(t *testing.T) {
	dir := t.TempDir()

	u, err := Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", dir, err)
	}

	if u.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d, want > 0", u.TotalBytes)
	}
	if u.UsedBytes < 0 {
		t.Errorf("UsedBytes = %d, want >= 0", u.UsedBytes)
	}
	if u.AvailableBytes < 0 {
		t.Errorf("AvailableBytes = %d, want >= 0", u.AvailableBytes)
	}
	if u.UsedBytes > u.TotalBytes {
		t.Errorf("UsedBytes = %d > TotalBytes = %d", u.UsedBytes, u.TotalBytes)
	}
	if u.AvailableBytes > u.TotalBytes {
		t.Errorf("AvailableBytes = %d > TotalBytes = %d", u.AvailableBytes, u.TotalBytes)
	}
}

// TestStat_NonexistentPath は存在しないパスに対してエラーを返すことを確認する
// （worker.StorageSyncWorker が「その root の観測に失敗した」と判定する経路）。
func TestStat_NonexistentPath(t *testing.T) {
	_, err := Stat("/nonexistent-path-for-rokuban-diskusage-test")
	if err == nil {
		t.Fatal("Stat() on a nonexistent path succeeded, want error")
	}
}
