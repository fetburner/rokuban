package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// CMLogoAreaParams は CMLogoArea の上書き対象。
type CMLogoAreaParams struct {
	NetworkID   int32
	ServiceID   int32
	X           int32
	Y           int32
	W           int32
	H           int32
	CodedWidth  int32
	CodedHeight int32
}

// CMLogoArea は局のロゴ枠を作る。既定値は network_id = 32736、service_id = 1024、枠 (1180, 24, 240, 96)、coded = 1440x1080。
// opts で既定値を上書きする。枠の保存（CMStation.SaveLogoArea）は解析元の録画と原本を要求し、既存の候補を消すので、
// 枠だけを前提にするテストはこのビルダーで作る。updated_at は now() になる。
func CMLogoArea(t testing.TB, db sqlcgen.DBTX, opts ...func(*CMLogoAreaParams)) {
	t.Helper()
	params := CMLogoAreaParams{
		NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).UpsertCMLogoArea(t.Context(), sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: params.NetworkID, ServiceID: params.ServiceID, X: params.X, Y: params.Y, W: params.W, H: params.H,
		CodedWidth: params.CodedWidth, CodedHeight: params.CodedHeight,
	}); err != nil {
		t.Fatalf("upserting CM logo area for network %d service %d: %v", params.NetworkID, params.ServiceID, err)
	}
}
