package streamer

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

func recordingHLSVideoPlaylistPath() string {
	return recordingHLSRequestPath("main", 42, 7, "video/index.m3u8")
}

// 録画中の playlist は、観測した映像に全体が収まる (n+1)·D ≤ 長さ のセグメントだけを、全て D 秒で載せる。
// 長さが縮んで観測されても、載せた行は減らない。
//
// 壊し方: buildRecordingHLSTempPlaylist の `videoDurationPTS/segmentPTS - 1` を
// `(videoDurationPTS+segmentPTS-1)/segmentPTS - 1`（切り上げ）にする。
// 壊し方: observeInput の `duration > timing.videoDurationPTS` を無条件の代入にする。
func TestRecordingHLSTempPlaylistListsOnlyObservedVideo(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.useTemp()
	// 先頭の映像パケットは 10 秒。D は 2 秒。
	steps := []struct {
		name       string
		endSeconds float64
		wantRows   int
	}{
		{"映像がまだ無い", 10, 0},
		{"1 本目の途中", 11.9, 0},
		{"1 本目がちょうど収まる", 12, 1},
		{"2 本目の途中", 13.9, 1},
		{"2 本目がちょうど収まる", 14, 2},
		{"5 本目の手前", 19.99, 4},
		{"観測が縮んでも載せた行は減らない", 15, 4},
		{"5 本目がちょうど収まる", 20, 5},
	}
	var previous []string
	for _, step := range steps {
		h.setTail(t, recordingHLSTailPackets(step.endSeconds))
		h.growTemp(t)
		response := h.get(t, recordingHLSVideoPlaylistPath())
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", step.name, response.Code, response.Body.String())
		}
		body := response.Body.String()
		rows, endlist := playlistRows(body)
		if endlist || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") {
			t.Fatalf("%s: playlist = %s, want EVENT without ENDLIST", step.name, body)
		}
		if len(rows) != step.wantRows {
			t.Fatalf("%s: %d rows, want %d: %s", step.name, len(rows), step.wantRows, body)
		}
		for n, row := range rows {
			if want := fmt.Sprintf("#EXTINF:2.000, %d.ts", n); row != want {
				t.Errorf("%s: row %d = %q, want %q (every row is D seconds while recording)", step.name, n, row, want)
			}
		}
		if len(rows) < len(previous) || strings.Join(rows[:len(previous)], "\n") != strings.Join(previous, "\n") {
			t.Fatalf("%s: rows %v do not extend the earlier rows %v", step.name, rows, previous)
		}
		previous = rows
	}
}

// 録画が終わった後に ingest が長引き、temp が最終サイズまで伸びても、commit の前に載せた行と
// EXTINF は commit の後も変わらない。commit で足されるのは最後の短い行と ENDLIST だけである。
// 実録画では 240 行の temp の playlist が 182 行の原本に縮んだ。
//
// 壊し方: buildRecordingHLSTempPlaylist を切り上げ（最後の短い行も D 秒で載せる）にする。
func TestRecordingHLSCommitKeepsPublishedRowsWhenIngestOutlastsRecording(t *testing.T) {
	// 映像の長さ 362.0617 秒（実録画の値）。D=2 なので temp は 181 行、原本は最後の 0.062 秒を足した 182 行。
	endTicks := int64(900000) + 362*mpegTSTimestampClock + 5553
	tail := fmt.Sprintf("pts=%d|duration=3600|\n", endTicks-3600)
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", recordingHLSTestProbe{firstPacketPTS: 900000, tail: tail})
	h.useTemp()
	for _, rendition := range []string{"video", "audio-standard", "audio-main", "audio-sub"} {
		path := recordingHLSRequestPath("main", 42, 7, rendition+"/index.m3u8")
		h.useTemp()
		growing := h.get(t, path)
		tempRows, tempEnd := playlistRows(growing.Body.String())
		if growing.Code != http.StatusOK || tempEnd || len(tempRows) != 181 {
			t.Fatalf("%s growing temp = %d, %d rows, endlist %v, want 181 rows without ENDLIST", rendition, growing.Code, len(tempRows), tempEnd)
		}

		h.useOriginal()
		committed := h.get(t, path)
		rows, endlist := playlistRows(committed.Body.String())
		if committed.Code != http.StatusOK || !endlist || len(rows) != 182 {
			t.Fatalf("%s committed = %d, %d rows, endlist %v, want 182 rows with ENDLIST", rendition, committed.Code, len(rows), endlist)
		}
		if strings.Join(rows[:181], "\n") != strings.Join(tempRows, "\n") {
			t.Errorf("%s: rows before commit changed after commit", rendition)
		}
		if rows[181] != "#EXTINF:0.062, 181.ts" {
			t.Errorf("%s: last row = %q, want the short remainder added at commit", rendition, rows[181])
		}
		if !strings.HasPrefix(committed.Body.String(), strings.TrimSuffix(growing.Body.String(), "\n")) {
			t.Errorf("%s: committed playlist does not extend the growing one", rendition)
		}
	}
	if got := h.tailCalls(t); got < 2 {
		t.Errorf("tail measurements = %d, want temp and original to both go through the one measuring function", got)
	}
}

