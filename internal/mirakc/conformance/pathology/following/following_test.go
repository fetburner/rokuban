//go:build conformance

package following

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
	"github.com/fetburner/rokuban/internal/mirakc/conformance/pathology"
)

// TestFollowingProgram は present が前番組で following が target のとき、録画 pipeline が
// target.startAt より先に始まること（実測 lead≈11.7〜11.9s）を測る。
func TestFollowingProgram(t *testing.T) {
	pathology.Run(t, fixture.CaseFollowing)
}
