package pgstore

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"

	"github.com/fetburner/rokuban/internal/catalogdoc"
)

// normalizeRuleImportKeys は配列が無い旧ダンプだけを移行し、呼び出し元の文書を変更しない（TestNormalizeRuleImportKeys）。
func normalizeRuleImportKeys(doc *catalogdoc.Document) (*catalogdoc.Document, error) {
	if doc.RuleImportKeys != nil {
		return doc, nil
	}
	out := *doc
	out.Rules = slices.Clone(doc.Rules)
	out.RuleImportKeys = []catalogdoc.RuleImportKey{}
	for i := range out.Rules {
		rule := &out.Rules[i]
		var meta map[string]json.RawMessage
		if len(rule.Metadata) == 0 || rule.Metadata[0] != '{' {
			continue
		}
		if err := json.Unmarshal(rule.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("decoding rule %d metadata: %w", rule.ID, err)
		}
		var epg map[string]json.RawMessage
		raw := meta["epgstation"]
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		if err := json.Unmarshal(raw, &epg); err != nil {
			return nil, fmt.Errorf("decoding rule %d legacy key: %w", rule.ID, err)
		}
		value := epg["ruleId"]
		if len(value) == 0 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) {
			continue
		}
		// float64 を経由せず、整数かつ int64 の範囲内かを判定する（TestNormalizeRuleImportKeys）。
		number, ok := new(big.Rat).SetString(string(value))
		if !ok {
			return nil, fmt.Errorf("invalid numeric import key for rule %d", rule.ID)
		}
		if !number.IsInt() || !number.Num().IsInt64() {
			continue
		}
		out.RuleImportKeys = append(out.RuleImportKeys, catalogdoc.RuleImportKey{
			RuleID: rule.ID, Source: "epgstation", ExternalID: strconv.FormatInt(number.Num().Int64(), 10),
		})
		delete(epg, "ruleId")
		if len(epg) == 0 {
			delete(meta, "epgstation")
		} else {
			encoded, err := json.Marshal(epg)
			if err != nil {
				return nil, fmt.Errorf("encoding legacy metadata: %w", err)
			}
			meta["epgstation"] = encoded
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			return nil, fmt.Errorf("encoding rule metadata: %w", err)
		}
		rule.Metadata = encoded
	}
	return &out, nil
}
