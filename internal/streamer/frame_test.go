package streamer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// ffmpeg は frame、ffprobe は start_time 0 と SAR 4:3 のコマを返す代役に差し替える。
func newFrameFixture(t *testing.T, frame []byte) (*frameFixture, int64) {
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
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg"})
	s.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{filepath.Base(name)}, args...))
		switch {
		case filepath.Base(name) == "ffmpeg":
			return frame, nil
		case strings.Contains(strings.Join(args, " "), "format=start_time"):
			return []byte("0.000000\n"), nil
		case filepath.Base(name) == "ffprobe":
			return []byte("12.480000,1440,1080,4:3\n12.520000,1440,1080,4:3\n"), nil
		}
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	r := chi.NewRouter()
	s.Mount(r)
	f.srv = httptest.NewServer(r)
	t.Cleanup(f.srv.Close)
	return f, id
}

// fakeJPEG は SOF0 だけを持つ最小の JPEG を作る。
func fakeJPEG(w, h int) []byte {
	return []byte{0xFF, 0xD8,
		0xFF, 0xC0, 0x00, 0x08, 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 1,
		0xFF, 0xD9}
}

func (f *frameFixture) url(id int64, query string) string {
	return fmt.Sprintf("%s/api/media/recordings/%d/frame%s", f.srv.URL, id, query)
}

// 原本の指定位置のコマを、記録上の大きさ付きで返す。**encoded は使わない。**
func TestRecordingFrameExtractsFromTheOriginalWithTheRecordedSize(t *testing.T) {
	jpeg := fakeJPEG(1440, 1080)
	f, id := newFrameFixture(t, jpeg)
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
	if got := res.Header.Get("X-Sample-Aspect-Ratio"); got != "4:3" {
		t.Errorf("X-Sample-Aspect-Ratio = %q, want 4:3", got)
	}
	if string(body) != string(jpeg) {
		t.Errorf("body = %v, want the extracted frame", body)
	}
	if len(f.calls) != 3 {
		t.Fatalf("commands = %v, want ffmpeg then two ffprobe", f.calls)
	}
	ffmpeg := f.calls[0]
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
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg"})
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
	f, id := newFrameFixture(t, fakeJPEG(1440, 1080))
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

	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: "ffmpeg"})
	s.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
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

// 実物の ffmpeg / ffprobe で MPEG-2 1440x1080（SAR 4:3）の TS を配る。偽の runCmd では、
// 実 ffprobe の -read_intervals と csv 出力から SAR を読めることを測れない。
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
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: ffmpeg})
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
	if got := res.Header.Get("X-Sample-Aspect-Ratio"); got != "4:3" {
		t.Errorf("sample aspect ratio = %q, want 4:3", got)
	}
}

// 途中で解像度と SAR が変わる録画（1440x1080 SAR 4:3 の後に 720x480 SAR 32:27）で、
// 後半の at を叩いたらヘッダは後半の値になる。先頭を probe する実装では 1440x1080 / 4:3 のまま。
func TestRecordingFrameFollowsResolutionAndSARChange(t *testing.T) {
	testFrameFollowsChange(t, "0")
}

// 33bit wrap 直前に始まる TS は start_time が負になる（-output_ts_offset 95441 で
// start_time = -1.317689 を実測）。そこでも窓が空にならず、同じ値を返す。
func TestRecordingFrameFollowsChangeWhenStartTimeIsNegative(t *testing.T) {
	testFrameFollowsChange(t, "95441")
}

