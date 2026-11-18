package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProcessingPolicyParams は ProcessingPolicy の上書き対象。KeepOriginal は 'always' か 'until_encoded'。until_encoded には空でない EncodeProfiles が必要（CHECK 制約）。
type ProcessingPolicyParams struct {
	KeepOriginal   string
	EncodeProfiles []string
	CMDetect       bool
}

// ProcessingPolicy は録画 recordingID の凍結済み処理方針（recording_processing_policy の行）を作る。既定値は keep_original = 'always'、encode_profiles = 空、cm_detect = false。opts で既定値を上書きする。行が既にあると失敗する（FreezeRecordingEncodePolicy は ON CONFLICT を持たない）。CHECK 制約により until_encoded には空でない encode_profiles が必要。
func ProcessingPolicy(t testing.TB, db sqlcgen.DBTX, recordingID int64, opts ...func(*ProcessingPolicyParams)) {
	t.Helper()
	params := ProcessingPolicyParams{
		KeepOriginal:   "always",
		EncodeProfiles: []string{},
		CMDetect:       false,
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).FreezeRecordingEncodePolicy(t.Context(), sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    recordingID,
		KeepOriginal:   params.KeepOriginal,
		EncodeProfiles: params.EncodeProfiles,
		CmDetect:       params.CMDetect,
	}); err != nil {
		t.Fatalf("freezing processing policy for recording %d: %v", recordingID, err)
	}
}
