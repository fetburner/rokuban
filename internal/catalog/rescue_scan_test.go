package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueLatest_NoCatalogScansBareAssetsIdempotently(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	at := time.Date(2026, 7, 30, 3, 4, 5, 0, time.UTC)

	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(mediaDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("archive/show.m2ts", "original bytes")
	write("archive/movie.mp4", "encoded bytes")
	write("archive/notes.txt", "not media")
	// catalog/ は拡張子が動画でも必ず除外する。
	write("catalog/old-backup.mp4", "not a media asset")

	result, err := RescueLatest(context.Background(), pool, mediaDir, "default")
	if err != nil {
		t.Fatalf("RescueLatest without catalog: %v", err)
	}
	if result.CatalogPath != "" || result.Recordings != 2 || result.MediaAssets != 2 {
		t.Fatalf("scan result = %+v, want no catalog path and 2 recordings/assets", result)
	}

	rows, err := pool.Query(context.Background(), `
		SELECT r.title, r.network_id, r.service_name,
		       a.kind, COALESCE(a.profile, ''), a.rel_path, a.size_bytes
		FROM recordings r JOIN media_assets a ON a.recording_id = r.id
		ORDER BY a.rel_path
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type gotRow struct {
		title, serviceName, kind, profile, relPath string
		networkID                                  int32
		size                                       int64
	}
	var got []gotRow
	for rows.Next() {
		var row gotRow
		if err := rows.Scan(&row.title, &row.networkID, &row.serviceName,
			&row.kind, &row.profile, &row.relPath, &row.size); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("registered rows = %+v", got)
	}
	if got[0].relPath != "archive/movie.mp4" || got[0].kind != "encoded" ||
		got[0].profile != "rescue-mp4" || got[0].title != "movie" || got[0].size != 13 {
		t.Errorf("mp4 row = %+v", got[0])
	}
	if got[1].relPath != "archive/show.m2ts" || got[1].kind != "original" ||
		got[1].profile != "" || got[1].title != "show" || got[1].size != 14 {
		t.Errorf("m2ts row = %+v", got[1])
	}
	for _, row := range got {
		if row.networkID >= 0 || row.serviceName != "Recovered file (metadata unavailable)" {
			t.Errorf("synthetic metadata not explicit: %+v", row)
		}
	}

	// 再実行で同じ合成 identity / asset tuple を upsert し、増殖しない。
	if _, err := RescueLatest(context.Background(), pool, mediaDir, "default"); err != nil {
		t.Fatalf("second RescueLatest: %v", err)
	}
	var recordingCount, assetCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&recordingCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets`).Scan(&assetCount); err != nil {
		t.Fatal(err)
	}
	if recordingCount != 2 || assetCount != 2 {
		t.Fatalf("rows after second scan = recordings %d, assets %d; want 2/2", recordingCount, assetCount)
	}

	// in-place 登録はファイル本体を変更しない。
	body, err := os.ReadFile(filepath.Join(mediaDir, "archive", "show.m2ts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original bytes" {
		t.Errorf("rescued file content changed: %q", body)
	}
}

// アーカイブは全 site で共有される単一のストレージなので、`sites/{site}/` 前置
// ファイルは前置を持つ他 site の分も同じスキャンで見つかる。前置ありのファイルは
// prefix から site を決め、`--site` の値（引数の "tokyo"）と食い違っても
// （takamatsu のファイル）prefix を正として復元する --- 除外すると災害復旧で
// その site の録画だけ黙って復元されなくなる。前置の無いファイルは前置導入前
// （単一 site 時代）の ingest なので `--site` にフォールバックする
// （issue #533 の「含むもの」3）。
func TestRescueLatest_ScansSitePrefixedAssetsUsingThePrefixSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	at := time.Date(2026, 7, 30, 3, 4, 5, 0, time.UTC)

	write := func(rel string) {
		t.Helper()
		path := filepath.Join(mediaDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("sites/tokyo/show.m2ts")
	write("sites/takamatsu/movie.mp4")
	write("legacy/old.m2ts")

	// `rescue --site tokyo` を模す: 呼び出し側は "tokyo" しか渡さないが、
	// takamatsu 前置のファイルも takamatsu として復元されなければならない。
	result, err := RescueLatest(context.Background(), pool, mediaDir, "tokyo")
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.Recordings != 3 || result.MediaAssets != 3 {
		t.Fatalf("result = %+v, want 3 recordings/assets", result)
	}

	siteOf := func(t *testing.T, relPath string) string {
		t.Helper()
		var site string
		err := pool.QueryRow(context.Background(), `
			SELECT r.site FROM recordings r JOIN media_assets a ON a.recording_id = r.id
			WHERE a.rel_path = $1
		`, relPath).Scan(&site)
		if err != nil {
			t.Fatalf("querying site for %s: %v", relPath, err)
		}
		return site
	}

	if got := siteOf(t, "sites/tokyo/show.m2ts"); got != "tokyo" {
		t.Errorf("site for sites/tokyo/show.m2ts = %q, want tokyo", got)
	}
	if got := siteOf(t, "sites/takamatsu/movie.mp4"); got != "takamatsu" {
		t.Errorf("site for sites/takamatsu/movie.mp4 = %q, want takamatsu (prefix must win over --site=tokyo)", got)
	}
	if got := siteOf(t, "legacy/old.m2ts"); got != "tokyo" {
		t.Errorf("site for legacy/old.m2ts = %q, want tokyo (no prefix, falls back to --site)", got)
	}
}
