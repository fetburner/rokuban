//go:build conformance

package eventidreset

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/pathology"
)

// TestEventIDReset は event_id が同じ時間帯で振り直されても、recording.failed を出さず
// 予約対象の record が recording から finished まで残ることを測る。
func TestEventIDReset(t *testing.T) {
	pathology.Run(t, fixture.CaseEventIDReset)
}
