package jobq

import "testing"

// OnceOutcome.String がログに出す名前を持つこと（運用側が
// 「1 件やって終わった」と「空振りで終わった」を区別する手段）。
func TestOnceOutcome_String(t *testing.T) {
	for _, tt := range []struct {
		outcome OnceOutcome
		want    string
	}{
		{OnceOutcomeJobDone, "job_done"},
		{OnceOutcomeIdleTimeout, "idle_timeout"},
		{OnceOutcomeCanceled, "canceled"},
		// docs と README が名指ししている値（outcome 属性が job_unhandled）。
		{OnceOutcomeJobUnhandled, "job_unhandled"},
		{OnceOutcome(99), "unknown"},
	} {
		if got := tt.outcome.String(); got != tt.want {
			t.Errorf("OnceOutcome(%d).String() = %q, want %q", tt.outcome, got, tt.want)
		}
	}
}
