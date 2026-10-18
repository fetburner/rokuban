// Package searchsql contains PostgreSQL search predicates shared across query owners.
package searchsql

// KeywordClause は PostgreSQL のキーワード検索述語を組み立てる。
//
// 列名と bind 引数を追加する関数は呼び出し側が渡す。
func KeywordClause(column, value string, caseSensitive bool, arg func(any) string) string {
	if caseSensitive {
		return column + " LIKE ('%' || like_escape(" + arg(value) + ") || '%') ESCAPE '\\'"
	}
	normalizedColumn := "normalize_search_text(" + column + ")"
	return normalizedColumn + " LIKE ('%' || normalize_search_text(like_escape(" + arg(value) + ")) || '%') ESCAPE '\\'"
}