// ENDLIST を付けるのは、原本の行が DB にあるとき（commit の後）だけである。
// record の状態が何であっても、temp の playlist には付けない。
//
// 壊し方: RenditionPlaylist の temp 分岐を buildRecordingHLSRenditionPlaylist にする。
func TestRecordingHLSEndlistOnlyFromTheOriginal(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.useTemp()
	response := h.get(t, recordingHLSVideoPlaylistPath())
	if _, endlist := playlistRows(response.Body.String()); response.Code != http.StatusOK || endlist {
		t.Fatalf("temp playlist = %d %s, want no ENDLIST", response.Code, response.Body.String())
	}
	h.useOriginal()
	response = h.get(t, recordingHLSVideoPlaylistPath())
	if _, endlist := playlistRows(response.Body.String()); response.Code != http.StatusOK || !endlist {
		t.Fatalf("original playlist = %d %s, want ENDLIST", response.Code, response.Body.String())
	}
}

// DB に問い合わせられない間も、既に付けた ENDLIST を外さず、載せた行を減らさない。
//
// 壊し方: recordingHLSMetadataCache.getAny の「原本を優先する」分岐を消す。
// 壊し方: RecordingHLS.request の DB エラーの分岐で cacheKey.kind が temp のとき timing.videoDurationPTS を 0 にする。
func TestRecordingHLSDatabaseOutageKeepsEndlistAndPublishedRows(t *testing.T) {
	t.Run("temp の行を減らさない", func(t *testing.T) {
		h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(20)})
		h.useTemp()
		before := h.get(t, recordingHLSVideoPlaylistPath())
		beforeRows, _ := playlistRows(before.Body.String())
		if before.Code != http.StatusOK || len(beforeRows) != 5 {
			t.Fatalf("playlist before the outage = %d, %d rows, want 5", before.Code, len(beforeRows))
		}
		h.store.setError(errors.New("database offline"))
		// キャッシュにセグメントがある世代だけが、DB なしで配られる。
		h.seedSegment(t, SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}, "cached")
		after := h.get(t, recordingHLSVideoPlaylistPath())
		afterRows, endlist := playlistRows(after.Body.String())
		if after.Code != http.StatusOK || endlist || strings.Join(afterRows, "\n") != strings.Join(beforeRows, "\n") {
			t.Fatalf("playlist during the outage = %d %s, want the 5 rows without ENDLIST", after.Code, after.Body.String())
		}
	})
	t.Run("付けた ENDLIST を外さない", func(t *testing.T) {
		h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
		h.useOriginal()
		committed := h.get(t, recordingHLSVideoPlaylistPath())
		if _, endlist := playlistRows(committed.Body.String()); !endlist {
			t.Fatalf("committed playlist = %s, want ENDLIST", committed.Body.String())
		}
		// commit をまたいだ temp の観測が遅れて終わり、temp の metadata が LRU の先頭に来た状態。
		h.h.meta.put(recordingHLSKey{recordingID: 42, generation: 7, kind: store.RecordingHLSInputTemp}, recordingHLSMetadata{
			startedAt: *h.store.target.StartedAt,
			timing:    recordingHLSTiming{originPTS: 900000, audioStreamCount: 1},
		})
		h.seedSegment(t, SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}, "cached")
		h.store.setError(errors.New("database offline"))
		during := h.get(t, recordingHLSVideoPlaylistPath())
		rows, endlist := playlistRows(during.Body.String())
		if during.Code != http.StatusOK || !endlist || len(rows) != 2 {
			t.Fatalf("playlist during the outage = %d %s, want the committed 2 rows with ENDLIST", during.Code, during.Body.String())
		}
	})
}

