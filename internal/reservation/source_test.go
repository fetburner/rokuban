package reservation

import "testing"

func TestDeriveRecordingSource(t *testing.T) {
	record := IntentRecord
	skip := "skip"
	tests := []struct {
		name           string
		intentAction   *string
		hasReservation bool
		want           string
	}{
		{name: "manual intent with reservation", intentAction: &record, hasReservation: true, want: SourceManual},
		{name: "manual intent after reservation is gone", intentAction: &record, want: SourceManual},
		{name: "reservation without record intent", hasReservation: true, want: SourceRule},
		{name: "no reservation or intent", want: SourceUnattributed},
		{name: "skip intent with reservation", intentAction: &skip, hasReservation: true, want: SourceRule},
		{name: "skip intent without reservation", intentAction: &skip, want: SourceUnattributed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveRecordingSource(tt.intentAction, tt.hasReservation); got != tt.want {
				t.Fatalf("DeriveRecordingSource(%v, %t) = %q, want %q", tt.intentAction, tt.hasReservation, got, tt.want)
			}
		})
	}
}
