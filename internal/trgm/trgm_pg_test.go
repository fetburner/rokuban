package trgm

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestSimilarityMatchesPostgres は、Go の Similarity と Postgres の similarity() が
// float32 のビット単位で一致することを確かめる。
//
// 対象は境界の表（similarityCases）と、固定シードの乱数で作った対である。
// 乱数の対は、英数字・記号・全角・半角カナ・かな・漢字・Nl・結合文字・絵文字・CJK 拡張 B を混ぜる。
//
// 多バイトの trigram の鍵（legacy CRC-32 の下位 3 バイト）は pg_trgm のドキュメントに無い
// 実装の詳細である（不変条件 14）。Postgres 17 との照合で確かめた事実として扱う。
// Postgres の版を上げたときは、このテストがずれを検出する。
//
// DB のテストは LC_CTYPE が日本語を英数字として扱うロケール（en_US.UTF-8 など）を前提とする。
// C ロケールの DB では similarity() の値が違うので、このテストは落ちる。
// これは pg_trgm の仕様であり、rokuban の起動時検査（[docs/operations/database.md] §3）と同じ条件である。
func TestSimilarityMatchesPostgres(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	// compare は Postgres の値を問い、Go の値とビット単位で比べる。Postgres の値を返す。
	compare := func(a, b, label string) float32 {
		t.Helper()
		var pg float32
		if err := pool.QueryRow(ctx, "SELECT similarity($1::text, $2::text)", a, b).Scan(&pg); err != nil {
			t.Fatalf("%s: similarity(%q, %q): %v", label, a, b, err)
		}
		got := Similarity(a, b)
		if math.Float32bits(got) != math.Float32bits(pg) {
			t.Errorf("%s: Similarity(%q, %q) = %v (bits %08x), Postgres = %v (bits %08x)",
				label, a, b, got, math.Float32bits(got), pg, math.Float32bits(pg))
		}
		return pg
	}

	for _, tc := range similarityCases {
		pg := compare(tc.a, tc.b, tc.name)
		if math.Float32bits(pg) != math.Float32bits(tc.want) {
			t.Errorf("%s: Postgres similarity(%q, %q) = %v, but the table says %v", tc.name, tc.a, tc.b, pg, tc.want)
		}
	}

	rng := rand.New(rand.NewPCG(20260, 2140))
	// 無関係な対だけだと類似度が 0 になりやすいので、半分は一部を書き換えた対にする。
	for i := 0; i < 300; i++ {
		a := randomTitle(rng)
		b := randomTitle(rng)
		if i%2 == 0 {
			b = mutateTitle(rng, a)
		}
		compare(a, b, fmt.Sprintf("random #%d", i))
	}
}

// randomPool は乱数の対に使う文字の集まり。種類ごとに 1 組ずつ混ぜる。
var randomPool = []string{
	"abcXYZ0159",
	" -!.,",
	"ＡＢＣａｂｃ１２３",
	"アイウｱｲｳ",
	"あいうえおかきくけこ",
	"全角漢字題名宇宙戦艦",
	"Ⅻⅻ〡",
	"́ͅ",
	"éÀÉ",
	"ΑΒΓαβγ",
	"ПРИВЕТпривет",
	"\U0001F600",
	"\U00020000\U00020001",
}

// mutateTitle は s の文字を 1〜3 個、乱数で置き換えた文字列を返す。
func mutateTitle(rng *rand.Rand, s string) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return randomTitle(rng)
	}
	for k := 0; k <= rng.IntN(3); k++ {
		group := randomPool[rng.IntN(len(randomPool))]
		pool := []rune(group)
		rs[rng.IntN(len(rs))] = pool[rng.IntN(len(pool))]
	}
	return string(rs)
}

// randomTitle は乱数で題名らしい文字列を作る。文字の種類は randomPool から選ぶ。
func randomTitle(rng *rand.Rand) string {
	n := rng.IntN(16)
	var b strings.Builder
	for i := 0; i < n; i++ {
		group := randomPool[rng.IntN(len(randomPool))]
		rs := []rune(group)
		b.WriteRune(rs[rng.IntN(len(rs))])
	}
	return b.String()
}

// TestIsAlnumMatchesPostgres は、Postgres の正規表現 [[:alnum:]] が英数字とする符号位置を、
// IsAlnum がすべて英数字とすることを、U+0001〜U+10FFFF（サロゲートを除く）の全件で確かめる。
//
// 逆向き（IsAlnum だけが英数字とする符号位置）の数は固定せず t.Log で出すだけにする。
// その数は DB の glibc の版で変わるので、版が上がるたびに落ちるテストにはしない。
func TestIsAlnumMatchesPostgres(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	rows, err := pool.Query(context.Background(),
		`SELECT c FROM generate_series(1, 1114111) c WHERE c NOT BETWEEN 55296 AND 57343 AND chr(c) ~ '[[:alnum:]]'`)
	if err != nil {
		t.Fatalf("querying alnum code points: %v", err)
	}
	defer rows.Close()
	pg := map[rune]bool{}
	for rows.Next() {
		var c rune
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scanning code point: %v", err)
		}
		pg[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating code points: %v", err)
	}

	var pgOnly, goOnly int
	for r := rune(1); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		g := IsAlnum(r)
		switch {
		case pg[r] && !g:
			pgOnly++
			if pgOnly <= 10 {
				t.Errorf("U+%04X は Postgres だけが英数字とする", r)
			}
		case g && !pg[r]:
			goOnly++
		}
	}
	if pgOnly > 0 {
		t.Errorf("Postgres だけが英数字とする符号位置: %d 件", pgOnly)
	}
	t.Logf("IsAlnum だけが英数字とする符号位置: %d 件（固定しない）", goOnly)
}
