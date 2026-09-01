//go:build conformance

// Package conformance は rokuban が mirakc に期待している挙動を、実物の mirakc コンテナに
// 対して internal/mirakc.Client 経由で機械判定する。生の HTTP は書かない（判定対象そのものが
// Client の契約なので、Client を経由しない判定は何も検査したことにならない）。
//
// `go test ./...`（タグなし）では一切ビルドされない。`go test -tags conformance ./...` で
// 実行する。Docker が必要（helpers_test.go の mirakcImage を起動する）。
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
)

const (
	epgBootstrapTimeout   = 60 * time.Second
	recordingStartTimeout = 20 * time.Second
	// フィクスチャの番組は fixture.EventDuration（+ mirakc 既定の --end-margin=2000ms）で
	// 自然終了する。EPG 収集にかかる時間とは独立（fixture.NewConfig のコメント参照）なので、
	// この値は fixture.EventDuration に対してだけ余裕を持たせればよい。
	recordingFinishTimeout = fixture.EventDuration + 60*time.Second
)

// TestConformance は 1 つの mirakc コンテナと 1 本の録画のライフサイクルを、項目 1〜5 の
// 全サブテストで使い回す。録画の状態遷移（scheduled → recording → finished）に沿った
// 順序依存があるため、意図的に 1 本の関数にまとめている。
func TestConformance(t *testing.T) {
	dir := testDir(t)
	tunerBin := buildFixtureTuner(t, dir)
	container := startMirakc(t, dir, tunerBin)
	client := mirakc.NewClient(container.baseURL, nil)
	ctx := context.Background()

	serviceID := mirakc.ServiceID(fixture.NetworkID, fixture.ServiceID)
	programID := mirakc.ComposeProgramID(fixture.NetworkID, fixture.ServiceID, fixture.EventID)

	// 受け入れ項目 5: pin の上げ忘れで別物を判定していないことの検査。
	t.Run("MirakcVersionPin", func(t *testing.T) {
		v, err := client.GetVersion(ctx)
		if err != nil {
			t.Fatalf("GetVersion: %v", err)
		}
		if v.Current != mirakcVersion {
			t.Fatalf("mirakc の版 = %q, テストの pin（helpers_test.go の mirakcVersion）は %q。"+
				"コンテナの版とテストの pin がずれている", v.Current, mirakcVersion)
		}
	})

	waitForService(t, ctx, client, serviceID)
	waitForProgram(t, ctx, client, programID)

	// SSE は録画開始より前に張る。接続時の既存 record 再送（項目 2 前半、後段の
	// RecordSavedResentOnConnect で改めて検査する）とは別に、この接続で「録画中に
	// 同一 record の record-saved が複数回来る」（項目 2 後半）を観測する。
	events := make(chan mirakc.Event, 256)
	sseCtx, stopSSE := context.WithCancel(ctx)
	defer stopSSE()
	go func() { _ = client.Subscribe(sseCtx, events, nil) }()

	const contentPath = "conformance/schedule-content-path.ts"
	created, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: programID,
		Options:   mirakc.Options{ContentPath: strPtr(contentPath), Priority: 1},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// 受け入れ項目 1 / reconciler.md の「未検証」を閉じる: mirakc が options.contentPath を
	// そのまま返すこと（正規化しない）を CreateSchedule の応答・GetSchedule・ListSchedules の
	// 3 箇所で確認する。
	t.Run("ContentPathRoundTrip", func(t *testing.T) {
		requireContentPath(t, "CreateSchedule", created.Options.ContentPath, contentPath)

		got, err := client.GetSchedule(ctx, programID)
		if err != nil {
			t.Fatalf("GetSchedule: %v", err)
		}
		requireContentPath(t, "GetSchedule", got.Options.ContentPath, contentPath)

		list, err := client.ListSchedules(ctx)
		if err != nil {
			t.Fatalf("ListSchedules: %v", err)
		}
		found := false
		for _, s := range list {
			if s.Program.ID == programID {
				found = true
				requireContentPath(t, "ListSchedules", s.Options.ContentPath, contentPath)
			}
		}
		if !found {
			t.Fatalf("ListSchedules に programId=%d が含まれない", programID)
		}
	})

	recordID := waitForRecord(t, ctx, client, programID)
	waitForRecordingStatus(t, ctx, client, recordID, "recording", recordingStartTimeout)

	// 受け入れ項目 3-4「録画中」: content が定まらない間は Content-Length を返さない。
	t.Run("RecordingInProgress", func(t *testing.T) {
		rec, err := client.GetRecord(ctx, recordID)
		if err != nil {
			t.Fatalf("GetRecord: %v", err)
		}
		if rec.Recording.Status != "recording" {
			// EventDuration は 30 秒あり、この時点はまだその序盤のはず。この状態のまま
			// 「finished だった」を通すと、この項目が何も検査しないまま緑になる
			// （フィクスチャが壊れて録画が一瞬で終わった、等）。
			t.Fatalf("この時点で録画中のはずが status=%s だった。フィクスチャが壊れている疑いがある", rec.Recording.Status)
		}

		// 受け入れ項目 4 の前半（divergence a）: `GetRecord` の `content.length` は
		// 録画中も非 nil で、時間とともに増える（`docs/recording/ingest.md` §5.6 の
		// 進捗分母 `record_sync.content_length` がこれに依存する）。
		if rec.Content.Length == nil {
			t.Fatalf("録画中なのに GetRecord の Content.Length が nil。ingest の進捗分母として非 nil のはず（docs/recording/ingest.md §5.6）")
		}
		firstLen := *rec.Content.Length
		time.Sleep(2 * time.Second)
		if rec2, err := client.GetRecord(ctx, recordID); err == nil && rec2.Content.Length != nil {
			if *rec2.Content.Length <= firstLen {
				t.Errorf("録画中に Content.Length が増えていない: %d -> %d", firstLen, *rec2.Content.Length)
			}
		}

		// 罠: HEAD は録画完了後にしか打たないのが普通だが（`checkRecordStream`）、ここでは
		// あえて録画中に叩いて「長さを返さない」ことそのものを確定する。
		if headLen, err := client.HeadRecordStream(ctx, recordID); err != nil {
			t.Errorf("HeadRecordStream（録画中）: %v", err)
		} else if headLen >= 0 {
			t.Errorf("録画中の HeadRecordStream の Content-Length = %d、録画中は不明（負値）のはず", headLen)
		}

		// 罠（`GetRecord` の `content.length` とは別物）: `records/{id}/stream` への
		// **Range なし** の GET は録画中は Content-Length ヘッダを返さない
		// （`transfer-encoding: chunked` になる。実 mirakc で確認した --- `compute_content_length`
		// が `None` を返す）。content がまだ 0 バイトの間は 204 になりうる（これも実 mirakc で
		// 確認した挙動）ので、実際にバイトが流れ始めるまで軽くリトライしてから判定する。
		// Go の http.Response.ContentLength は Content-Length ヘッダの有無だけで決まる
		// （無ければ -1）ので、Client の戻り値だけでこの前提を判定できる（生のヘッダを読まなくてよい）。
		length, err := streamContentLengthDuringRecording(t, ctx, client, recordID, 0)
		if err != nil {
			t.Errorf("StreamRecord(offset=0)（録画中）: %v", err)
		} else if length >= 0 {
			t.Errorf("録画中の StreamRecord(offset=0) の Content-Length = %d、録画中は不明（負値）のはず", length)
		}

		// Range 付き（offset>0）は録画中でも 206 で返り、**Content-Length ヘッダ自体は
		// 付く**（値はオフセットから今バッファに溜まっている末尾まで。実 mirakc で確認した:
		// `content-range: bytes 1-114687/*` のように total は `*` でも
		// `content-length: 114687` は具体値）。issue の「Range 付きでも Content-Range の
		// 総サイズが無い」は Content-Range の total フィールドの話であって、
		// Content-Length が不明になるわけではない --- 上の offset=0 の「不明」と混同しない。
		// 206 を返さない（= Range が届いていない）と Client 内の checkStatus(206) が
		// 失敗するので、StreamRecord の Range ヘッダを落とす変異はここでも検出できる。
		rangedLength, err := streamContentLengthDuringRecording(t, ctx, client, recordID, 1)
		if err != nil {
			t.Errorf("StreamRecord(offset=1)（録画中）: %v（Range が届いていない）", err)
		} else if rangedLength < 0 {
			t.Errorf("録画中の StreamRecord(offset=1) の Content-Length = %d、Range 付きなら定まるはず", rangedLength)
		}
	})

	waitForRecordingStatus(t, ctx, client, recordID, "finished", recordingFinishTimeout)

	// 受け入れ項目 2 後半: 同一 record の record-saved が録画中に複数回来る。
	// SSE は録画開始前から張ってあるので、ここまでに届いたぶんを数える。
	t.Run("RecordSavedFiresMultipleTimes", func(t *testing.T) {
		saved := drainFor(events, 2*time.Second)
		count := countRecordSaved(saved, recordID)
		// この件数はライフサイクル全体（作成〜finished）での合計であって「録画中」に
		// 限っていない。`Subscribe` は切断時に自動再接続する（sse.go）ので、途中で
		// 再接続が起きれば接続時の再送 1 件が混ざりうる --- しきい値ちょうどで通ったときは
		// この可能性を疑う。実測件数は毎回ログしておく。
		t.Logf("record-saved(id=%s) の観測件数 = %d", recordID, count)
		if count < 2 {
			t.Errorf("録画のライフサイクル中に届いた record-saved(id=%s) は %d 件。"+
				"複数回来る前提（docs/recording/watcher.md §3.3 (a)）が崩れている", recordID, count)
		}
	})

	// 受け入れ項目 2 前半: /events に接続すると既存 record の record-saved が再送される。
	// DeleteRecord（次のサブテスト）より前に確認する必要がある --- 削除後は再送する record が
	// 無くなる。
	t.Run("RecordSavedResentOnConnect", func(t *testing.T) {
		reconnectEvents := make(chan mirakc.Event, 64)
		reconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		go func() { _ = client.Subscribe(reconnectCtx, reconnectEvents, nil) }()
		received := drainFor(reconnectEvents, 4*time.Second)
		if countRecordSaved(received, recordID) < 1 {
			t.Errorf("接続直後に record-saved(id=%s) が再送されなかった（docs/recording/watcher.md §3.3 (b)）", recordID)
		}
	})

	// 受け入れ項目 3「完了後」: HEAD の Content-Length・Range 再開・DeleteRecord(purge=true)
	// による content 削除。
	t.Run("CompletedRecordStreamAndDelete", func(t *testing.T) {
		rec, err := client.GetRecord(ctx, recordID)
		if err != nil {
			t.Fatalf("GetRecord: %v", err)
		}
		if rec.Content.Length == nil || *rec.Content.Length == 0 {
			t.Fatalf("完了後の Content.Length = %v、非ゼロのはず", rec.Content.Length)
		}
		wantLen := int64(*rec.Content.Length)

		// 変異「HeadRecordStream の返り値を固定する」はここで落ちる: 固定値は
		// GetRecord が返す実際の長さと一致しない。
		headLen, err := client.HeadRecordStream(ctx, recordID)
		if err != nil {
			t.Fatalf("HeadRecordStream: %v", err)
		}
		if headLen != wantLen {
			t.Fatalf("HeadRecordStream の Content-Length = %d、"+
				"GetRecord の Content.Length = %d と食い違う", headLen, wantLen)
		}

		full, fullLen, err := client.StreamRecord(ctx, recordID, 0)
		if err != nil {
			t.Fatalf("StreamRecord(offset=0): %v", err)
		}
		n, err := io.Copy(io.Discard, full)
		_ = full.Close()
		if err != nil {
			t.Fatalf("reading full stream: %v", err)
		}
		if n != wantLen {
			t.Errorf("StreamRecord(offset=0) で読めたバイト数 = %d、want %d", n, wantLen)
		}
		// 完了後は録画中と違って Content-Length ヘッダが具体値になる（実 mirakc で確認した）。
		// 録画中のような「不明でもよい」猶予はない。
		if fullLen != wantLen {
			t.Errorf("StreamRecord(offset=0) の Content-Length = %d、want %d", fullLen, wantLen)
		}

		// 変異「StreamRecord の Range ヘッダを落とす」はここで落ちる: Range を送らなければ
		// mirakc は 200 で全量を返すため、Client 内の checkStatus(206) が失敗して
		// *mirakc.APIError になる。
		offset := wantLen / 2
		if offset == 0 {
			offset = 1
		}
		partial, partialLen, err := client.StreamRecord(ctx, recordID, offset)
		if err != nil {
			t.Fatalf("StreamRecord(offset=%d): %v（Range 再開ができていない）", offset, err)
		}
		pn, err := io.Copy(io.Discard, partial)
		_ = partial.Close()
		if err != nil {
			t.Fatalf("reading partial stream: %v", err)
		}
		wantPartial := wantLen - offset
		if pn != wantPartial {
			t.Errorf("StreamRecord(offset=%d) で読めたバイト数 = %d、want %d", offset, pn, wantPartial)
		}
		if partialLen != wantPartial {
			t.Errorf("StreamRecord(offset=%d) の Content-Length = %d、want %d", offset, partialLen, wantPartial)
		}

		result, err := client.DeleteRecord(ctx, recordID, true)
		if err != nil {
			t.Fatalf("DeleteRecord(purge=true): %v", err)
		}
		if !result.RecordRemoved || !result.ContentRemoved {
			t.Fatalf("DeleteRecord(purge=true) = %+v、両方 true のはず", result)
		}
	})
}

