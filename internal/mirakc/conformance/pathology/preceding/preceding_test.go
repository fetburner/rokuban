//go:build conformance

package preceding

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/pathology"
)

// TestPrecedingProgramExtension は mirakc 4.0.0-dev.0 相当で、未定尺の前番組を含む
// stream が target.startAt より先に録画されること（実測 lead≈7.0s）を測る。
func TestPrecedingProgramExtension(t *testing.T) {
	pathology.Run(t, fixture.CasePrecedingExtension)
}
