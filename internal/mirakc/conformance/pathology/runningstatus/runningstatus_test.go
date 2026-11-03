//go:build conformance

package runningstatus

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/pathology"
)

// TestRunningStatusStartsSoon は running_status=2 の target で、録画 pipeline の開始が
// target.startAt より先になること（実測 lead≈11.6s）を測る。
func TestRunningStatusStartsSoon(t *testing.T) {
	pathology.Run(t, fixture.CaseRunningStatus)
}
