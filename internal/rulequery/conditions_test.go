package rulequery

import "testing"

func TestValidateRequiresSites(t *testing.T) {
	if err := Validate(Conditions{}); err == nil {
		t.Fatal("Validate accepted empty sites")
	}
	if err := Validate(Conditions{Sites: []string{"default"}}); err != nil {
		t.Fatalf("Validate with a site: %v", err)
	}
}

func TestValidateRejectsUnsupportedTextCondition(t *testing.T) {
	for _, match := range []TextMatch{
		{Target: "name; SELECT 1", Mode: "keyword"},
		{Target: "name", Mode: "case-insensitive"},
	} {
		if err := Validate(Conditions{Sites: []string{"default"}, TextMatches: []TextMatch{match}}); err == nil {
			t.Errorf("Validate accepted target=%q mode=%q", match.Target, match.Mode)
		}
	}
}

func TestValidateRejectsInvalidTimeWindow(t *testing.T) {
	for _, window := range []TimeWindow{
		{Weekdays: 0, StartSec: 0, EndSec: 1},
		{Weekdays: 128, StartSec: 0, EndSec: 1},
		{Weekdays: 1, StartSec: -1, EndSec: 1},
		{Weekdays: 1, StartSec: 0, EndSec: 86401},
	} {
		if err := Validate(Conditions{Sites: []string{"default"}, Times: []TimeWindow{window}}); err == nil {
			t.Errorf("Validate accepted time window %+v", window)
		}
	}
}
