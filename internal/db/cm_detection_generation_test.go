package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestCMDetectionGenerationAdvancesOnlyOnRealTransitions は、cm_detect の切り替え
// （SQL クエリ SetRecordingCMDetection / ClearRecordingCMDetection）が
// cm_detect_generation を実際に値が変わるときだけ 1 進めることを、遷移ごとの行で固定する。
// 世代は試行行の現在性の柵なので、同じ値の再送で進むと有効な試行が無効になり、
// 変わるべき切り替えで進まないと古い worker が新しい試行に触れる。
// 各行は前提の状態、操作の列、期待の世代と cm_detect を明示する。既定値には頼らない。
func TestCMDetectionGenerationAdvancesOnlyOnRealTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// policy は初期状態。none は凍結前（行が無い）、off / on は凍結済みで cm_detect が false / true。
		policy string
		// original は録画の資産の状態。active は利用可能な原本のコピーがあり、copy_deleted は原本はあるがコピーが deleted、none は資産が無い、encoded_only は原本が無く active な encoded 資産だけがある。
		original string
		deleted  bool
		// ops は "set"（SetRecordingCMDetection）と "clear"（ClearRecordingCMDetection）の列。
		ops []string
		// wantRows は set の影響行数。
		wantRows       int64
		wantPolicy     bool
		wantGeneration int64
		wantCMDetect   bool
	}{
		{name: "FreshFreezeWithCMDetect_StartsAtGeneration0", policy: "on", original: "active",
			ops: nil, wantPolicy: true, wantGeneration: 0, wantCMDetect: true},
		{name: "FirstFreezeUnfrozenWithActiveOriginal_StartsAtGeneration1", policy: "none", original: "active",
			ops: []string{"set"}, wantRows: 1, wantPolicy: true, wantGeneration: 1, wantCMDetect: true},
		{name: "FrozenOffEnable_Advances", policy: "off", original: "active",
			ops: []string{"set"}, wantRows: 1, wantPolicy: true, wantGeneration: 1, wantCMDetect: true},
		{name: "FrozenOnReenable_Idempotent", policy: "on", original: "active",
			ops: []string{"set"}, wantRows: 1, wantPolicy: true, wantGeneration: 0, wantCMDetect: true},
		{name: "FrozenOffEnableWithoutOriginal_NotApplied", policy: "off", original: "none",
			ops: []string{"set"}, wantRows: 0, wantPolicy: true, wantGeneration: 0, wantCMDetect: false},
		{name: "FrozenOffEnableWithDeletedOriginalCopy_NotApplied", policy: "off", original: "copy_deleted",
			ops: []string{"set"}, wantRows: 0, wantPolicy: true, wantGeneration: 0, wantCMDetect: false},
		{name: "FrozenOffEnableOfDeletedRecording_NotApplied", policy: "off", original: "active", deleted: true,
			ops: []string{"set"}, wantRows: 0, wantPolicy: true, wantGeneration: 0, wantCMDetect: false},
		{name: "UnfrozenWithoutOriginal_NotFrozen", policy: "none", original: "none",
			ops: []string{"set"}, wantRows: 0, wantPolicy: false},
		{name: "UnfrozenWithDeletedOriginalCopy_NotFrozen", policy: "none", original: "copy_deleted",
			ops: []string{"set"}, wantRows: 0, wantPolicy: false},
		{name: "UnfrozenOfDeletedRecording_NotFrozen", policy: "none", original: "active", deleted: true,
			ops: []string{"set"}, wantRows: 0, wantPolicy: false},
		{name: "UnfrozenWithOnlyNonOriginalAsset_NotFrozen", policy: "none", original: "encoded_only",
			ops: []string{"set"}, wantRows: 0, wantPolicy: false},
		{name: "FrozenOnDisable_Advances", policy: "on", original: "active",
			ops: []string{"clear"}, wantPolicy: true, wantGeneration: 1, wantCMDetect: false},
		{name: "FrozenOnDisableTwice_SecondIsIdempotent", policy: "on", original: "active",
			ops: []string{"clear", "clear"}, wantPolicy: true, wantGeneration: 1, wantCMDetect: false},
		{name: "FrozenOffDisable_Idempotent", policy: "off", original: "active",
			ops: []string{"clear"}, wantPolicy: true, wantGeneration: 0, wantCMDetect: false},
		{name: "RoundTrip_EnableDisableDisableEnable_GenerationIs2", policy: "on", original: "active",
			ops: []string{"set", "clear", "clear", "set"}, wantRows: 1, wantPolicy: true, wantGeneration: 2, wantCMDetect: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			q := sqlcgen.New(pool)

			id := dbtest.Recording(t, pool)
			switch tc.original {
			case "none":
			case "encoded_only":
				profile := "h264"
				dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
					p.Kind = "encoded"
					p.Profile = &profile
				})
			case "active", "copy_deleted":
				assetID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.Kind = "original" })
				if tc.original == "copy_deleted" {
					dbtest.SetMediaAssetState(t, pool, assetID, "deleted")
				}
			default:
				t.Fatalf("unknown original state %q", tc.original)
			}
			if tc.policy != "none" {
				dbtest.ProcessingPolicy(t, pool, id, func(p *dbtest.ProcessingPolicyParams) {
					p.KeepOriginal = "always"
					p.EncodeProfiles = []string{}
					p.CMDetect = tc.policy == "on"
				})
			}
			if tc.deleted {
				testutil.MustExec(t, pool, "setting recording deleted_at",
					"UPDATE recordings SET deleted_at = now() WHERE id = $1", id)
			}

			for _, op := range tc.ops {
				switch op {
				case "set":
					rows, err := q.SetRecordingCMDetection(ctx, id)
					if err != nil || rows != tc.wantRows {
						t.Fatalf("SetRecordingCMDetection(%d) = %d, %v; want %d rows", id, rows, err, tc.wantRows)
					}
				case "clear":
					testutil.RequireNoError(t, q.ClearRecordingCMDetection(ctx, id), "q.ClearRecordingCMDetection")
				default:
					t.Fatalf("unknown op %q", op)
				}
			}

			var generation int64
			var cmDetect bool
			err := pool.QueryRow(ctx,
				`SELECT cm_detect_generation, cm_detect FROM recording_processing_policy WHERE recording_id = $1`, id).
				Scan(&generation, &cmDetect)
			hasPolicy := err == nil
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("reading processing policy: %v", err)
			}
			if hasPolicy != tc.wantPolicy {
				t.Fatalf("processing policy row exists = %v, want %v", hasPolicy, tc.wantPolicy)
			}
			if !hasPolicy {
				return
			}
			if generation != tc.wantGeneration || cmDetect != tc.wantCMDetect {
				t.Fatalf("after %v: cm_detect_generation = %d, cm_detect = %v; want %d, %v",
					tc.ops, generation, cmDetect, tc.wantGeneration, tc.wantCMDetect)
			}
		})
	}
}
