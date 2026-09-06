//go:build conformance

package conformance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/programid"
)

// TestLiveStreamReleaseUnderRecording は、実 mirakc のチューナー解放を測る。
//
// 2 本の fixture tuner を 3 波へ割り当て、A の録画と B のライブストリームで容量を埋める。
// C のライブ要求が 503 になることを確認した後、B の HTTP body を Close して C を一度だけ
// 再要求する。再要求の成功と Close からレスポンスまでの時間を、Issue #677 の
// LiveSession.stop（ctx cancel + done 待ち）の根拠としてログに残す。
func TestLiveStreamReleaseUnderRecording(t *testing.T) {
	dir := testDir(t)
	tunerBin := buildFixtureTuner(t, dir)
	container := startMirakcWithConfig(t, dir, tunerBin, "",
		mirakcLiveReleaseConfigYAML("/fixtures/fixturetuner {{{channel}}}"))
	client := mirakc.NewClient(container.baseURL, nil)
	ctx := context.Background()

	serviceID := func(siServiceID int) int64 {
		return programid.ServiceID(fixture.NetworkID, siServiceID)
	}
	const (
		recordingService  = 101
		staleLiveService  = 102
		targetLiveService = 103
	)
	for _, siServiceID := range []int{recordingService, staleLiveService, targetLiveService} {
		waitForService(t, ctx, client, serviceID(siServiceID))
	}

	tuners, err := client.ListTuners(ctx)
	if err != nil {
		t.Fatalf("ListTuners: %v", err)
	}
	if len(tuners) != 2 {
		t.Fatalf("len(ListTuners) = %d, want 2", len(tuners))
	}

	recordingProgramID := programid.ComposeProgramID(fixture.NetworkID, recordingService, fixture.EventID)
	waitForProgram(t, ctx, client, recordingProgramID)
	if _, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: recordingProgramID,
		Options:   mirakc.Options{Priority: 10},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	recordID := waitForRecord(t, ctx, client, recordingProgramID)
	waitForRecordingStatus(t, ctx, client, recordID, "recording", recordingStartTimeout)

	stale, err := client.StreamService(ctx, serviceID(staleLiveService), 1)
	if err != nil {
		t.Fatalf("StreamService(stale): %v", err)
	}
	staleClosed := false
	defer func() {
		if !staleClosed {
			_ = stale.Close()
		}
	}()

	// A の録画 + B のライブで 2 本とも使用中なので、異なる波 C は受け付けられない。
	failedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = client.StreamService(failedCtx, serviceID(targetLiveService), 1)
	cancel()
	if err == nil {
		t.Fatalf("StreamService(target) succeeded while recording and stale live stream occupied both tuners")
	}
	var apiErr *mirakc.APIError
	// mirakc 4.0.0-dev.0 reports tuner exhaustion from this endpoint as 404; some
	// compatible versions use 503. Both are upstream rejection and exercise the
	// same release path in rokuban.
	if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusNotFound && apiErr.StatusCode != http.StatusServiceUnavailable) {
		t.Fatalf("StreamService(target) error = %v, want mirakc capacity response (404 or 503)", err)
	}

	closedAt := time.Now()
	if err := stale.Close(); err != nil {
		t.Fatalf("closing stale live stream: %v", err)
	}
	staleClosed = true

	// mirakc stops the tuner process asynchronously after the HTTP client closes its body.
	// This loop measures that implementation detail; it is deliberately outside rokuban's
	// one-retry policy. The test fails on any error other than the same capacity response.
	var target io.ReadCloser
	var attempts int
	releaseDeadline := time.Now().Add(5 * time.Second)
	for attempts = 1; time.Now().Before(releaseDeadline); attempts++ {
		retryCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		target, err = client.StreamService(retryCtx, serviceID(targetLiveService), 1)
		cancel()
		if err == nil {
			break
		}
		var retryErr *mirakc.APIError
		capacityError := errors.As(err, &retryErr) &&
			(retryErr.StatusCode == http.StatusNotFound || retryErr.StatusCode == http.StatusServiceUnavailable)
		if !capacityError && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StreamService(target) after stale Close: %v", err)
		}
		target = nil
		time.Sleep(100 * time.Millisecond)
	}
	if target == nil {
		t.Fatalf("StreamService(target) did not recover within %s after stale Close", 5*time.Second)
	}
	defer target.Close()

	t.Logf("mirakc live tuner release: 2 tuners + 1 recording, Close -> next live response = %s (%d attempt(s))",
		time.Since(closedAt), attempts)
}