// 同じ録画・世代の観測は 1 本にまとめる。temp のサイズが前回と同じなら測り直さない。
//
// 壊し方: metadataForTarget の h.observe.Do を直接 h.observeInput の呼び出しにする。
// 壊し方: observeInput の `cached.observedSize == info.Size()` の早期 return を消す。
func TestRecordingHLSTempObservationIsSharedAndSkippedForUnchangedSize(t *testing.T) {
	probe := recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(20), tailDelay: "0.3"}
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, 5*time.Second, "", probe)
	h.useTemp()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if response := h.get(t, recordingHLSVideoPlaylistPath()); response.Code != http.StatusOK {
				t.Errorf("playlist status = %d: %s", response.Code, response.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := h.tailCalls(t); got != 1 {
		t.Fatalf("tail measurements for 8 concurrent requests = %d, want 1", got)
	}
	h.get(t, recordingHLSVideoPlaylistPath())
	if got := h.tailCalls(t); got != 1 {
		t.Fatalf("tail measurements after a request at the same size = %d, want to reuse the earlier value", got)
	}
	h.growTemp(t)
	h.get(t, recordingHLSVideoPlaylistPath())
	if got := h.tailCalls(t); got != 2 {
		t.Fatalf("tail measurements after the temp grew = %d, want 2", got)
	}
}

// 末尾の窓に映像パケットが 1 つも無いときは、前回の値を使う。最初なら行は載らない。
//
// 壊し方: observeInput の errRecordingHLSNoVideoPacket の分岐を、エラーを返すようにする。
func TestRecordingHLSTempWithoutVideoPacketInTheWindowKeepsPreviousValue(t *testing.T) {
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", recordingHLSTestProbe{firstPacketPTS: 900000, tail: ""})
	h.useTemp()
	rowsNow := func(step string) int {
		t.Helper()
		response := h.get(t, recordingHLSVideoPlaylistPath())
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", step, response.Code, response.Body.String())
		}
		rows, _ := playlistRows(response.Body.String())
		return len(rows)
	}
	if got := rowsNow("映像が無い"); got != 0 {
		t.Fatalf("rows without any video packet = %d, want 0", got)
	}
	h.setTail(t, recordingHLSTailPackets(16))
	h.growTemp(t)
	if got := rowsNow("映像が届く"); got != 3 {
		t.Fatalf("rows after video arrives = %d, want 3", got)
	}
	h.setTail(t, "pts=N/A|duration=N/A|\n")
	h.growTemp(t)
	if got := rowsNow("窓に映像が無い"); got != 3 {
		t.Fatalf("rows after a window without video = %d, want the previous 3", got)
	}
}

// PTS の巻き戻り（2^33 tick）をまたぐと、末尾の PTS は原点より小さくなる。
// 長さは 2^33 を法とした原点との差で測り、負の値や飛び値にしない。
//
// 壊し方: recordingHLSVideoDurationFromPackets の `%recordingHLSPTSModulus` を消す。
func TestRecordingHLSTempPlaylistAcrossPTSWrap(t *testing.T) {
	// 原点は巻き戻りの 3 秒前。末尾は巻き戻りの後の 5 秒なので、映像の長さは 8 秒（4 本）。
	origin := recordingHLSPTSModulus - 3*mpegTSTimestampClock
	probe := recordingHLSTestProbe{firstPacketPTS: origin, tail: fmt.Sprintf("pts=%d|duration=3600|\n", 5*mpegTSTimestampClock-3600)}
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", probe)
	h.useTemp()
	response := h.get(t, recordingHLSVideoPlaylistPath())
	rows, endlist := playlistRows(response.Body.String())
	if response.Code != http.StatusOK || endlist || len(rows) != 4 {
		t.Fatalf("playlist across the PTS wrap = %d, %d rows, want 4: %s", response.Code, len(rows), response.Body.String())
	}
	h.useOriginal()
	response = h.get(t, recordingHLSVideoPlaylistPath())
	finalRows, endlist := playlistRows(response.Body.String())
	if response.Code != http.StatusOK || !endlist || len(finalRows) != 4 {
		t.Fatalf("original across the PTS wrap = %d, %d rows, want 4 with ENDLIST: %s", response.Code, len(finalRows), response.Body.String())
	}
}

