package streamer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// frameFixture はコマ切り出しの配信サーバーと、実行されたコマンドの記録を返す。
type frameFixture struct {
	srv   *httptest.Server
	calls [][]string
	pool  *pgxpool.Pool
}

// newFrameFixture は原本を 1 本持つ録画で /frame を配るサーバーを作る。
// ffprobe は size、ffmpeg は frame を返す代役に差し替える。
func newFrameFixture(t *testing.T, size string, frame []byte) (*frameFixture, int64) {
	t.Helper()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	relPath := "gr/frame.ts"
	full := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, makeTSData(10), 0o644); err != nil {
		t.Fatal(err)
	}
	id := seedRecording(t, pool)
	seedAsset(t, pool, id, relPath, int64(len(makeTSData(10))))

	f := &frameFixture{pool: pool}
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg", FFprobe: "ffprobe"})
	s.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{filepath.Base(name)}, args...))
		switch filepath.Base(name) {
		case "ffprobe":
			return []byte(probeJSON(size)), nil
		case "ffmpeg":
			return frame, nil
		}
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	r := chi.NewRouter()
	s.Mount(r)
	f.srv = httptest.NewServer(r)
	t.Cleanup(f.srv.Close)
	return f, id
}

// probeJSON は size（"1440x1080"）を、実 ffprobe 9.0.2 が MPEG-TS に対して
// `-of json` で返す形（programs 側と streams 側の 2 回出る）にする。
func probeJSON(size string) string {
	w, h, _ := strings.Cut(size, "x")
	stream := fmt.Sprintf(`{"width": %s, "height": %s}`, w, h)
	return fmt.Sprintf(`{"programs": [{"streams": [%s]}], "stream_groups": [], "streams": [%s]}`, stream, stream)
}

func (f *frameFixture) url(id int64, query string) string {
	return fmt.Sprintf("%s/api/media/recordings/%d/frame%s", f.srv.URL, id, query)
}

// 原本の指定位置のコマを、記録上の大きさ付きで返す。**encoded は使わない。**
func TestRecordingFrameExtractsFromTheOriginalWithTheRecordedSize(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	f, id := newFrameFixture(t, "1440x1080", jpeg)
	// encoded を持たせても、コマは原本から取る（縮小済みの座標は使えない）。
	profile := "h264"
	if _, err := sqlcgen.New(f.pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindEncoded, Profile: &profile,
		RelPath: "gr/frame-h264.mp4", SizeBytes: 3,
	}); err != nil {
		t.Fatal(err)
	}

	res, body := get(t, f.url(id, "?at=12500"), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if got := res.Header.Get("X-Coded-Width"); got != "1440" {
		t.Errorf("X-Coded-Width = %q, want 1440", got)
	}
	if got := res.Header.Get("X-Coded-Height"); got != "1080" {
		t.Errorf("X-Coded-Height = %q, want 1080", got)
	}
	if string(body) != string(jpeg) {
		t.Errorf("body = %v, want the extracted frame", body)
	}
	if len(f.calls) != 2 {
		t.Fatalf("commands = %v, want ffprobe then ffmpeg", f.calls)
	}
	ffmpeg := f.calls[1]
	if ffmpeg[0] != "ffmpeg" {
		t.Fatalf("second command = %q, want ffmpeg", ffmpeg[0])
	}
	args := strings.Join(ffmpeg[1:], " ")
	if !strings.Contains(args, "-ss 12.500") {
		t.Errorf("ffmpeg args = %q, want the position as seconds before -i", args)
	}
	if !strings.Contains(args, filepath.Join("gr", "frame.ts")) {
		t.Errorf("ffmpeg args = %q, want the original", args)
	}
	if strings.Contains(args, "mp4") {
		t.Errorf("ffmpeg args = %q, want the original instead of the encoded asset", args)
	}
	// 縮小も SAR の焼き込みもしない（教える座標は記録上の画素）。
	if strings.Contains(args, "scale") || strings.Contains(args, "setsar") {
		t.Errorf("ffmpeg args = %q, want no scaling and no SAR", args)
	}
}

