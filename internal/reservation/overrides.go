package reservation

import "fmt"

// OverrideField は、上書きを取り除く（reset する）予約オプションのフィールド名。
// skip はここに含めない。skip は overrides のキーではなく program_intents.action が担う。
type OverrideField string

const (
	// OverridePriority は priority の上書きを指す。
	OverridePriority OverrideField = "priority"
	// OverrideContentPath は contentPath の上書きを指す。
	OverrideContentPath OverrideField = "contentPath"
	// OverrideFilenameTemplate は filenameTemplate の上書きを指す。
	OverrideFilenameTemplate OverrideField = "filenameTemplate"
	// OverrideKeepOriginal は keepOriginal の上書きを指す。
	OverrideKeepOriginal OverrideField = "keepOriginal"
	// OverrideEncodeProfiles は encodeProfiles の上書きを指す。
	OverrideEncodeProfiles OverrideField = "encodeProfiles"
)

// OverrideFields は reset で許容する全フィールド名を返す。上書きの対象である Options の
// フィールド（skip を除く全部）と一致することを TestOverrideFieldsMatchOptions が固定し、
// 各名前が reset で 1 フィールドを消すことを TestMergeOverrides_ResetClearsEachField が固定する。
func OverrideFields() []OverrideField {
	return []OverrideField{
		OverridePriority, OverrideContentPath, OverrideFilenameTemplate, OverrideKeepOriginal, OverrideEncodeProfiles,
	}
}

// OverridesPatch は上書きの部分更新。値のフィールドが nil なら、そのフィールドは変更しない。
type OverridesPatch struct {
	Priority         *int
	ContentPath      *string
	FilenameTemplate *string
	KeepOriginal     *string
	EncodeProfiles   *[]string
	Reset            []OverrideField
}

// MergeOverrides は既存の上書きに patch を適用した結果を返す。値を持つフィールドは設定し、
// Reset に挙げたフィールドは取り除く。副作用はない。
//
// フィールド名と Options のフィールドの対応は、この関数の 2 箇所（設定と reset の switch）と
// OverrideFields だけに置く。新しい上書きフィールドを足すときは、Options・OverridesPatch・
// OverrideField の定数・OverrideFields・この関数の 2 箇所を揃える。API 側の許容値
// （internal/api の overridesFields）と食い違えば internal/api のテストが落ちる。
func MergeOverrides(existing Options, patch OverridesPatch) (Options, error) {
	merged := existing
	if patch.Priority != nil {
		merged.Priority = patch.Priority
	}
	if patch.ContentPath != nil {
		merged.ContentPath = patch.ContentPath
	}
	if patch.FilenameTemplate != nil {
		merged.FilenameTemplate = patch.FilenameTemplate
	}
	if patch.KeepOriginal != nil {
		merged.KeepOriginal = patch.KeepOriginal
	}
	if patch.EncodeProfiles != nil {
		merged.EncodeProfiles = patch.EncodeProfiles
	}
	for _, field := range patch.Reset {
		switch field {
		case OverridePriority:
			merged.Priority = nil
		case OverrideContentPath:
			merged.ContentPath = nil
		case OverrideFilenameTemplate:
			merged.FilenameTemplate = nil
		case OverrideKeepOriginal:
			merged.KeepOriginal = nil
		case OverrideEncodeProfiles:
			merged.EncodeProfiles = nil
		default:
			return Options{}, fmt.Errorf("unknown override field %q", field)
		}
	}
	return merged, nil
}
