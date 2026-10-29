package breaker

import "testing"

func TestScopeKindForClassifiesBreakerScopes(t *testing.T) {
	tests := []struct {
		name string
		want ScopeKind
	}{
		{name: RulerDeletes, want: ScopeSite},
		{name: ReconcileTotalLoss, want: ScopeSite},
		{name: DeleteReconcile, want: ScopeStore},
		{name: "not_a_breaker", want: ScopeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScopeKindFor(tt.name); got != tt.want {
				t.Fatalf("ScopeKindFor(%q) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// All と scopeKinds は手で別々に書いてあるので、片方だけ直すと名前が ScopeUnknown
// になり、API が 400 を返し続ける。
func TestAllBreakersHaveKnownScope(t *testing.T) {
	for _, name := range All {
		if ScopeKindFor(name) == ScopeUnknown {
			t.Errorf("ScopeKindFor(%q) = ScopeUnknown, want a registered scope", name)
		}
	}
}
