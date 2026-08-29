package ptr

import "testing"

func TestDeref(t *testing.T) {
	if got := Deref[string](nil); got != "" {
		t.Errorf("Deref[string](nil) = %q, want %q", got, "")
	}
	s := "hello"
	if got := Deref(&s); got != "hello" {
		t.Errorf("Deref(&s) = %q, want %q", got, "hello")
	}

	if got := Deref[int64](nil); got != 0 {
		t.Errorf("Deref[int64](nil) = %d, want 0", got)
	}
	var i int64 = 42
	if got := Deref(&i); got != 42 {
		t.Errorf("Deref(&i) = %d, want 42", got)
	}

	if got := Deref[bool](nil); got != false {
		t.Errorf("Deref[bool](nil) = %v, want false", got)
	}
	b := true
	if got := Deref(&b); got != true {
		t.Errorf("Deref(&b) = %v, want true", got)
	}
}
