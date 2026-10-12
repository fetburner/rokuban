package api

import jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"

type recordingEnqueuer = jobqtestutil.RecordingEnqueuer

func newRecordingEnqueuer() *recordingEnqueuer {
	return jobqtestutil.NewRecordingEnqueuer()
}
