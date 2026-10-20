package rulequery

// KeywordClause は列 col への部分一致（LIKE '%value%'）を組む。
// キーワード中の LIKE 特殊文字（\、%、_）は文字どおり検索する。
//
// pgstore の compileTextMatch と録画検索（internal/api の recordings_query.go）が
// 共有する。共有するのはこの正規化方言だけで、テーブルや列名マップは
// 持たない --- 呼び出し側が列名（epg_programs なら "p.name"、recordings なら
// "r.title" 等）を渡す（docs/data/search.md §5「録画検索は rulequery を共有しない」）。
//
// caseSensitive が false なら normalize_search_text（全角→半角 + lower）を
// 両辺にかけて全角/半角の揺れを吸収する。pg_trgm の式 GIN が
// normalize_search_text(col) に乗っていれば、この形のまま加速される。
//
// エスケープは SQL 側の like_escape に一本化する。分類ルールの当たりは DB の
// トリガーと全件再評価のジョブが評価するので、Go 側でエスケープする経路を通らない。
// 4 経路（ruler・EPG 検索・録画一覧・分類ルール）が同じ関数を通ることで、
// 「検索では出るのに分類ルールが当たらない」を構造的に防ぐ（docs/data/series.md §8）。
//
// 録画検索（api）が呼ぶ間はここに置く。api は pgstore を import できない
// （depguard の api-role）ためである。録画検索を pgstore へ移すときに、この関数も
// pgstore の番組検索の隣へ移して消す。
func KeywordClause(col string, value string, caseSensitive bool, arg func(any) string) string {
	if caseSensitive {
		return col + " LIKE ('%' || like_escape(" + arg(value) + ") || '%') ESCAPE '\\'"
	}
	normCol := "normalize_search_text(" + col + ")"
	return normCol + " LIKE ('%' || normalize_search_text(like_escape(" + arg(value) + ")) || '%') ESCAPE '\\'"
}
