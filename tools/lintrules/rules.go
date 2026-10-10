//go:build ruleguard

// Package lintrules は gocritic の ruleguard で読む rokuban 固有の lint ルール。
//
// golangci-lint のキャッシュはこのファイルの変更で無効化されないので、ルールを変えたら
// golangci-lint cache clean してから確認する。
package lintrules

import "github.com/quasilyte/go-ruleguard/dsl"

// riverInternalTables は River の内部表を SQL で直接参照することを禁じる（不変条件 14）。
func riverInternalTables(m dsl.Matcher) {
	m.Match(
		`$db.Exec($ctx, $q, $*_)`,
		`$db.Query($ctx, $q, $*_)`,
		`$db.QueryRow($ctx, $q, $*_)`,
		`fmt.Sprintf($q, $*_)`,
		`$b.Queue($q, $*_)`,
		`const $_ = $q`,
		`var $_ = $q`,
		`const $_ $_ = $q`,
		`var $_ $_ = $q`,
		`$_ := $q`,
		`$_ = $q`,
	).
		Where(m["q"].Const && m["q"].Text.Matches(`\briver_(job|leader|queue|client|migration)\b`)).
		At(m["q"]).
		Report(`River の内部表を参照しない。公開 API（JobList / rivertest）を使う（不変条件 14）`)
}
