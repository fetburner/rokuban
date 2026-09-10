package ruler

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

type failingProgramIDReuseLister struct{}

func (failingProgramIDReuseLister) ListProgramIDReusesBySite(context.Context, string) ([]sqlcgen.ListProgramIDReusesBySiteRow, error) {
	return nil, errors.New("synthetic detector failure")
}

// 再利用検出は補助観測なので、クエリに失敗してもエラーを返さずに終了する。
// runPassForSite はこの関数の後に通常の評価・適用を続ける契約である。
func TestObserveProgramIDReuses_QueryFailureIsNonFatal(t *testing.T) {
	t.Helper()
	observeProgramIDReuses(context.Background(), failingProgramIDReuseLister{}, "default")
}
