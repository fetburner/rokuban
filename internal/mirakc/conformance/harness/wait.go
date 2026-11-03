//go:build conformance

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
)

// EPGBootstrapTimeout は mirakc が EPG の service と program を公開するまでの待機時間。
const EPGBootstrapTimeout = 60 * time.Second

// RecordingStartTimeout は mirakc が録画 record を作るまでの待機時間。
const RecordingStartTimeout = 20 * time.Second

// RecordingFinishTimeout は fixture の event duration に録画終了の余裕を加えた待機時間。
const RecordingFinishTimeout = fixture.EventDuration + 60*time.Second

// WaitForService は serviceID の service が mirakc に現れるまで待つ。
func WaitForService(t *testing.T, ctx context.Context, c *mirakc.Client, serviceID int64) {
	t.Helper()
	deadline := time.Now().Add(EPGBootstrapTimeout)
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
	t.Fatalf("service id=%d が %s 以内に現れなかった（scan-services が失敗しているか、フィクスチャの SI が壊れている）", serviceID, EPGBootstrapTimeout)
}

// WaitForProgram は programID の program が mirakc に現れるまで待つ。
func WaitForProgram(t *testing.T, ctx context.Context, c *mirakc.Client, programID int64) {
	t.Helper()
	deadline := time.Now().Add(EPGBootstrapTimeout)
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
	t.Fatalf("program id=%d が %s 以内に現れなかった（update-schedules が失敗しているか、フィクスチャの EIT が壊れている）", programID, EPGBootstrapTimeout)
}

// WaitForRecord は programID の record が mirakc に現れるまで待ち、その record ID を返す。
func WaitForRecord(t *testing.T, ctx context.Context, c *mirakc.Client, programID int64) string {
	t.Helper()
	deadline := time.Now().Add(RecordingStartTimeout)
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
	t.Fatalf("programId=%d の record が %s 以内に現れなかった（mirakc がスケジュールどおり録画を開始していない）", programID, RecordingStartTimeout)
	return ""
}

// WaitForRecordingStatus は指定した record が want の状態になるまで待つ。
func WaitForRecordingStatus(t *testing.T, ctx context.Context, c *mirakc.Client, recordID, want string, timeout time.Duration) {
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
	t.Fatalf("record %s が %s 以内に status=%q に到達しなかった（最後に見えた status=%q）", recordID, timeout, want, last)
}
