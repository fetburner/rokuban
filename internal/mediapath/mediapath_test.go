package mediapath

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	mediaDir := "/storage/media"

	tests := []struct {
		name    string
		relPath string
		want    string
		wantErr bool
	}{
		{
			name:    "通常の相対パス",
			relPath: "2026/07/recording.m2ts",
			want:    "/storage/media/2026/07/recording.m2ts",
		},
		{
			name:    "単一要素",
			relPath: "recording.m2ts",
			want:    "/storage/media/recording.m2ts",
		},
		{
			name:    "冗長な要素は正規化される",
			relPath: "./a/./b/../recording.m2ts",
			want:    "/storage/media/a/recording.m2ts",
		},
		{
			// filepath.Join は先頭の / を落とすため media 配下に収まる
			name:    "絶対パスに見えるものは media 配下に解決される",
			relPath: "/etc/passwd",
			want:    "/storage/media/etc/passwd",
		},
		{
			name:    "上位への脱出",
			relPath: "../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "途中の .. で脱出",
			relPath: "a/../../b",
			wantErr: true,
		},
		{
			name:    "media ディレクトリ自身",
			relPath: ".",
			wantErr: true,
		},
		{
			name:    "空文字",
			relPath: "",
			wantErr: true,
		},
		{
			// /storage/media-other へ抜けないこと（接頭辞一致のすり抜け）
			name:    "接頭辞が一致する兄弟ディレクトリ",
			relPath: "../media-other/x.m2ts",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(mediaDir, tt.relPath)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q, %q) = %q, want error", mediaDir, tt.relPath, got)
				}
				if !errors.Is(err, ErrEscapesMediaDir) {
					t.Errorf("error = %v, want ErrEscapesMediaDir", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q, %q) error: %v", mediaDir, tt.relPath, err)
			}
			if got != filepath.Clean(tt.want) {
				t.Errorf("Resolve(%q, %q) = %q, want %q", mediaDir, tt.relPath, got, tt.want)
			}
		})
	}
}

// mediaDir 側に末尾スラッシュや冗長な要素があっても判定が変わらないこと。
func TestResolve_MediaDirNormalization(t *testing.T) {
	for _, dir := range []string{"/storage/media", "/storage/media/", "/storage/./media"} {
		t.Run(dir, func(t *testing.T) {
			got, err := Resolve(dir, "a/b.m2ts")
			if err != nil {
				t.Fatalf("Resolve(%q): %v", dir, err)
			}
			if got != "/storage/media/a/b.m2ts" {
				t.Errorf("Resolve(%q) = %q", dir, got)
			}
			if _, err := Resolve(dir, "../escape"); err == nil {
				t.Errorf("Resolve(%q, \"../escape\") should fail", dir)
			}
		})
	}
}

func TestResolve_RejectsReservedLockDirectory(t *testing.T) {
	for _, relPath := range []string{MediaRelPathLockDirName, MediaRelPathLockDirName + "/recording.m2ts", "./" + MediaRelPathLockDirName + "/.gate.lock"} {
		if got, err := Resolve("/storage/media", relPath); !errors.Is(err, ErrReservedMediaPath) {
			t.Errorf("Resolve(%q) = %q, %v; want ErrReservedMediaPath", relPath, got, err)
		}
	}
}

