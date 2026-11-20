package pgstore

import (
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/store"
)

// TestOriginalRulesJSONIsLiteral は、設定から SQL へ渡る規則の JSON のバイト列を固定する。
// 期待値は変更前の OriginalRulesJSON が同じ設定から出した値を写したものである。SQL 関数 desired_media_store が読む形が変わると、既存の規則が読めなくなる。
// 条件が無い規則（match が nil と空の両方）は match を省き、site が空なら site を省く。時間はマイクロ秒で表す。
func TestOriginalRulesJSONIsLiteral(t *testing.T) {
	storage := config.StorageConfig{
		Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "default", OlderThan: "720h"}, Store: "tokyo"},
			{Match: &config.OriginalStoreMatch{OlderThan: "30d"}, Store: "takamatsu"},
			{Match: &config.OriginalStoreMatch{OlderThan: "1.5h"}, Store: "tokyo"},
			{Match: &config.OriginalStoreMatch{Site: "default"}, Store: "takamatsu"},
			{Match: &config.OriginalStoreMatch{}, Store: "tokyo"},
			{Store: "takamatsu"},
		},
	}
	policy, err := storage.StoragePolicy()
	if err != nil {
		t.Fatal(err)
	}
	got, err := originalRulesJSON(policy)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"match":{"site":"default","older_than_us":2592000000000},"store":"tokyo"},{"match":{"older_than_us":2592000000000},"store":"takamatsu"},{"match":{"older_than_us":5400000000},"store":"tokyo"},{"match":{"site":"default"},"store":"takamatsu"},{"store":"tokyo"},{"store":"takamatsu"}]`
	if string(got) != want {
		t.Fatalf("original rules JSON = %s, want %s", got, want)
	}
}

// TestOriginalRulesJSONOfEmptyPolicyIsEmptyArray は、零値の方針が SQL の空の配列になることを固定する。
// SQL 関数は NULL も空の配列として読むので、規則が無いことは配列の空で表す。
func TestOriginalRulesJSONOfEmptyPolicyIsEmptyArray(t *testing.T) {
	got, err := originalRulesJSON(store.StoragePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "[]" {
		t.Fatalf("empty policy rules JSON = %s, want []", got)
	}
}
