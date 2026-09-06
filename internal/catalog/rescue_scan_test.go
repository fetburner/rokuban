package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
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

	result, err := RescueLatest(context.Background(), pool, mediaDir, "default", []string{"default"})
	if err != nil {
		t.Fatalf("RescueLatest without catalog: %v", err)
	}
	if result.CatalogPath != "" || result.Recordings != 2 || result.MediaAssets != 2 {
		t.Fatalf("scan result = %+v, want no catalog path and 2 recordings/assets", result)
	}

	rows, err := pool.Query(context.Background(), `
		SELECT r.title, r.source, r.network_id, r.service_name,
		       a.kind, COALESCE(a.profile, ''), a.rel_path, a.size_bytes
		FROM recordings r JOIN media_assets a ON a.recording_id = r.id
		ORDER BY a.rel_path
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type gotRow struct {
		title, source, serviceName, kind, profile, relPath string
		networkID                                          int32
		size                                               int64
	}
	var got []gotRow
	for rows.Next() {
		var row gotRow
		if err := rows.Scan(&row.title, &row.source, &row.networkID, &row.serviceName,
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
		if row.source != reservation.SourceUnattributed {
			t.Errorf("rescued source = %q, want %q: %+v", row.source, reservation.SourceUnattributed, row)
		}
		if row.networkID >= 0 || row.serviceName != "Recovered file (metadata unavailable)" {
			t.Errorf("synthetic metadata not explicit: %+v", row)
		}
	}

	// 再実行で同じ合成 identity / asset tuple を upsert し、増殖しない。
	if _, err := RescueLatest(context.Background(), pool, mediaDir, "default", []string{"default"}); err != nil {
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

func TestRescueLatest_ReusesRecordingWhenDeletedAssetMtimeChanges(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	path := filepath.Join(mediaDir, "archive", "show.m2ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstAt := time.Date(2026, 7, 30, 3, 4, 5, 0, time.UTC)
	secondAt := firstAt.Add(24 * time.Hour)
	if err := os.Chtimes(path, firstAt, firstAt); err != nil {
		t.Fatal(err)
	}

	if _, err := RescueLatest(context.Background(), pool, mediaDir, "default", []string{"default"}); err != nil {
		t.Fatalf("first RescueLatest: %v", err)
	}
	var recordingID, assetID int64
	if err := pool.QueryRow(context.Background(), `
		SELECT r.id, a.id
		FROM recordings r JOIN media_assets a ON a.recording_id = r.id
		WHERE a.rel_path = $1
	`, "archive/show.m2ts").Scan(&recordingID, &assetID); err != nil {
		t.Fatal(err)
	}

	// Reconcile can mark the asset deleted while the physical file remains. A later
	// rescue must treat that file as the same recording even if its mtime changed.
	if _, err := pool.Exec(context.Background(), `
		UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1
	`, assetID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, secondAt, secondAt); err != nil {
		t.Fatal(err)
	}

	if _, err := RescueLatest(context.Background(), pool, mediaDir, "default", []string{"default"}); err != nil {
		t.Fatalf("second RescueLatest: %v", err)
	}

	var recordings, assets, liveRecordings int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&recordings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM recordings
		WHERE deleted_at IS NULL AND superseded_at IS NULL
	`).Scan(&liveRecordings); err != nil {
		t.Fatal(err)
	}
	if recordings != 1 || assets != 1 || liveRecordings != 1 {
		t.Fatalf("rows after rescue = recordings %d, assets %d, live recordings %d; want 1/1/1",
			recordings, assets, liveRecordings)
	}

	var gotAssetID, gotRecordingID int64
	var state string
	var deletedAtIsNull bool
	var programStartAt time.Time
	if err := pool.QueryRow(context.Background(), `
		SELECT a.id, a.recording_id, a.state, a.deleted_at IS NULL, r.program_start_at
		FROM media_assets a JOIN recordings r ON r.id = a.recording_id
		WHERE a.rel_path = $1
	`, "archive/show.m2ts").Scan(
		&gotAssetID, &gotRecordingID, &state, &deletedAtIsNull, &programStartAt,
	); err != nil {
		t.Fatal(err)
	}
	if gotAssetID != assetID || gotRecordingID != recordingID || state != "active" || !deletedAtIsNull {
		t.Fatalf("rescued asset = id %d, recording %d, state %q, deleted_at NULL %v; want original active asset",
			gotAssetID, gotRecordingID, state, deletedAtIsNull)
	}
	if !programStartAt.Equal(firstAt) {
		t.Errorf("program_start_at changed from initial mtime: got %s, want %s", programStartAt, firstAt)
	}
}

