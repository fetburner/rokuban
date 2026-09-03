//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
)

// TestBroadcastPathologies は録画を 1 件ずつ別コンテナで実行する。
// TestConformance は 1 録画の状態遷移をサブテスト間で共有しているため、ここへ混ぜると
// ケース間の順序依存が入り、放送病態そのものの判定にならない。
func TestBroadcastPathologies(t *testing.T) {
	dir := testDir(t)
	tunerBin := buildFixtureTuner(t, dir)

	cases := []struct {
		name  string
		mode  string
		judge func(*testing.T, context.Context, *mirakc.Client, mirakc.Program)
	}{
		{
			name:  "PrecedingProgramExtension",
			mode:  fixture.CasePrecedingExtension,
			judge: judgePrecedingProgramExtension,
		},
		{
			name:  "RunningStatusStartsSoon",
			mode:  fixture.CaseRunningStatus,
			judge: judgeRunningStatus,
		},
		{
			name:  "FollowingProgram",
			mode:  fixture.CaseFollowing,
			judge: judgeFollowingProgram,
		},
		{
			name: "EventIDReset",
			mode: fixture.CaseEventIDReset,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ケースごとに別コンテナにする。録画のチューナー占有と EPG 状態を共有すると、
			// 前ケースの schedule / record が次の判定を汚染する。
			caseDir := testDir(t)
			container := startMirakc(t, caseDir, tunerBin, tc.mode)
			client := mirakc.NewClient(container.baseURL, nil)
			ctx := context.Background()
			serviceID := mirakc.ServiceID(fixture.NetworkID, fixture.ServiceID)
			programID := mirakc.ComposeProgramID(fixture.NetworkID, fixture.ServiceID, int(fixture.CaseEventID(tc.mode)))

			waitForService(t, ctx, client, serviceID)
			// EPG が前周期の同じ event_id を返している場合があるので、次周期の開始時刻
			// 以降の program を待つ。作成時点で少なくとも 5 秒先にして早期録画の窓を残す。
			minStart := time.Now().Add(5 * time.Second)
			if next := fixture.NextCaseStart(time.Now(), tc.mode); next.After(minStart) {
				minStart = next
			}
			program := waitForProgramAt(t, ctx, client, programID, minStart)

			if tc.mode == fixture.CaseEventIDReset {
				events := make(chan mirakc.Event, 128)
				sseCtx, stopSSE := context.WithCancel(ctx)
				defer stopSSE()
				go func() { _ = client.Subscribe(sseCtx, events, nil) }()

				createPathologySchedule(t, ctx, client, programID)
				judgeEventIDResetWithEvents(t, ctx, client, program, events)
				return
			}

			createPathologySchedule(t, ctx, client, programID)
			tc.judge(t, ctx, client, program)
		})
	}
}

func createPathologySchedule(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64) {
	t.Helper()
	if _, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: programID,
		Options:   mirakc.Options{Priority: 1},
	}); err != nil {
		t.Fatalf("CreateSchedule(programId=%d): %v", programID, err)
	}
}

