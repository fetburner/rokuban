package store

import "time"

// OriginalStoreRule は条件に一致した原本の宛先を表す。
// Site と OlderThan がどちらも空なら無条件で一致する。
type OriginalStoreRule struct {
	Site      string
	OlderThan time.Duration
	Store     string
}

// StoragePolicy は原本と派生物の配置に使う設定値をまとめる。
// ゼロ値は規則も置き場も無い方針であり、原本の削除資格を作らない。
type StoragePolicy struct {
	originalRules    []OriginalStoreRule
	fsStores         []string
	derivedStore     string
	placementEnabled bool
}

// NewStoragePolicy は配置規則と置き場のスナップショットを作る。配置削除は既定で無効にする。
func NewStoragePolicy(originalRules []OriginalStoreRule, fsStores []string, derivedStore string) StoragePolicy {
	rules := append([]OriginalStoreRule(nil), originalRules...)
	return StoragePolicy{
		originalRules: rules,
		fsStores:      append([]string(nil), fsStores...),
		derivedStore:  derivedStore,
	}
}

// WithPlacementEnabled は配置削除の判定を有効または無効にした方針を返す。
func (p StoragePolicy) WithPlacementEnabled(enabled bool) StoragePolicy {
	p.placementEnabled = enabled
	return p
}

// PlacementPolicy は配置削除で使う方針を返す。無効な場合はゼロ値になる。
func (p StoragePolicy) PlacementPolicy() StoragePolicy {
	if !p.placementEnabled {
		return StoragePolicy{}
	}
	return p
}

// PlacementEnabled は配置削除の判定が有効かを返す。
func (p StoragePolicy) PlacementEnabled() bool {
	return p.placementEnabled
}

// OriginalRules は原本の配置規則を返す。
func (p StoragePolicy) OriginalRules() []OriginalStoreRule {
	return append([]OriginalStoreRule(nil), p.originalRules...)
}

// FSStores はファイルシステム置き場の名前を返す。
func (p StoragePolicy) FSStores() []string {
	return append([]string(nil), p.fsStores...)
}

// DerivedStore は派生物の置き場の名前を返す。
func (p StoragePolicy) DerivedStore() string {
	return p.derivedStore
}
