//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/programid"
)

// TestBroadcastPathologies は録画を 1 件ずつ別コンテナで実行する。
// TestConformance は 1 録画の状態遷移をサブテスト間で共有しているため、ここへ混ぜると
// ケース間の順序依存が入り、放送病態そのものの判定にならない。
func TestBroadcastPathologies(t *testing.T) {
	dir := testDir(t)
	tunerBin := buildFixtureTuner(t, dir)

	cases := []struct {
		name string
		mode string
	}{
		// mirakc 4.0.0-dev.0 相当の実測: record の program は target だが、未定尺の前番組を
		// 含むストリームが target の startAt より先に録画される（実測 lead≈7.0s）。
		{"PrecedingProgramExtension", fixture.CasePrecedingExtension},
		// running_status=2 の target を present に載せたケース。mirakc の録画パイプライン
		// 開始と target event のフィルタ結果は別の時刻で、recording.startTime は startAt
		// より前になる（実測 lead≈11.6s）。
		{"RunningStatusStartsSoon", fixture.CaseRunningStatus},
		// present が前番組、following が target のケース。録画パイプラインは
		// target.startAt より先に始まる（実測 lead≈11.7〜11.9s）。
		{"FollowingProgram", fixture.CaseFollowing},
		// target の event_id が同じ時間帯で振り直されるケース。時刻の先行ではなく record
		// の状態遷移（recording.failed の有無）を判定するので下で別処理にする。
		{"EventIDReset", fixture.CaseEventIDReset},
	}

	for _, tc := range cases {
		mode := tc.mode
		t.Run(tc.name, func(t *testing.T) {
			// ケースごとに別コンテナにする。録画のチューナー占有と EPG 状態を共有すると、
			// 前ケースの schedule / record が次の判定を汚染する。
			caseDir := testDir(t)
			container := startMirakc(t, caseDir, tunerBin, mode)
			client := mirakc.NewClient(container.baseURL, nil)
			ctx := context.Background()
			serviceID := programid.ServiceID(fixture.NetworkID, fixture.ServiceID)
			programID := programid.ComposeProgramID(fixture.NetworkID, fixture.ServiceID, fixture.EventID)

			waitForService(t, ctx, client, serviceID)

			var events chan mirakc.Event
			if mode == fixture.CaseEventIDReset {
				events = make(chan mirakc.Event, 128)
				sseCtx, stopSSE := context.WithCancel(ctx)
				defer stopSSE()
				go func() { _ = client.Subscribe(sseCtx, events, nil) }()
			}

			program, created := scheduleFreshPathologyProgram(t, ctx, client, programID)

			if mode == fixture.CaseEventIDReset {
				judgeEventIDResetWithEvents(t, ctx, client, program, events)
				return
			}

			record := waitForPathologyRecord(t, ctx, client, programID)
			assertRecordStartsEarly(t, record, created.Program.StartAt.Time(), 2*time.Second, 20*time.Second)
		})
	}
}

