// Package trgm は、PostgreSQL の pg_trgm の similarity() と同じ値を Go で計算する。
//
// 重複排除の類似度は、Postgres 構成では SQL の similarity() で求め、MySQL 構成
// （pg_trgm が無い）では このパッケージで求める。
// このパッケージは MySQL ドライバにも pgx にも依存しない。利用側は mysqlstore になり、
// 照合テストだけが Postgres を使うため、本体を DB ドライバから切り離しておく。
//
// 類似度を判定する理由は docs/recording/ruler.md の §重複排除にある。値は Postgres 17 の
// pg_trgm と float32 のビット単位で一致させる（trgm_pg_test.go が照合する）。
package trgm

import (
	"hash/crc32"
	"unicode"
	"unicode/utf8"
)

// IsAlnum は r を pg_trgm の語の文字（英数字）として扱うかを返す。
//
// Postgres の LC_CTYPE が glibc（libc プロバイダ）のとき、語の区切りは glibc の
// iswalnum に従う。Go の unicode.IsLetter / unicode.IsDigit だけでは一致しない。
// 文字種 Nl（例: Ⅻ）と Other_Alphabetic（一部の結合文字）が glibc では英数字なので含める。
//
// 照合（Postgres 17、LC_CTYPE=en_US.utf8）で、Postgres が英数字とする codepoint の
// 集合は、この判定の部分集合であった。差は glibc の Unicode 版より後に割り当てられた
// 文字だけで、その差は全て Go 側が多く英数字とする側にある（2026-11 時点の測定）。
// 差の仕方が変わったら照合テストで検出する。
func IsAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) ||
		unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_Alphabetic, r)
}

// Similarity は a と b の trigram 類似度を返す。pg_trgm の similarity(a, b) と同じ値である。
//
// 値は「共通の trigram 数 / (両者の trigram 数の和 - 共通数)」を float32 で割った結果で、
// どちらかの trigram 集合が空なら 0 を返す。
//
// trigram の作り方は pg_trgm に従う。語は IsAlnum で区切って小文字化し、
// 語の前に空白 2 つ、後ろに空白 1 つを付けて 3 文字ずつ切り出す。
// 3 文字がすべて 1 バイトの文字なら、そのバイト列を trigram とし、
// 1 つでもマルチバイトを含めば、3 文字の UTF-8 バイト列の CRC-32 (IEEE) の下位 3 バイトを
// trigram とする。
//
// 後者の CRC の扱いは pg_trgm のドキュメントに無い実装の詳細である。
//
// 照合の範囲: 境界の表と固定シードの乱数の対では、Postgres 17 とビット単位で一致した。
// ただし show_trgm で見た Postgres の trigram の鍵は、この CRC の下位 3 バイトとは一致しなかった。
// 多バイトの trigram と ASCII の 3 文字の trigram が同じ鍵になる組では食い違う
// （similarity('nn8', '丞') は Postgres で 0.2、この実装では 0）。
// 鍵の式を Postgres の実装から確定できていないので、その組は未解決である。
func Similarity(a, b string) float32 {
	ta := trigramSet(a)
	tb := trigramSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	// 小さい方を回して大きい方を引く。結果は対称なので順序は変えてよい。
	if len(ta) > len(tb) {
		ta, tb = tb, ta
	}
	common := 0
	for k := range ta {
		if _, ok := tb[k]; ok {
			common++
		}
	}
	return float32(common) / float32(len(ta)+len(tb)-common)
}

// trigramSet は s の trigram を重複なしの集合として返す。
func trigramSet(s string) map[uint32]struct{} {
	set := map[uint32]struct{}{}
	for _, w := range words(s) {
		// 語の前は空白 2 つ、後ろは空白 1 つ。
		padded := make([]rune, 0, len(w)+3)
		padded = append(padded, ' ', ' ')
		padded = append(padded, w...)
		padded = append(padded, ' ')
		for i := 0; i+3 <= len(padded); i++ {
			set[trigramKey(padded[i], padded[i+1], padded[i+2])] = struct{}{}
		}
	}
	return set
}

// words は s を IsAlnum の文字で区切り、各語を小文字化して返す。
func words(s string) [][]rune {
	var out [][]rune
	var cur []rune
	for _, r := range s {
		if IsAlnum(r) {
			cur = append(cur, unicode.ToLower(r))
			continue
		}
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// trigramKey は 3 文字の trigram を uint32 にする。
//
// 3 文字がすべて ASCII ならバイトをそのまま下位から詰める。マルチバイトを含むなら
// 3 文字の UTF-8 バイト列の crc32.ChecksumIEEE の下位 3 バイトを同じ順で詰める。
// どちらも 24 ビットに収まるので、比較は等価判定だけで済む。
func trigramKey(r0, r1, r2 rune) uint32 {
	if r0 < utf8.RuneSelf && r1 < utf8.RuneSelf && r2 < utf8.RuneSelf {
		return uint32(r0) | uint32(r1)<<8 | uint32(r2)<<16
	}
	var buf [12]byte
	n := 0
	n += utf8.EncodeRune(buf[n:], r0)
	n += utf8.EncodeRune(buf[n:], r1)
	n += utf8.EncodeRune(buf[n:], r2)
	return crc32.ChecksumIEEE(buf[:n]) & 0xFFFFFF
}