// TestSubtitleSibling は encoded アセット隣の .vtt サイドカーのパス規則を固定する。
// この規則は worker（生成・削除）と streamer（配信）が共有するので、片方だけ
// 変えると「作ったファイルを誰も配れない / 消せない」形でずれる。
// 壊し方: ".vtt" を ".webvtt" にする / TrimSuffix を消して拡張子を残す。
func TestSubtitleSibling(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "rel_path", path: "20240101/show_web.mp4", want: "20240101/show_web.vtt"},
		{name: "絶対パス（scratch）", path: "/scratch/enc/42/out.mkv", want: "/scratch/enc/42/out.vtt"},
		{name: "basename に . を含む", path: "a/show.v2.mp4", want: "a/show.v2.vtt"},
		{name: "拡張子が無い", path: "20240101/show_web", wantErr: true},
		{name: "空", path: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SubtitleSibling(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SubtitleSibling(%q) = %q, nil; want error", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubtitleSibling(%q) returned error: %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("SubtitleSibling(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsIngestTempFile(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "reserved basename", path: ".rokuban-ingest-1234", want: true},
		{name: "reserved basename with media extension", path: "sites/default/.rokuban-ingest-abcd.m2ts", want: true},
		{name: "canonical media file", path: "sites/default/recording.m2ts", want: false},
		{name: "similar but different prefix", path: ".rokuban-ingest", want: false},
		{name: "ordinary hidden file", path: ".partial-recording.m2ts", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIngestTempFile(tt.path); got != tt.want {
				t.Errorf("IsIngestTempFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsStorageTransferTempAndCheckpointFiles(t *testing.T) {
	for _, tc := range []struct {
		path           string
		wantTemp       bool
		wantCheckpoint bool
	}{
		{path: "sites/default/.rokuban-transfer-12-34", wantTemp: true},
		{path: ".rokuban-transfer-12-34.checkpoint", wantCheckpoint: true},
		{path: ".rokuban-transfer-12-34.checkpoint.tmp-random", wantCheckpoint: true},
		{path: ".rokuban-transfer-not-a-checkpoint.txt", wantTemp: true},
		{path: ".rokuban-transfer-12-34.checkpoint.tmp-random.extra"},
		{path: "sites/default/program.ts"},
	} {
		if got := IsStorageTransferTempFile(tc.path); got != tc.wantTemp {
			t.Errorf("IsStorageTransferTempFile(%q) = %v, want %v", tc.path, got, tc.wantTemp)
		}
		if got := IsStorageTransferCheckpointFile(tc.path); got != tc.wantCheckpoint {
			t.Errorf("IsStorageTransferCheckpointFile(%q) = %v, want %v", tc.path, got, tc.wantCheckpoint)
		}
	}
}

func TestIsGeneratedAssetTempFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: ".rokuban-media-asset-deadbeef", want: true},
		{path: "thumbnails/.rokuban-media-asset-deadbeef", want: true},
		{path: "thumbnails/42.jpg", want: false},
		{path: ".rokuban-media-asset", want: false},
	}
	for _, tt := range tests {
		if got := IsGeneratedAssetTempFile(tt.path); got != tt.want {
			t.Errorf("IsGeneratedAssetTempFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsMediaRelPathLockFile(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "reserved basename", path: ".rokuban-rel-path-lock-deadbeef.lock", want: true},
		{name: "nested path", path: "sites/default/archive/.rokuban-rel-path-lock-deadbeef.lock", want: true},
		{name: "canonical media file", path: "sites/default/recording.m2ts", want: false},
		{name: "similar but different prefix", path: ".rokuban-rel-path-lock", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMediaRelPathLockFile(tt.path); got != tt.want {
				t.Errorf("IsMediaRelPathLockFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestResolve_ReservesRootStoreMarkerOnly(t *testing.T) {
	for _, relPath := range []string{StoreMarkerFileName, "./" + StoreMarkerFileName, "sub/../" + StoreMarkerFileName} {
		if got, err := Resolve("/storage/media", relPath); !errors.Is(err, ErrReservedMediaPath) {
			t.Errorf("Resolve(%q) = %q, %v; want ErrReservedMediaPath", relPath, got, err)
		}
	}
	for relPath, want := range map[string]string{
		"x/" + StoreMarkerFileName:       "/storage/media/x/" + StoreMarkerFileName,
		StoreMarkerFileName + ".ts":      "/storage/media/" + StoreMarkerFileName + ".ts",
		"sites/a/" + StoreMarkerFileName: "/storage/media/sites/a/" + StoreMarkerFileName,
	} {
		if got, err := Resolve("/storage/media", relPath); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", relPath, got, err, want)
		}
	}
}

func TestParseStorageTransferTempFile(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		in                           string
		wantAsset, wantCopy, wantGen int64
		wantOK                       bool
	}{
		{name: "generation form", in: "sites/x/.rokuban-transfer-12-34-g5", wantAsset: 12, wantCopy: 34, wantGen: 5, wantOK: true},
		{name: "legacy form has generation zero", in: ".rokuban-transfer-12-34", wantAsset: 12, wantCopy: 34, wantGen: 0, wantOK: true},
		{name: "sidecar is not a temp", in: ".rokuban-transfer-12-34-g5.checkpoint", wantOK: false},
		{name: "legacy sidecar is not a temp", in: ".rokuban-transfer-12-34.checkpoint", wantOK: false},
		{name: "generation without g prefix", in: ".rokuban-transfer-12-34-5", wantOK: false},
		{name: "zero generation", in: ".rokuban-transfer-12-34-g0", wantOK: false},
		{name: "signed generation", in: ".rokuban-transfer-12-34-g+5", wantOK: false},
		{name: "non-numeric copy id", in: ".rokuban-transfer-12-x-g5", wantOK: false},
		{name: "too many fields", in: ".rokuban-transfer-1-2-3-g4", wantOK: false},
		{name: "not a transfer temp", in: "sites/x/.rokuban-ingest-abc.ts", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asset, copyID, gen, ok := ParseStorageTransferTempFile(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseStorageTransferTempFile(%q) ok = %t, want %t", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if asset != tc.wantAsset || copyID != tc.wantCopy || gen != tc.wantGen {
				t.Fatalf("ParseStorageTransferTempFile(%q) = (%d, %d, %d), want (%d, %d, %d)", tc.in, asset, copyID, gen, tc.wantAsset, tc.wantCopy, tc.wantGen)
			}
		})
	}
}