func testFrameFollowsChange(t *testing.T, baseOffset string) {
	t.Helper()
	ffmpeg := lookPathFFmpeg(t)
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	dir := filepath.Join(mediaDir, "gr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gen := func(name, size, sar, offset string) string {
		out := filepath.Join(dir, name)
		if b, err := exec.Command(ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size="+size+":rate=25",
			"-t", "2", "-vf", "setsar="+sar, "-g", "5", "-c:v", "mpeg2video",
			"-output_ts_offset", offset, "-f", "mpegts", out).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, b)
		}
		return out
	}
	first := gen("a.ts", "1440x1080", "4/3", baseOffset)
	second := gen("b.ts", "720x480", "32/27", strconv.FormatFloat(mustFloat(t, baseOffset)+2, 'f', -1, 64))
	// TS は連結できる。後半は pts を 2 秒ずらしてあり、at=2 秒以降が後半の解像度になる。
	relPath := "gr/joined.ts"
	var joined []byte
	for _, p := range []string{first, second} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, b...)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, relPath), joined, 0o644); err != nil {
		t.Fatal(err)
	}
	id := seedRecording(t, pool)
	seedAsset(t, pool, id, relPath, int64(len(joined)))
	s := New(pool, Config{MediaDir: mediaDir, FFmpeg: ffmpeg})
	r := chi.NewRouter()
	s.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	for _, tc := range []struct{ at, size, sar string }{
		{"500", "1440x1080", "4:3"},
		{"1900", "1440x1080", "4:3"},
		{"1930", "1440x1080", "4:3"},
		{"1950", "720x480", "32:27"},
		{"1970", "720x480", "32:27"},
		{"1990", "720x480", "32:27"},
		{"2100", "720x480", "32:27"},
		{"3000", "720x480", "32:27"},
	} {
		res, body := get(t, fmt.Sprintf("%s/api/media/recordings/%d/frame?at=%s", srv.URL, id, tc.at), nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("at=%s: status = %d (body %q)", tc.at, res.StatusCode, body)
		}
		if got := res.Header.Get("X-Coded-Width") + "x" + res.Header.Get("X-Coded-Height"); got != tc.size {
			t.Errorf("at=%s: coded size = %s, want %s", tc.at, got, tc.size)
		}
		if got := res.Header.Get("X-Sample-Aspect-Ratio"); got != tc.sar {
			t.Errorf("at=%s: SAR = %q, want %s", tc.at, got, tc.sar)
		}
	}
}

func mustFloat(t *testing.T, v string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// pickFrame は pts が at 以上の最初のコマを採り、大きさが JPEG と違えば 1 つ前を見る。
// SAR が不明なら 1:1 にする。
func TestPickFrame(t *testing.T) {
	two := "1.00,1440,1080,4:3\n1.04,720,480,32:27\n"
	hd, sd := probedFrame{1440, 1080, "4:3"}, probedFrame{720, 480, "32:27"}
	for _, tc := range []struct {
		name, csv string
		target    float64
		w, h      int
		want      probedFrame
	}{
		{"exactly on a frame", two, 1.04, 720, 480, sd},
		{"between frames takes the next", two, 1.02, 720, 480, sd},
		{"next does not match the JPEG, takes the previous", two, 1.02, 1440, 1080, hd},
		{"before the first", two, 0.5, 1440, 1080, hd},
		{"after the last takes the last", two, 9, 720, 480, sd},
		{"0:1", "1.0,720,480,0:1\n", 1, 720, 480, probedFrame{720, 480, "1:1"}},
		{"N/A", "1.0,720,480,N/A\n", 1, 720, 480, probedFrame{720, 480, "1:1"}},
		{"empty", "1.0,720,480,\n", 1, 720, 480, probedFrame{720, 480, "1:1"}},
		{"negative pts", "-1.0,720,480,8:9\n-0.96,720,480,8:9\n", -0.97, 720, 480, probedFrame{720, 480, "8:9"}},
	} {
		got, err := pickFrame(tc.csv, tc.target, tc.w, tc.h)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	if _, err := pickFrame("", 1, 720, 480); err == nil {
		t.Error("no frames: want an error")
	}
	if _, err := pickFrame(two, 1.04, 1920, 1080); err == nil {
		t.Error("no frame matches the JPEG: want an error")
	}
}

// ffmpeg が返した JPEG と ffprobe のコマの大きさが違うなら、SAR を信用できないので 500。
func TestRecordingFrameRejectsSizeMismatchBetweenJPEGAndProbe(t *testing.T) {
	f, id := newFrameFixture(t, fakeJPEG(720, 480)) // fake ffprobe は 1440x1080 を返す
	res, _ := get(t, f.url(id, "?at=12500"), nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}
