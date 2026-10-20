package api

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/reservation"
)

// TestOverridesFieldsMatchReservation は、API が reset で許容する名前と、reservation が扱う名前の
// 集合が一致することを固定する。片方だけにフィールドを足すと、reset が 400 になるか、
// store で未知のフィールドとして落ちる。
func TestOverridesFieldsMatchReservation(t *testing.T) {
	var apiFields, domainFields []string
	for _, f := range overridesFields {
		apiFields = append(apiFields, string(f))
	}
	for _, f := range reservation.OverrideFields() {
		domainFields = append(domainFields, string(f))
	}
	slices.Sort(apiFields)
	slices.Sort(domainFields)
	if !slices.Equal(apiFields, domainFields) {
		t.Fatalf("overridesFields = %v, reservation.OverrideFields() = %v, want the same set", apiFields, domainFields)
	}
}
