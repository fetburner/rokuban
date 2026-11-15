package pgstore

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/catalogdoc"
)

func TestNormalizeRuleImportKeys(t *testing.T) {
	for _, tc := range []struct{ name, metadata, wantMetadata, wantID string }{
		{"number", `{"epgstation":{"ruleId":42}}`, `{}`, "42"},
		{"other fields", `{"epgstation":{"ruleId":42,"other":true},"user":"kept"}`, `{"epgstation":{"other":true},"user":"kept"}`, "42"},
		{"large number", `{"epgstation":{"ruleId":9007199254740993}}`, `{}`, "9007199254740993"},
		{"decimal", `{"epgstation":{"ruleId":42.0}}`, `{}`, "42"},
		{"exponent", `{"epgstation":{"ruleId":4.2e1}}`, `{}`, "42"},
		{"fraction", `{"epgstation":{"ruleId":42.5}}`, `{"epgstation":{"ruleId":42.5}}`, ""},
		{"small fraction", `{"epgstation":{"ruleId":0.1250}}`, `{"epgstation":{"ruleId":0.1250}}`, ""},
		{"precise fraction", `{"epgstation":{"ruleId":42.0000000000000000001}}`, `{"epgstation":{"ruleId":42.0000000000000000001}}`, ""},
		{"maximum", `{"epgstation":{"ruleId":9223372036854775807}}`, `{}`, "9223372036854775807"},
		{"minimum", `{"epgstation":{"ruleId":-9223372036854775808}}`, `{}`, "-9223372036854775808"},
		{"overflow", `{"epgstation":{"ruleId":9223372036854775808}}`, `{"epgstation":{"ruleId":9223372036854775808}}`, ""},
		{"underflow", `{"epgstation":{"ruleId":-9223372036854775809}}`, `{"epgstation":{"ruleId":-9223372036854775809}}`, ""},
		{"string", `{"epgstation":{"ruleId":"42"}}`, `{"epgstation":{"ruleId":"42"}}`, ""},
		{"null", `{"epgstation":{"ruleId":null}}`, `{"epgstation":{"ruleId":null}}`, ""},
		{"boolean", `{"epgstation":{"ruleId":true}}`, `{"epgstation":{"ruleId":true}}`, ""},
		{"array", `{"epgstation":{"ruleId":[42]}}`, `{"epgstation":{"ruleId":[42]}}`, ""},
		{"object", `{"epgstation":{"ruleId":{"value":42}}}`, `{"epgstation":{"ruleId":{"value":42}}}`, ""},
		{"no key", `{"epgstation":{},"user":1}`, `{"epgstation":{},"user":1}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := &catalogdoc.Document{Rules: []catalogdoc.Rule{{ID: 7, Metadata: json.RawMessage(tc.metadata)}}}
			got, err := normalizeRuleImportKeys(doc)
			if err != nil {
				t.Fatal(err)
			}
			want := []catalogdoc.RuleImportKey{}
			if tc.wantID != "" {
				want = append(want, catalogdoc.RuleImportKey{RuleID: 7, Source: "epgstation", ExternalID: tc.wantID})
			}
			if !reflect.DeepEqual(got.RuleImportKeys, want) {
				t.Fatalf("keys = %+v, want %+v", got.RuleImportKeys, want)
			}
			assertImportMetadata(t, got.Rules[0].Metadata, tc.wantMetadata)
			if string(doc.Rules[0].Metadata) != tc.metadata || doc.RuleImportKeys != nil {
				t.Fatal("input document was mutated")
			}
			doc.RuleImportKeys = []catalogdoc.RuleImportKey{}
			current, err := normalizeRuleImportKeys(doc)
			if err != nil {
				t.Fatal(err)
			}
			if len(current.RuleImportKeys) != 0 {
				t.Fatalf("new format imported opaque metadata: %+v", current.RuleImportKeys)
			}
			assertImportMetadata(t, current.Rules[0].Metadata, tc.metadata)
		})
	}
}