// ごみ箱・原本の無い録画・実体の無い原本は 404。**ffmpeg は回さない。**
func TestRecordingFrameNotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	f := &frameFixture{pool: pool}
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg", FFprobe: "ffprobe"})
	s.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{filepath.Base(name)}, args...))
		return nil, fmt.Errorf("must not run %s", name)
	}
	r := chi.NewRouter()
	s.Mount(r)
	f.srv = httptest.NewServer(r)
	t.Cleanup(f.srv.Close)

	// 原本が無い（encoded だけ）。
	encodedOnly := seedRecordingWithEvent(t, pool, 101)
	profile := "h264"
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: encodedOnly, Kind: db.AssetKindEncoded, Profile: &profile,
		RelPath: "gr/only-encoded.mp4", SizeBytes: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// ごみ箱の録画（原本の行はある）。
	trashed := seedRecordingWithEvent(t, pool, 102)
	seedAsset(t, pool, trashed, "gr/trashed.ts", 1880)
	if _, err := pool.Exec(context.Background(),
		`UPDATE recordings SET deleted_at = now() WHERE id = $1`, trashed); err != nil {
		t.Fatal(err)
	}

	// 行はあるが実体が無い。
	missing := seedRecordingWithEvent(t, pool, 103)
	seedAsset(t, pool, missing, "gr/missing.ts", 1880)

	for name, id := range map[string]int64{"no original": encodedOnly, "trashed": trashed, "missing file": missing} {
		res, _ := get(t, f.url(id, "?at=0"), nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, res.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("commands = %v, want none for 404s", f.calls)
	}
	res, _ := get(t, f.url(encodedOnly+1000, "?at=0"), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown recording: status = %d, want 404", res.StatusCode)
	}
}

// at が無い・負・数値でないのは 400。
func TestRecordingFrameRejectsInvalidPosition(t *testing.T) {
	f, id := newFrameFixture(t, "1440x1080", []byte{0xFF, 0xD8})
	for _, query := range []string{"", "?at=", "?at=-1", "?at=abc"} {
		res, _ := get(t, f.url(id, query), nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("at=%q: status = %d, want 400", query, res.StatusCode)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("commands = %v, want none for invalid positions", f.calls)
	}
}

// ffmpeg が失敗したら 500（枠を教える画面は「取り寄せできない」と出す）。
func TestRecordingFrameReportsExtractionFailure(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	relPath := "gr/broken.ts"
	full := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, makeTSData(10), 0o644); err != nil {
		t.Fatal(err)
	}
	id := seedRecording(t, pool)
	seedAsset(t, pool, id, relPath, int64(len(makeTSData(10))))

	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg", FFprobe: "ffprobe"})
	s.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if filepath.Base(name) == "ffprobe" {
			return []byte(probeJSON("1440x1080")), nil
		}
		return nil, fmt.Errorf("ffmpeg: exit status 1: Output file #0 does not contain any stream")
	}
	r := chi.NewRouter()
	s.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	res, _ := get(t, fmt.Sprintf("%s/api/media/recordings/%d/frame?at=900000", srv.URL, id), nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

// 実物の ffprobe / ffmpeg で MPEG-2 1440x1080（SAR 4:3）の TS を配る。偽の runCmd では
// ffprobe の実出力の形（programs 側と streams 側の 2 回出る）を読めることを測れない。
func TestRecordingFrameWithRealFFmpegOnAnamorphicMPEG2(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	relPath := "gr/real.ts"
	full := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=1440x1080:rate=25",
		"-t", "3", "-vf", "setsar=4/3", "-c:v", "mpeg2video", "-f", "mpegts", full).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	id := seedRecording(t, pool)
	seedAsset(t, pool, id, relPath, info.Size())
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: ffmpeg, FFprobe: "ffprobe"})
	r := chi.NewRouter()
	s.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	res, body := get(t, fmt.Sprintf("%s/api/media/recordings/%d/frame?at=1000", srv.URL, id), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", res.StatusCode, body)
	}
	if len(body) < 4 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Errorf("body is not a JPEG (%d bytes)", len(body))
	}
	if got := res.Header.Get("X-Coded-Width") + "x" + res.Header.Get("X-Coded-Height"); got != "1440x1080" {
		t.Errorf("coded size = %s, want 1440x1080", got)
	}
}
