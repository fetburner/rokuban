package pgstore

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

type originalStoreRuleValue struct {
	Match *originalStoreMatchValue `json:"match,omitempty"`
	Store string                   `json:"store"`
}

type originalStoreMatchValue struct {
	Site            string `json:"site,omitempty"`
	OlderThanMicros int64  `json:"older_than_us,omitempty"`
}

type encodedStoragePolicy struct {
	originalRules []byte
	fsStores      []string
	derivedStore  string
}

func encodeStoragePolicy(policy store.StoragePolicy) (encodedStoragePolicy, error) {
	rules, err := originalRulesJSON(policy)
	if err != nil {
		return encodedStoragePolicy{}, err
	}
	return encodedStoragePolicy{
		originalRules: rules,
		fsStores:      policy.FSStores(),
		derivedStore:  policy.DerivedStore(),
	}, nil
}

func originalRulesJSON(policy store.StoragePolicy) ([]byte, error) {
	rules := policy.OriginalRules()
	encodedRules := make([]originalStoreRuleValue, 0, len(rules))
	for i, rule := range rules {
		if rule.OlderThan < 0 || rule.OlderThan%time.Microsecond != 0 {
			return nil, fmt.Errorf("original rule %d older-than duration must have microsecond precision", i)
		}
		converted := originalStoreRuleValue{Store: rule.Store}
		if rule.Site != "" || rule.OlderThan != 0 {
			match := &originalStoreMatchValue{Site: rule.Site}
			if rule.OlderThan != 0 {
				match.OlderThanMicros = rule.OlderThan.Microseconds()
			}
			converted.Match = match
		}
		encodedRules = append(encodedRules, converted)
	}
	encoded, err := json.Marshal(encodedRules)
	if err != nil {
		return nil, fmt.Errorf("encoding original store rules: %w", err)
	}
	return encoded, nil
}
