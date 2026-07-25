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
