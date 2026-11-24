// Package serieskey は、Postgres の SQL 関数 series_key() と同じ値を Go で求める。
//
// MySQL では関数を生成列に写せないため、series_key 列はアプリが書く（docs/data/series.md §8）。
// 値の定義は internal/db/schema.sql の series_key() が権威であり、このパッケージは
// その各段を同じ順に写す。MySQL のドライバにも pgx にも依存しない。
// 両者の一致は、同じ入力について Postgres と照合するテストで確かめる。
package serieskey

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/fetburner/rokuban/internal/trgm"
)

var (
	squareBracket = regexp.MustCompile(`\[[^\]]*\]`)
	cornerBracket = regexp.MustCompile(`【[^】]*】`)
	// episode は話数の表記を消す。Postgres の regexp_replace と同じく全件を消す。
	episode = regexp.MustCompile(`第[0-9]+[話回週]|#[0-9]+|\([0-9]+\)|[0-9]+話`)
	// frame は枠名（アニメ・TVアニメ・日5）と、その後の空白を 1 回だけ消す。
	frame = regexp.MustCompile(`^(アニメ|TVアニメ|日5) +`)
)

const (
	// leadingSymbols は題名の先頭から取り除く記号の集合。
	leadingSymbols = "▽◇★☆〜「『◆■●○◎※#|"
	// cutSymbols は、ここで題名を切る記号の集合。leadingSymbols に 』」 を足したもの。
	cutSymbols = "▽◇★☆〜「『』」◆■●○◎※#|"
	// trimChars は両端から落とす文字の集合（Postgres の btrim の第 2 引数）。
	trimChars = " 　、,・.。:：;；!！?？"
)

// SeriesKey は題名 title から series_key を求める。
//
// ok が false のときは SQL の NULL に当たる。英数字を一つも含まない題名がこれになる。
// 入力は有効な UTF-8 を前提とする。
//
// NFKC は Go の x/text の表で行う。postgres:17.12 と照合した結果、版が違い値の出る
// 37 符号位置（U+A7F1 と U+1CCD6〜U+1CCF9）を題名に含むと、値が Postgres と異なる。
func SeriesKey(title string) (key string, ok bool) {
	t := norm.NFKC.String(title)
	t = squareBracket.ReplaceAllString(t, "")
	t = cornerBracket.ReplaceAllString(t, "")
	t = strings.TrimLeft(t, leadingSymbols)
	if i := strings.IndexAny(t, cutSymbols); i >= 0 {
		t = t[:i]
	}
	t = episode.ReplaceAllString(t, "")
	t = frame.ReplaceAllString(t, "")
	// 最初の ASCII 空白で切る。全角空白は NFKC で ASCII 空白になっている。
	if i := strings.IndexByte(t, ' '); i >= 0 {
		t = t[:i]
	}
	if !strings.ContainsFunc(t, trgm.IsAlnum) {
		return "", false
	}
	return strings.Trim(t, trimChars), true
}