// waitForProgramAt は programID の program のうち、startAt がポーリング時点の現在時刻より
// leadIn 以上先にあるものが現れるまで待つ。「leadIn 以上先」は呼び出し時ではなく毎回の
// ポーリング時点の現在時刻で判定する（呼び出し時に 1 回だけ計算すると、EPG bootstrap の
// 待ち時間のぶんだけ判定が古びてガードにならない）。
//
// deadline は呼び出し側が持つ予算をそのまま受け取る。自前の予算を持つと、リトライする
// 呼び出し側（scheduleFreshPathologyProgram）の予算判定が「1 回の待ちが終わった後」に
// しか効かず、実際の上限が呼び出し側の予算の 2 倍近くまで伸びてしまう。
func waitForProgramAt(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64, leadIn time.Duration, deadline time.Time) mirakc.Program {
	t.Helper()
	for time.Now().Before(deadline) {
		minStart := time.Now().Add(leadIn)
		programs, err := client.ListPrograms(ctx)
		if err == nil {
			for _, p := range programs {
				if p.ID != programID || p.StartAt == nil || p.StartAt.Time().Before(minStart) {
					continue
				}
				return p
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("program id=%d が期限 %s までに startAt がその時点の現在時刻より %s 以上先の状態で現れなかった", programID, deadline.Format(time.RFC3339), leadIn)
	return mirakc.Program{}
}

// scheduleFreshPathologyProgram は waitForProgramAt で受理した program に対して
// CreateSchedule を呼び、mirakc 自身がその CreateSchedule 応答で返す program.startAt が
// waitForProgramAt が見た値と一致することを確認してから両方を返す。
//
// フィクスチャの病態イベントは 30 秒周期（fixture.activeCaseEventStart）で繰り返すため、
// waitForProgramAt が「leadIn 以上先」と判定した直後でも、CreateSchedule の往復や mirakc
// 内部の再同期の間にその周期が次に進むと、mirakc 側が同じ programID を次の周期の
// startAt で解決し直すことがありうる。これが起きた場合、後続の判定基準にするべき
// startAt は「waitForProgramAt が見た値」でも「record が現れた時点で record.Program に
// 載っている値」でもなく、両者が一致しなくなった時点で測定が壊れている。
//
// leadIn=5s の根拠: 実測（内部診断、コミットしていない）では、mirakc が次周期の
// program を ListPrograms に反映し始めた直後の lead はおよそ 11〜12 秒どまりで、
// pathologyDuration の 15 秒に届かない（mirakc 側の再同期に数秒の遅れがある）。leadIn を
// この上限に近づけるほど「受理できる窓」が数秒未満まで狭まり、受理そのものが
// 度々失敗するようになる（実測で leadIn=10s は 100 秒のポーリング予算内で一度も受理でき
// ないことがあった）。leadIn=5s は CreateSchedule の往復（実測 <1s）や mirakc の
// 3 秒周期の再同期に対して十分な余裕を残しつつ、受理の窓自体を数秒〜十秒弱に保つ。
// この余裕はロールオーバーをめったに起こさない効果はあるが、それ自体は正しさの根拠では
// ない（CI 環境の遅延次第でこの余裕を使い切ることはありうる）ので、実際に起きたことを
// 以下で検出し、起きていれば新しい program を取り直して測り直す。
func scheduleFreshPathologyProgram(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64) (mirakc.Program, *mirakc.Schedule) {
	t.Helper()
	const leadIn = 5 * time.Second
	const budget = epgBootstrapTimeout + 60*time.Second
	retryDeadline := time.Now().Add(budget)
	for {
		program := waitForProgramAt(t, ctx, client, programID, leadIn, retryDeadline)
		created, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
			ProgramID: programID,
			Options:   mirakc.Options{Priority: 1},
		})
		if err != nil {
			t.Fatalf("CreateSchedule(programId=%d): %v", programID, err)
		}
		if program.StartAt != nil && created.Program.StartAt != nil &&
			created.Program.StartAt.Time().Equal(program.StartAt.Time()) {
			return program, created
		}
		gotStart := "nil"
		if created.Program.StartAt != nil {
			gotStart = created.Program.StartAt.Time().String()
		}
		wantStart := "nil"
		if program.StartAt != nil {
			wantStart = program.StartAt.Time().String()
		}
		t.Logf("schedule 作成までに周期のロールオーバーを検出した（waitForProgramAt の "+
			"startAt=%s、CreateSchedule 応答の startAt=%s）。測り直す", wantStart, gotStart)
		if !time.Now().Before(retryDeadline) {
			t.Fatalf("周期のロールオーバーが解消しないまま %s のリトライ予算を使い切った", budget)
		}
		if err := client.DeleteSchedule(ctx, programID); err != nil {
			t.Fatalf("DeleteSchedule(programId=%d)（測り直しのため）: %v", programID, err)
		}
	}
}

func waitForPathologyRecord(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64) mirakc.Record {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		records, err := client.ListRecords(ctx)
		if err == nil {
			for _, r := range records {
				if r.Program.ID == programID {
					return r
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("programId=%d の record が 45 秒以内に現れなかった", programID)
	return mirakc.Record{}
}

// assertRecordStartsEarly は record.Recording.StartTime が want（scheduleFreshPathologyProgram
// が CreateSchedule 応答から確認した target の startAt）より minLead〜maxLead だけ早い
// ことを固定する。基準を record 自身の Program.StartAt ではなく呼び出し側が確認済みの
// want に取るのは、record が現れるまでの間（最大 45 秒）に mirakc 側の EPG 解決が
// フィクスチャの次周期にロールオーバーしていた場合、record.Program.StartAt がその
// ロールオーバー後の値になっていて基準として使えないため（scheduleFreshPathologyProgram
// のコメント参照）。
func assertRecordStartsEarly(t *testing.T, r mirakc.Record, want time.Time, minLead, maxLead time.Duration) {
	t.Helper()
	if r.Program.EventID != fixture.EventID {
		t.Fatalf("record の eventId=%d、予約対象の eventId=%d ではない", r.Program.EventID, fixture.EventID)
	}
	start := r.Recording.StartTime.Time()
	lead := want.Sub(start)
	t.Logf("record eventId=%d, recording.startTime=%s, schedule 確認済み startAt=%s, 先行=%s", r.Program.EventID, start, want, lead)
	if lead < minLead || lead > maxLead {
		t.Fatalf("録画開始先行=%s、%s〜%s の範囲で早期に始まるはず（startAt=%s 基準）", lead, minLead, maxLead, want)
	}
}

// judgeEventIDResetWithEvents は「target の event_id が同じ時間帯で振り直される」ケースの
// mirakc 4.0.0-dev.0 相当の実測を固定する。実測（docs/recording/delegation.md）は
// recording.failed を一切出さず、予約対象 event_id の record が recording→finished の
// まま残る、というものだった。recording.failed はこの固定した挙動からの逸脱なので、
// SSE 経由・ポーリング経由のどちらで観測しても落とす（両方を pass にしない）。
func judgeEventIDResetWithEvents(t *testing.T, ctx context.Context, client *mirakc.Client, program mirakc.Program, events <-chan mirakc.Event) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event := <-events:
			if event.Type != "recording.failed" {
				continue
			}
			var failed mirakc.RecordingFailedData
			if err := json.Unmarshal(event.Data, &failed); err != nil || failed.ProgramID != program.ID {
				continue
			}
			t.Fatalf("event_id 振り直し後に recording.failed(reason=%s) を SSE で観測した。"+
				"固定した挙動（recording.failed を出さず record が recording/finished のまま"+
				"残る。docs/recording/delegation.md）からの逸脱", failed.Reason.Type)
		case <-time.After(300 * time.Millisecond):
			if replacement, ok := findReplacementRecord(t, ctx, client); ok {
				t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が作られた", replacement.Program.EventID)
			}
			record, ok := findRecord(t, ctx, client, program.ID)
			if !ok {
				continue
			}
			t.Logf("event_id 振り直し: record eventId=%d status=%s", record.Program.EventID, record.Recording.Status)
			if record.Program.EventID != fixture.EventID {
				t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が作られた", record.Program.EventID)
			}
			switch record.Recording.Status {
			case "failed":
				reason := "unknown"
				if record.Recording.FailedReason != nil {
					reason = record.Recording.FailedReason.Type
				}
				t.Fatalf("event_id 振り直し後に record が failed(reason=%s) になった。固定した挙動"+
					"（recording.failed を出さず recording/finished のまま残る）からの逸脱", reason)
			case "recording", "finished":
				final := waitForFinishedWithoutReplacement(t, ctx, client, record)
				t.Logf("event_id 振り直し: target event_id の録画が status=%s で残った（recording.failed なし）", final.Recording.Status)
				return
			}
		}
	}
	t.Fatalf("event_id 振り直し後、programId=%d の record が 45 秒以内に recording/finished の"+
		"まま（recording.failed なしで）残ることを確認できなかった。record 自体が現れなかったか、"+
		"それ以外の status に留まった可能性がある", program.ID)
}

// waitForFinishedWithoutReplacement は record が status=="finished" になるまで待ち、
// その間ずっと ---さらに finished になった後も settle 分の猶予を置いて--- 置換 event_id の
// record が現れないことを確認し続ける。
//
// 置換放送（fixture.ReplacementEventID）は target の録画がちょうど完了するまでに要する
// 期間（pathologyDuration）だけ on-air になっている。mirakc の update-schedules
// （このテスト設定では 3 秒間隔）はその期間中いつでも置換放送に反応しうるので、
// 「finished を確認した直後に一度だけ・数秒だけ」置換 record の有無を見る形だと、
// 途中や settle 前後で作られた置換 record を見逃しうる。そのため待機の全区間で
// 継続的に確認する。
func waitForFinishedWithoutReplacement(t *testing.T, ctx context.Context, client *mirakc.Client, record mirakc.Record) mirakc.Record {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var settleUntil time.Time
	for {
		if replacement, ok := findReplacementRecord(t, ctx, client); ok {
			t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が作られた", replacement.Program.EventID)
		}
		if record.Recording.Status == "finished" {
			if settleUntil.IsZero() {
				// mirakc の update-schedules が数ティック遅れて置換放送に反応する場合に
				// 備え、finished 確認後もしばらく監視を続ける。
				settleUntil = time.Now().Add(10 * time.Second)
			}
			if time.Now().After(settleUntil) {
				return record
			}
		} else if !time.Now().Before(deadline) {
			t.Fatalf("record %s が 45 秒以内に status=finished に到達しなかった（最後に見えた status=%q）", record.ID, record.Recording.Status)
		}
		time.Sleep(500 * time.Millisecond)
		final, err := client.GetRecord(ctx, record.ID)
		if err != nil {
			t.Fatalf("GetRecord: %v", err)
		}
		record = *final
	}
}

func findRecord(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64) (mirakc.Record, bool) {
	t.Helper()
	records, err := client.ListRecords(ctx)
	if err != nil {
		t.Logf("ListRecords: %v", err)
		return mirakc.Record{}, false
	}
	for _, record := range records {
		if record.Program.ID == programID {
			return record, true
		}
	}
	return mirakc.Record{}, false
}

func findReplacementRecord(t *testing.T, ctx context.Context, client *mirakc.Client) (mirakc.Record, bool) {
	t.Helper()
	records, err := client.ListRecords(ctx)
	if err != nil {
		t.Logf("ListRecords: %v", err)
		return mirakc.Record{}, false
	}
	for _, record := range records {
		if record.Program.EventID == fixture.ReplacementEventID && record.Program.ServiceID == fixture.ServiceID && record.Program.NetworkID == fixture.NetworkID {
			return record, true
		}
	}
	return mirakc.Record{}, false
}
