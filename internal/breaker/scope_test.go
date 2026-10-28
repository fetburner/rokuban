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
