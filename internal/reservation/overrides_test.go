package reservation

import (
	"reflect"
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

func TestMergeOverrides_RejectsUnknownField(t *testing.T) {
	if _, err := MergeOverrides(Options{}, OverridesPatch{Reset: []OverrideField{"skip"}}); err == nil {
		t.Fatal("MergeOverrides(reset skip) = nil, want an unknown field error")
	}
}
