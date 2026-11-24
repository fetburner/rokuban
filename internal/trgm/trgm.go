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
// 測定方法: Postgres 17（公式イメージ、LC_CTYPE=en_US.utf8）で正規表現 [[:alnum:]] を
// 全 codepoint について走査した。結果、Postgres だけが英数字とする codepoint は 0、
// この判定だけが英数字とする codepoint は 4,672 だった。後者の範囲は 088F・0C5C・A7CE-A7CF・
// 10940-10959・16EA0-16EB8・1E6C0-1E6DE・323B0-33479 など、新しい Unicode 版で割り当てられた
// と見られる文字である（個々の割り当て版は未確認）。英数字の判定は DB の glibc の版に依存し、
// その差は このパッケージでは埋めない。
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
// 3 文字がすべて 1 バイトの文字なら、そのバイト列を trigram の鍵とし、
// 1 つでもマルチバイトを含めば、3 文字の UTF-8 バイト列の legacy CRC-32 の
// 下位 3 バイトを鍵とする（trigramKey）。
//
// 鍵の式は pg_trgm のドキュメントに無い実装の詳細である。類似度に効くのは鍵の衝突の有無
// だけなので、鍵の式が違っても大半の対は一致する。差が出るのは、多バイトの trigram と
// ASCII の 3 文字の trigram が同じ鍵になる組だけである（例: similarity('nn8', '丞') は
// Postgres で 0.2 になるが、多バイト側を標準の crc32.ChecksumIEEE で鍵にすると 0 になる）。
// 照合テストは Postgres の版を上げたときのずれを検出する。
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

// trigramSet は s の trigram の鍵を重複なしの集合として返す。
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

// trigramKey は 3 文字の trigram の鍵を返す。鍵は 24 ビットで、比較は等価判定だけに使う。
//
// 3 文字がすべて ASCII なら、バイトを先頭から下位に詰める（r0 | r1<<8 | r2<<16）。
// マルチバイトを含むなら、3 文字の UTF-8 バイト列の legacy CRC-32 の下位 3 バイトを
// 同じ向きで詰める。これは pg_trgm の compact_trigram が使う式である。
func trigramKey(r0, r1, r2 rune) uint32 {
	if r0 < utf8.RuneSelf && r1 < utf8.RuneSelf && r2 < utf8.RuneSelf {
		return uint32(r0) | uint32(r1)<<8 | uint32(r2)<<16
	}
	var buf [12]byte
	n := 0
	n += utf8.EncodeRune(buf[n:], r0)
	n += utf8.EncodeRune(buf[n:], r1)
	n += utf8.EncodeRune(buf[n:], r2)
	return legacyCRC32(buf[:n]) & 0xFFFFFF
}

// legacyCRC32 は Postgres の legacy CRC-32（pg_crc.h の INIT/COMP/FIN_LEGACY_CRC32）を返す。
//
// 多項式は IEEE だが、標準の crc32.ChecksumIEEE とは値が一致しない別の式である。
// 標準の関数で代用してはいけない。
func legacyCRC32(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, x := range b {
		c = crc32.IEEETable[byte(c>>24)^x] ^ (c << 8)
	}
	return c ^ 0xFFFFFFFF
}