func waitForProgramAt(t *testing.T, ctx context.Context, client *mirakc.Client, programID int64, minStart time.Time) mirakc.Program {
	t.Helper()
	deadline := time.Now().Add(epgBootstrapTimeout + 40*time.Second)
	for time.Now().Before(deadline) {
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
	t.Fatalf("program id=%d が %s 以内に startAt >= %s で現れなかった", programID, epgBootstrapTimeout+40*time.Second, minStart.Format(time.RFC3339))
	return mirakc.Program{}
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

func assertRecordBoundary(t *testing.T, r mirakc.Record, program mirakc.Program, latest time.Duration) {
	t.Helper()
	if r.Program.EventID != fixture.EventID {
		t.Fatalf("record の eventId=%d、予約対象の eventId=%d ではない", r.Program.EventID, fixture.EventID)
	}
	if program.StartAt == nil {
		t.Fatal("予約対象 program の startAt が nil")
	}
	start := r.Recording.StartTime.Time()
	want := program.StartAt.Time()
	if start.Before(want.Add(-2 * time.Second)) {
		t.Fatalf("録画開始=%s、予約対象の開始=%s より早い（前番組の中身を録っている可能性）", start, want)
	}
	if start.After(want.Add(latest)) {
		t.Fatalf("録画開始=%s、予約対象の開始=%s から %s 以内に始まらない", start, want, latest)
	}
}

// mirakc 4.0.0-dev.0 相当の測定では、record の program は target だが、未定尺の前番組を
// 含むストリームが target の startAt より先に録画される。予約対象 event_id のみを見て
// 「前番組を録っていない」と判定しないため、recording.startTime の早期開始を固定する。
func judgePrecedingProgramExtension(t *testing.T, ctx context.Context, client *mirakc.Client, program mirakc.Program) {
	t.Helper()
	record := waitForPathologyRecord(t, ctx, client, program.ID)
	start := record.Recording.StartTime.Time()
	want := program.StartAt.Time()
	lead := want.Sub(start)
	t.Logf("前番組延長: record eventId=%d, recording.startTime=%s, target.startAt=%s, 先行=%s", record.Program.EventID, start, want, lead)
	if record.Program.EventID != fixture.EventID {
		t.Fatalf("record の eventId=%d、予約対象の eventId=%d ではない", record.Program.EventID, fixture.EventID)
	}
	if lead < 2*time.Second || lead > 40*time.Second {
		t.Fatalf("前番組延長の録画開始先行=%s、2〜40 秒の範囲で前番組を含むはず", lead)
	}
}

// running_status=2 の target を present に載せたときの mirakc の実測を固定する。現在の
// pin では recording.startTime は startAt より前になる（mirakc の録画パイプライン開始と、
// target event のフィルタ結果は別の時刻である）。
func judgeRunningStatus(t *testing.T, ctx context.Context, client *mirakc.Client, program mirakc.Program) {
	t.Helper()
	record := waitForPathologyRecord(t, ctx, client, program.ID)
	lead := program.StartAt.Time().Sub(record.Recording.StartTime.Time())
	t.Logf("running_status=2: record eventId=%d, recording.startTime=%s, target.startAt=%s, 先行=%s", record.Program.EventID, record.Recording.StartTime.Time(), program.StartAt.Time(), lead)
	assertRecordStartsEarly(t, record, program, 2*time.Second, 40*time.Second)
}

// present が前番組、following が target のときの mirakc の実測を固定する。record の
// program は target だが、録画パイプラインは target.startAt より先に始まる。
func judgeFollowingProgram(t *testing.T, ctx context.Context, client *mirakc.Client, program mirakc.Program) {
	t.Helper()
	record := waitForPathologyRecord(t, ctx, client, program.ID)
	lead := program.StartAt.Time().Sub(record.Recording.StartTime.Time())
	t.Logf("present=前番組 / following=target: record eventId=%d, recording.startTime=%s, target.startAt=%s, 先行=%s", record.Program.EventID, record.Recording.StartTime.Time(), program.StartAt.Time(), lead)
	assertRecordStartsEarly(t, record, program, 2*time.Second, 40*time.Second)
}

func assertRecordStartsEarly(t *testing.T, r mirakc.Record, program mirakc.Program, minLead, maxLead time.Duration) {
	t.Helper()
	if r.Program.EventID != fixture.EventID {
		t.Fatalf("record の eventId=%d、予約対象の eventId=%d ではない", r.Program.EventID, fixture.EventID)
	}
	if program.StartAt == nil {
		t.Fatal("予約対象 program の startAt が nil")
	}
	lead := program.StartAt.Time().Sub(r.Recording.StartTime.Time())
	if lead < minLead || lead > maxLead {
		t.Fatalf("録画開始先行=%s、%s〜%s の範囲で早期に始まるはず", lead, minLead, maxLead)
	}
}

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
			t.Logf("event_id 振り直し: recording.failed reason=%s", failed.Reason.Type)
			if failed.Reason.Type != "need-rescheduling" {
				t.Fatalf("event_id 振り直しの recording.failed reason=%q、want need-rescheduling", failed.Reason.Type)
			}
			assertNoReplacementRecord(t, ctx, client)
			return
		case <-time.After(300 * time.Millisecond):
			if replacement, ok := findReplacementRecord(t, ctx, client); ok {
				t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が作られた", replacement.Program.EventID)
			}
			if record, ok := findRecord(t, ctx, client, program.ID); ok {
				t.Logf("event_id 振り直し: record eventId=%d status=%s", record.Program.EventID, record.Recording.Status)
				if record.Program.EventID != fixture.EventID {
					t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が作られた", record.Program.EventID)
				}
				if record.Recording.Status == "failed" && record.Recording.FailedReason != nil {
					t.Logf("event_id 振り直し: record failed reason=%s", record.Recording.FailedReason.Type)
					if record.Recording.FailedReason.Type != "need-rescheduling" {
						t.Fatalf("record failed reason=%q、want need-rescheduling", record.Recording.FailedReason.Type)
					}
					return
				}
				if record.Recording.Status == "recording" || record.Recording.Status == "finished" {
					if record.Recording.Status == "recording" {
						waitForRecordingStatus(t, ctx, client, record.ID, "finished", 45*time.Second)
						final, err := client.GetRecord(ctx, record.ID)
						if err != nil {
							t.Fatalf("event_id 振り直し後の GetRecord: %v", err)
						}
						record = *final
					}
					t.Logf("event_id 振り直し: target event_id の録画が status=%s で残った（recording.failed なし）", record.Recording.Status)
					assertNoReplacementRecord(t, ctx, client)
					return
				}
			}
		}
	}
	t.Fatalf("event_id 振り直し後の recording.failed(need-rescheduling) が 45 秒以内に観測されなかった")
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

func assertNoReplacementRecord(t *testing.T, ctx context.Context, client *mirakc.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if replacement, ok := findReplacementRecord(t, ctx, client); ok {
			t.Fatalf("event_id 振り直し後に別 event_id=%d の録画が残った", replacement.Program.EventID)
		}
		time.Sleep(200 * time.Millisecond)
	}
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