func TestRecordingHLSVideoDurationFromPackets(t *testing.T) {
	const second = mpegTSTimestampClock
	nearWrap := recordingHLSPTSModulus - 3*second
	for _, tc := range []struct {
		name   string
		output string
		origin int64
		want   int64
		none   bool
	}{
		{name: "最大の pts + duration から原点を引く", output: "pts=100000|duration=3000|\npts=106000|duration=3000|\npts=103000|duration=3000|\n", origin: 90000, want: 19000},
		{name: "duration が N/A なら最大 2 つの pts の差を使う", output: "pts=100000|duration=N/A|\npts=106000|duration=N/A|\npts=103000|duration=N/A|\n", origin: 90000, want: 19000},
		{name: "パケットが 1 つで duration も無ければ 0", output: "pts=100000|duration=N/A|\n", origin: 90000, want: 10000},
		{name: "pts の無いパケットは読み飛ばす", output: "pts=N/A|duration=3000|\npts=100000|duration=3000|\n", origin: 90000, want: 13000},
		{name: "巻き戻りの後ろだけの窓", output: "pts=450000|duration=3600|\n", origin: nearWrap, want: 3*second + 450000 + 3600},
		{name: "窓が巻き戻りをまたぐ", output: fmt.Sprintf("pts=%d|duration=3600|\npts=90000|duration=3600|\n", recordingHLSPTSModulus-3600), origin: nearWrap, want: 3*second + 90000 + 3600},
		{name: "原点より少し前の pts（並べ替え）は負として扱う", output: "pts=89000|duration=1000|\n", origin: 90000, want: 0, none: true},
		{name: "原点より少し前の pts があっても最大は後ろ", output: "pts=89000|duration=1000|\npts=100000|duration=3000|\n", origin: 90000, want: 13000},
		{name: "映像パケットが無い", output: "", origin: 90000, none: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := recordingHLSVideoDurationFromPackets(tc.output, tc.origin)
			if ok == tc.none || (ok && got != tc.want) {
				t.Fatalf("duration = (%d, %v), want (%d, found %v)", got, ok, tc.want, !tc.none)
			}
		})
	}
}

// 取り消し・失敗した録画は配信を保証しない。ingest が temp と進捗行を消すと、行が引けなくなり、
// playlist もセグメントも 404 になり、走っていた変換も止まる。
//
// 壊し方: RecordingHLS.request の store.ErrNotFound の分岐で 404 ではなく 503 を返す。
func TestRecordingHLSCanceledIngestReturns404AndStopsTransform(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process")
	}
	ffmpeg := filepath.Join(t.TempDir(), "sleeping-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newRecordingHLSTestHarness(t, false, false, 50*time.Millisecond, ffmpeg)
	h.useTemp()
	if response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts")); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("segment of a growing temp = %d, want 503 while the transform runs", response.Code)
	}
	h.store.setError(store.ErrNotFound)
	for _, suffix := range []string{"video/index.m3u8", "master.m3u8", "video/0.ts"} {
		if response := h.get(t, recordingHLSRequestPath("main", 42, 7, suffix)); response.Code != http.StatusNotFound {
			t.Errorf("%s after the progress row is gone = %d, want 404", suffix, response.Code)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.h.manager.HasRecording("42", "7") {
		if time.Now().After(deadline) {
			t.Fatal("the transform of the canceled recording is still running 5 seconds after the 404")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// temp は追従（-follow 1）で読み、長さを未確定として渡す。原本は追従せず、音声を映像の終端までで切る。
// 入力エラーで変換が終わっても、temp の playlist に ENDLIST は付かない。
//
// 壊し方: RecordingHLS.Segment で temp にも VideoDurationPTS を渡す。
// 壊し方: RecordingHLS.Segment で原本にも -follow 1 を渡す。
func TestRecordingHLSTempFollowsAndOriginalHasKnownEnd(t *testing.T) {
	for _, kind := range []store.RecordingHLSInputKind{store.RecordingHLSInputTemp, store.RecordingHLSInputOriginal} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			argsPath := filepath.Join(t.TempDir(), "ffmpeg-args")
			ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 7\n", argsPath)
			if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, ffmpeg, recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(20)})
			if kind == store.RecordingHLSInputTemp {
				h.useTemp()
			}
			response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
			wantStatus := http.StatusInternalServerError
			if kind == store.RecordingHLSInputTemp {
				wantStatus = http.StatusServiceUnavailable
			}
			if response.Code != wantStatus {
				t.Fatalf("segment response = %d %s, want %d", response.Code, response.Body.String(), wantStatus)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("reading ffmpeg arguments: %v", err)
			}
			isTemp := kind == store.RecordingHLSInputTemp
			for _, option := range []string{"-follow\n1\n", "-rw_timeout\n90000000\n", "-skip_estimate_duration_from_pts\n1\n"} {
				if got := strings.Contains(string(args), option); got != isTemp {
					t.Errorf("ffmpeg args contain %q = %v, want %v: %s", option, got, isTemp, args)
				}
			}
			if got := strings.Contains(string(args), "apad,atrim=end=10.000000000"); got == isTemp {
				t.Errorf("ffmpeg args contain the audio end trim = %v, want %v (10 seconds of video): %s", got, !isTemp, args)
			}
			if isTemp {
				playlist := h.get(t, recordingHLSVideoPlaylistPath())
				if _, endlist := playlistRows(playlist.Body.String()); playlist.Code != http.StatusOK || endlist {
					t.Fatalf("playlist after the transform failed = %d %s, want no ENDLIST", playlist.Code, playlist.Body.String())
				}
			}
		})
	}
}