func strPtr(s string) *string { return &s }

func requireContentPath(t *testing.T, label string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s の contentPath = %v、want %q（mirakc が正規化して返している可能性がある）", label, got, want)
	}
}

func waitForService(t *testing.T, ctx context.Context, c *mirakc.Client, serviceID int64) {
	t.Helper()
	deadline := time.Now().Add(epgBootstrapTimeout)
	for time.Now().Before(deadline) {
		services, err := c.ListServices(ctx)
		if err == nil {
			for _, s := range services {
				if s.ID == serviceID {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("service id=%d が %s 以内に現れなかった（scan-services が失敗しているか、フィクスチャの SI が壊れている）",
		serviceID, epgBootstrapTimeout)
}

func waitForProgram(t *testing.T, ctx context.Context, c *mirakc.Client, programID int64) {
	t.Helper()
	deadline := time.Now().Add(epgBootstrapTimeout)
	for time.Now().Before(deadline) {
		programs, err := c.ListPrograms(ctx)
		if err == nil {
			for _, p := range programs {
				if p.ID == programID {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("program id=%d が %s 以内に現れなかった（update-schedules が失敗しているか、フィクスチャの EIT が壊れている）",
		programID, epgBootstrapTimeout)
}

func waitForRecord(t *testing.T, ctx context.Context, c *mirakc.Client, programID int64) string {
	t.Helper()
	deadline := time.Now().Add(recordingStartTimeout)
	for time.Now().Before(deadline) {
		records, err := c.ListRecords(ctx)
		if err == nil {
			for _, r := range records {
				if r.Program.ID == programID {
					return r.ID
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("programId=%d の record が %s 以内に現れなかった（mirakc がスケジュールどおり録画を開始していない）",
		programID, recordingStartTimeout)
	return ""
}

func waitForRecordingStatus(t *testing.T, ctx context.Context, c *mirakc.Client, recordID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		rec, err := c.GetRecord(ctx, recordID)
		if err == nil {
			last = rec.Recording.Status
			if last == want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("record %s が %s 以内に status=%q に到達しなかった（最後に見えた status=%q）",
		recordID, timeout, want, last)
}

// drainFor は d の間だけ ch から読み続けて集める。SSE は常時接続なので終端がなく、
// 「今この瞬間までに届いたぶん」を固定時間で区切って観測する以外に集める方法がない。
func drainFor(ch <-chan mirakc.Event, d time.Duration) []mirakc.Event {
	var out []mirakc.Event
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-timer.C:
			return out
		}
	}
}

// streamContentLengthDuringRecording は StreamRecord(recordID, offset) を、content が
// まだ 0 バイトで 204 が返る間だけ短い間隔でリトライする。204 のまま 10 秒の猶予が尽きたら
// その 204 の APIError をそのまま返す（呼び出し側が t.Errorf / t.Fatalf にするかを決める。
// ここではログもエラーの握り潰しもしない）。
func streamContentLengthDuringRecording(t *testing.T, ctx context.Context, c *mirakc.Client, recordID string, offset int64) (int64, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, length, err := c.StreamRecord(ctx, recordID, offset)
		if err == nil {
			_ = body.Close()
			return length, nil
		}
		var apiErr *mirakc.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNoContent || !time.Now().Before(deadline) {
			return 0, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func countRecordSaved(events []mirakc.Event, recordID string) int {
	count := 0
	for _, e := range events {
		if e.Type != "recording.record-saved" {
			continue
		}
		var data mirakc.RecordSavedData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			continue
		}
		if data.RecordID == recordID {
			count++
		}
	}
	return count
}
