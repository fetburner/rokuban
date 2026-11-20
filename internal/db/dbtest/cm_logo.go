package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// CMLogoParams は CMLogo の上書き対象。LearnedFrom は録画 ID で、nil なら録画を指さない。
type CMLogoParams struct {
	NetworkID   int32
	ServiceID   int32
	LGD         []byte
	PreviewPNG  []byte
	LearnedFrom *int64
	CodedWidth  int32
	CodedHeight int32
}

// CMLogo は局のロゴを作る。既定値は network_id = 32736、service_id = 1024、lgd = "lgd"、coded = 1440x1080、learned_from = nil。
// opts で既定値を上書きする。学習の経路（試行と枠の版の照合）は通らず、learned_at は now() になる。
// 学習の保存そのものを試すテストは、CMDetection.SaveLearnedLogo で作る。
func CMLogo(t testing.TB, db sqlcgen.DBTX, opts ...func(*CMLogoParams)) {
	t.Helper()
	params := CMLogoParams{
		NetworkID: 32736, ServiceID: 1024, LGD: []byte("lgd"), CodedWidth: 1440, CodedHeight: 1080,
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).UpsertCMLogo(t.Context(), sqlcgen.UpsertCMLogoParams{
		NetworkID: params.NetworkID, ServiceID: params.ServiceID, Lgd: params.LGD, PreviewPng: params.PreviewPNG,
		LearnedFrom: params.LearnedFrom, CodedWidth: params.CodedWidth, CodedHeight: params.CodedHeight,
	}); err != nil {
		t.Fatalf("upserting CM logo for network %d service %d: %v", params.NetworkID, params.ServiceID, err)
	}
}
