package reservation

import (
	"encoding/json"
	"fmt"
)

// Options は reservations.base / overrides の jsonb 構造。
// jsonb 内は camelCase（Go/JSON 規約）。
// EncodeProfiles は *[]string: nil=未指定、&[]string{}=エンコードなし override。
//
// FilenameTemplate は rules.filename_template（ruler が base に載せる）または
// ユーザーの明示的な上書き（program_overrides.overrides）由来の Go text/template
// テンプレート文字列。reconciler が予約行のスナップショットだけから
// internal/contentpath で展開する（docs/recording.md §3.2）。ルール作成/更新時に
// internal/contentpath.Validate で構文・実行時エラーを検証し 400 で弾く
// （internal/api/rules.go の validateRuleInput）。ContentPath（フルパスの直接
// 指定）とは別物で、両方指定された場合は ContentPath が勝つ
// （reconciler.createSchedule 参照）。
type Options struct {
	Skip             *bool     `json:"skip,omitempty"`
	Priority         *int      `json:"priority,omitempty"`
	ContentPath      *string   `json:"contentPath,omitempty"`
	FilenameTemplate *string   `json:"filenameTemplate,omitempty"`
	EncodeProfiles   *[]string `json:"encodeProfiles,omitempty"`
	KeepOriginal     *string   `json:"keepOriginal,omitempty"`
}

// IsSkipped は実効の skip 判定（`opts.Skip != nil && *opts.Skip`）に名前を付けたもの。
//
// この 1 行は EffectiveOptions の呼び出し元 5 箇所（internal/reconciler,
// internal/capacity, internal/api/reservations_overlaps.go, internal/api/handler.go,
// cmd/rokuban/shadowdiff.go）がそれぞれ書き下していた。式そのものは単純だが、
// 「skip か」という判断が名前を持たないまま散らばっていたことが issue #54 の
// 見逃し（クエリ名が絞り込み済みだと嘘をつき、shadow-diff がこの判定を書き忘れた）
// の土壌になった。
func (o Options) IsSkipped() bool {
	return o.Skip != nil && *o.Skip
}

// Effective は base に overrides をマージした結果を返す。
func (o *Options) Effective(base *Options) Options {
	if base == nil {
		if o == nil {
			return Options{}
		}
		return *o
	}
	eff := *base
	if o == nil {
		eff.EncodeProfiles = cloneStringSlicePtr(eff.EncodeProfiles)
		return eff
	}
	if o.Skip != nil {
		eff.Skip = o.Skip
	}
	if o.Priority != nil {
		eff.Priority = o.Priority
	}
	if o.ContentPath != nil {
		eff.ContentPath = o.ContentPath
	}
	if o.FilenameTemplate != nil {
		eff.FilenameTemplate = o.FilenameTemplate
	}
	if o.EncodeProfiles != nil {
		eff.EncodeProfiles = o.EncodeProfiles
	}
	eff.EncodeProfiles = cloneStringSlicePtr(eff.EncodeProfiles)
	if o.KeepOriginal != nil {
		eff.KeepOriginal = o.KeepOriginal
	}
	return eff
}

// EffectiveOptions は base（ruler の導出結果）と overrides（program_overrides の
// ユーザー上書き）と intentAction（program_intents.action）から実効オプションを
// 組む。予約行・program_overrides 行の 2 つの jsonb を扱う箇所はすべてここを
// 通し、Unmarshal の失敗を握りつぶさない。
//
// skip の解決は docs/recording.md §4.2 の式に従う:
//
//	effective.skip = (action = 'skip') OR (意図がなく base.skip)
//
// つまり**意図があれば action だけが skip を決める**。action = 'record' なら
// base.skip が true でも false を返す --- M2-6 の重複排除が base に skip を
// 立てても、ユーザーの「録れ」意図が勝つ（同 §4.2「dedup skip（重複排除）」）。
// 意図が無いときだけ base / overrides 由来の skip がそのまま効く。
//
// action は overrides とは別表（program_intents）にあるので base 側の skip を
// 上書きする形になり、jsonb マージに細工を仕込む必要がない。
func EffectiveOptions(base, overrides []byte, intentAction *string) (Options, error) {
	var b *Options
	if len(base) > 0 {
		var v Options
		if err := json.Unmarshal(base, &v); err != nil {
			return Options{}, fmt.Errorf("unmarshalling base: %w", err)
		}
		b = &v
	}

	var o *Options
	if len(overrides) > 0 {
		var v Options
		if err := json.Unmarshal(overrides, &v); err != nil {
			return Options{}, fmt.Errorf("unmarshalling overrides: %w", err)
		}
		o = &v
	}

	eff := o.Effective(b)
	// 意図があれば action が skip を決め切る（record なら false で上書きする）。
	// ここを *intentAction == IntentSkip のときだけ true を書く形にすると、
	// action='record' かつ base.skip=true の予約が skip されたままになる。
	if intentAction != nil {
		skip := *intentAction == IntentSkip
		eff.Skip = &skip
	}
	return eff, nil
}

// IntentRecord は「録れ」。手動予約、およびルール由来予約への上書き。
const IntentRecord = "record"

// IntentSkip は「録るな」。どのルール経由でも一貫して除外される。
const IntentSkip = "skip"

// KeepOriginalAlways は原本を常に保持するポリシー。
const KeepOriginalAlways = "always"

// KeepOriginalUntilEncoded はエンコード済みアセットが揃うまで原本を保持するポリシー。
const KeepOriginalUntilEncoded = "until_encoded"

func cloneStringSlicePtr(p *[]string) *[]string {
	if p == nil {
		return nil
	}
	c := make([]string, len(*p))
	copy(c, *p)
	return &c
}
