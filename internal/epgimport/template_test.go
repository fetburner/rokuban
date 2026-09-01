package epgimport

import (
	"reflect"
	"testing"
)

func TestConvertRecordedFormat_KnownVariables(t *testing.T) {
	got, unsupported := ConvertRecordedFormat("%YEAR%%MONTH%%DAY%_%TITLE%_%SID%")
	if unsupported != nil {
		t.Fatalf("unsupported = %v, want nil", unsupported)
	}
	want := "{{.Year}}{{.Month}}{{.Day}}_{{.Title}}_{{.ServiceID}}"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}

// TestConvertRecordedFormat_UnsupportedVariable は受け入れ基準そのもの:
// %CHNAME% 等の未対応変数が黙って空文字にならず、変換失敗（tmpl=""）+
// unsupported リストになることを確認する。
func TestConvertRecordedFormat_UnsupportedVariable(t *testing.T) {
	tmpl, unsupported := ConvertRecordedFormat("%CHNAME%_%TITLE%")
	if tmpl != "" {
		t.Errorf("tmpl = %q, want \"\" (must not silently convert on unsupported var)", tmpl)
	}
	if !reflect.DeepEqual(unsupported, []string{"%CHNAME%"}) {
		t.Errorf("unsupported = %v, want [%%CHNAME%%]", unsupported)
	}
}

func TestConvertRecordedFormat_UnknownTypo(t *testing.T) {
	// %TITEL% はタイプミス（変換表に無い）。空文字化せず不支持として拾う。
	tmpl, unsupported := ConvertRecordedFormat("%TITEL%")
	if tmpl != "" {
		t.Errorf("tmpl = %q, want \"\"", tmpl)
	}
	if len(unsupported) != 1 || unsupported[0] != "%TITEL%" {
		t.Errorf("unsupported = %v, want [%%TITEL%%]", unsupported)
	}
}

func TestConvertRecordedFormat_Empty(t *testing.T) {
	tmpl, unsupported := ConvertRecordedFormat("")
	if tmpl != "" || unsupported != nil {
		t.Errorf("got tmpl=%q unsupported=%v, want \"\"/nil", tmpl, unsupported)
	}
}
