package reservation

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fullOptions() Options {
	priority := 5
	contentPath := "a/b.ts"
	template := "{{.Title}}"
	keep := KeepOriginalAlways
	profiles := []string{"p1"}
	return Options{
		Priority: &priority, ContentPath: &contentPath, FilenameTemplate: &template,
		KeepOriginal: &keep, EncodeProfiles: &profiles,
	}
}

func TestMergeOverrides_SetsOnlyGivenFields(t *testing.T) {
	existing := fullOptions()
	priority := 9
	got, err := MergeOverrides(existing, OverridesPatch{Priority: &priority})
	if err != nil {
		t.Fatalf("MergeOverrides() = %v", err)
	}
	want := fullOptions()
	want.Priority = &priority
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeOverrides() = %+v, want only priority changed: %+v", got, want)
	}
	if *existing.Priority != 5 {
		t.Fatalf("existing priority = %d, want the input to stay untouched", *existing.Priority)
	}
}

func TestMergeOverrides_ResetClearsEachField(t *testing.T) {
	for _, field := range OverrideFields() {
		got, err := MergeOverrides(fullOptions(), OverridesPatch{Reset: []OverrideField{field}})
		if err != nil {
			t.Fatalf("MergeOverrides(reset %s) = %v", field, err)
		}
		before, after := reflect.ValueOf(fullOptions()), reflect.ValueOf(got)
		cleared := 0
		for i := range before.NumField() {
			if !before.Field(i).IsNil() && after.Field(i).IsNil() {
				cleared++
			}
		}
		if cleared != 1 {
			t.Errorf("reset %s cleared %d fields, want exactly 1 (got %+v)", field, cleared, got)
		}
	}
}

// TestOverrideFieldsMatchOptions は、OverrideFields が Options の上書き対象フィールドの集合と
// 一致することを固定する。Options に足して OverrideFields に足し忘れた場合と、
// OverrideFields から落とした場合の両方で落ちる。
func TestOverrideFieldsMatchOptions(t *testing.T) {
	var want []string
	typ := reflect.TypeOf(Options{})
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		// skip は overrides のキーではなく program_intents.action が担うので、上書きの対象に含めない。
		if name == "skip" {
			continue
		}
		want = append(want, name)
	}
	var got []string
	for _, f := range OverrideFields() {
		got = append(got, string(f))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("OverrideFields() = %v, want the Options fields except skip: %v", got, want)
	}
}

func TestMergeOverrides_RejectsUnknownField(t *testing.T) {
	if _, err := MergeOverrides(Options{}, OverridesPatch{Reset: []OverrideField{"skip"}}); err == nil {
		t.Fatal("MergeOverrides(reset skip) = nil, want an unknown field error")
	}
}
