package serieskey

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/testutil"
)

// extraInputs は境界の表に無い、Postgres と照合する入力である。
// 全角空白・結合文字・絵文字は Go のエスケープで書く。
var extraInputs = []string{
	"\u3000",
	"番組\u3000\u3000第3話",
	"[x]\u05b0",
	"a\u05b0",
	"\t番組",
	"番組\t続き",
	"\U0001F3AC映画",
	"\U0001F600",
	"ＡＢ\u3000ＣＤ",
	"「[字幕]」番組",
	"番組 第3話 第4話",
}

// TestSeriesKeyMatchesPostgres は、境界の表と extraInputs の各入力で、
// Go の値が SELECT series_key($1) と一致することを確かめる。NULL どうしも一致とみなす。
func TestSeriesKeyMatchesPostgres(t *testing.T) {
	pool := testutil.SetupDB(t)
	inputs := append([]string{}, extraInputs...)
	for _, c := range cases {
		inputs = append(inputs, c.in)
	}
	for _, in := range inputs {
		var pg *string
		if err := pool.QueryRow(context.Background(), "SELECT series_key($1)", in).Scan(&pg); err != nil {
			t.Fatalf("series_key(%q): %v", in, err)
		}
		got, ok := SeriesKey(in)
		if ok != (pg != nil) || (ok && got != *pg) {
			want := "NULL"
			if pg != nil {
				want = *pg
			}
			t.Errorf("SeriesKey(%q) = (%q, %v), Postgres = %s", in, got, ok, want)
		}
	}
}