func TestRescueLatest_ScansMediaDirSymlinkAndSkipsCatalog(t *testing.T) {
	pool := testutil.SetupDB(t)
	baseDir := t.TempDir()
	realMediaDir := filepath.Join(baseDir, "real-media")
	mediaDir := filepath.Join(baseDir, "media")
	if err := os.MkdirAll(filepath.Join(realMediaDir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realMediaDir, mediaDir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(realMediaDir, "archive", "show.m2ts"), []byte("original bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realMediaDir, "catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realMediaDir, "catalog", "old-backup.mp4"), []byte("not a media asset"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RescueLatest(context.Background(), pool, mediaDir, "default", []string{"default"})
	if err != nil {
		t.Fatalf("RescueLatest through media_dir symlink: %v", err)
	}
	if result.Recordings != 1 || result.MediaAssets != 1 {
		t.Fatalf("scan result = %+v, want 1 recording/asset", result)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets WHERE rel_path = 'archive/show.m2ts'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rescued asset count = %d, want 1", count)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets WHERE rel_path LIKE 'catalog/%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("catalog assets registered = %d, want 0", count)
	}
}

func TestRescueLatest_ReturnsErrorWhenMediaDirCannotBeResolved(t *testing.T) {
	missingMediaDir := filepath.Join(t.TempDir(), "missing-media")
	_, err := RescueLatest(context.Background(), nil, missingMediaDir, "default", []string{"default"})
	if err == nil {
		t.Fatal("RescueLatest with missing media_dir should return an error")
	}
	// symlink 解決の失敗（media_dir が無い）と catalog 読み取りの失敗を issue #665
	// が求める通り文言で区別できることを見る。err == nil だけを見るアサーションだと
	// EvalSymlinks を削除しても（filepath.WalkDir が root の lstat エラーを
	// 返し、それを "scanning media_dir for rescue" が wrap するので）このテストは
	// 通り続けてしまう。
	const want = "resolving media_dir symlinks for rescue"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("RescueLatest with missing media_dir error = %q, want it to contain %q", err.Error(), want)
	}
}

// アーカイブは全 site で共有される単一のストレージなので、`sites/{site}/` 前置
// ファイルは前置を持つ他 site の分も同じスキャンで見つかる。前置ありのファイルは
// prefix から site を決め、`--site` の値（引数の "tokyo"）と食い違っても
// （takamatsu のファイル）prefix を正として復元する --- 除外すると孤児回収の
// 通常の掃除対象になり 2 週間ほどで実削除されてしまう（docs/storage/rescue.md）。
// 前置の無いファイルは前置導入前（単一 site 時代）の ingest なので `--site` に
// フォールバックする。レジストリに無い site の前置（`junkdir`）も typo の疑いは
// あるが復元は止めない --- 一覧・削除は site 非依存なので事後に UI から消せる
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
	write("sites/junkdir/typo.m2ts")
	write("legacy/old.m2ts")

	// `rescue --site tokyo` を模す: 呼び出し側は "tokyo" しか渡さないが、
	// takamatsu / junkdir 前置のファイルもそれぞれの site として復元されなければ
	// ならない。registrySites には junkdir を含めない（レジストリに無い typo の形）。
	result, err := RescueLatest(context.Background(), pool, mediaDir, "tokyo", []string{"tokyo", "takamatsu"})
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.Recordings != 4 || result.MediaAssets != 4 {
		t.Fatalf("result = %+v, want 4 recordings/assets", result)
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
	if got := siteOf(t, "sites/junkdir/typo.m2ts"); got != "junkdir" {
		t.Errorf("site for sites/junkdir/typo.m2ts = %q, want junkdir "+
			"(unregistered prefix site still restores, just under a different log level)", got)
	}
	if got := siteOf(t, "legacy/old.m2ts"); got != "tokyo" {
		t.Errorf("site for legacy/old.m2ts = %q, want tokyo (no prefix, falls back to --site)", got)
	}
}

// classifySiteForRescuedFile はファイルごとに1回だけ判定される非自明な分岐
// （no-prefix フォールバック / 前置一致 / 前置が flag と食い違う / さらにその
// 前置がレジストリに無い、の 4 通り）を持つ純粋関数なので、テーブルテストで
// 直接固定する。境界ケース（`sites/` 単体・`sites/x.m2ts`・二重スラッシュ・
// 先頭以外に現れる `sites/`・大文字・先頭スラッシュ）はすべてフォールバックに
// 倒れることを固定する --- filepath.WalkDir + filepath.Rel を経由した relPath
// では作れない形だが、関数単体としての契約を決めておく。
func TestClassifySiteForRescuedFile(t *testing.T) {
	registrySites := []string{"tokyo", "takamatsu"}
	tests := []struct {
		name                           string
		relPath, flagSite              string
		wantSite                       string
		wantCrossSite, wantUnknownSite bool
	}{
		{"no prefix falls back to flag", "legacy/old.m2ts", "tokyo", "tokyo", false, false},
		{"prefix matches flag", "sites/tokyo/x.m2ts", "tokyo", "tokyo", false, false},
		{"prefix differs, known site", "sites/takamatsu/x.m2ts", "tokyo", "takamatsu", true, false},
		{"prefix differs, unknown site", "sites/junkdir/x.m2ts", "tokyo", "junkdir", true, true},
		{"bare sites dir falls back", "sites/", "tokyo", "tokyo", false, false},
		{"sites/ with no site segment falls back", "sites/x.m2ts", "tokyo", "tokyo", false, false},
		{"empty site segment falls back", "sites//a.ts", "tokyo", "tokyo", false, false},
		{"sites/ not at path start falls back", "a/sites/tokyo/x.ts", "tokyo", "tokyo", false, false},
		{"case-sensitive prefix falls back", "Sites/tokyo/x.ts", "tokyo", "tokyo", false, false},
		{"leading slash falls back", "/sites/tokyo/x.ts", "tokyo", "tokyo", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site, crossSite, unknownSite := classifySiteForRescuedFile(tt.relPath, tt.flagSite, registrySites)
			if site != tt.wantSite || crossSite != tt.wantCrossSite || unknownSite != tt.wantUnknownSite {
				t.Errorf("classifySiteForRescuedFile(%q, %q) = (%q, %v, %v), want (%q, %v, %v)",
					tt.relPath, tt.flagSite, site, crossSite, unknownSite,
					tt.wantSite, tt.wantCrossSite, tt.wantUnknownSite)
			}
		})
	}
}
