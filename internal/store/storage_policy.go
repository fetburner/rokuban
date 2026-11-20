package store

import "time"

// StoragePolicy は置き場の方針の値である。原本の置き場の規則、FS 置き場の集合、派生物の置き場の 3 つからなる。
//
// 零値は「方針なし」である。規則が空なので原本の置き場はどれも決まらず、どのコピーにも望ましい置き場が無い。
// そのため零値を渡された削除の判定は何も削除しない。削除の判定をしない呼び出しも、規則が無い扱いで零値を渡せる。
type StoragePolicy struct {
	// Originals は原本の置き場を先勝ちで選ぶ規則である。条件の無い規則（Site も OlderThan も空）は最後に置く。
	Originals []OriginalRule
	// FSStores は設定の FS 置き場の名前の集合である。マウントの有無は含まない（マウントは worker ごとの事実で、引数で渡す）。
	FSStores []string
	// Derived は派生物の望ましい置き場である。空なら派生物の置き場は決まらない。
	Derived string
}

// OriginalRule は原本の置き場を選ぶ 1 規則である。
type OriginalRule struct {
	// Site が空なら、どの放送局の録画にも一致する。
	Site string
	// OlderThan が 0 なら、経過時間の条件は無い。0 より大きいときは、放送終了から現在までがこの時間以上なら一致する。
	OlderThan time.Duration
	// Store は一致したときに原本を置く FS 置き場の名前である。
	Store string
}
