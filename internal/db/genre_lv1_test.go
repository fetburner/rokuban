package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestGenreLv1_EdgeCasesDoNotFailInsert は recordings.genre_lv1
// （00034_recordings_search.sql の生成列、issue #136）の受け入れ基準
// 「genres が null / [] / 配列でない / lv1 が数値でない行でも INSERT が落ちない」を
// 実 INSERT で固定する。
//
// PR #187 のレビューで、桁数を見ない正規表現チェック（`^[0-9]+$`）だけでは
// smallint の範囲外の lv1（40000 等、さらに int4 の範囲すら超える巨大な整数）が
// genre_lv1_of 内部の `::smallint` キャストで例外を投げ、INSERT ごと失敗する
// （watcher の CreateRecording / CreateFailedRecording が失敗 = 録画行が
// 作られない不可逆な事実の喪失）ことが分かった。genre_lv1_of に
// `(e->>'lv1')::numeric BETWEEN 0 AND 15`（rule_genres.genre_lv1 の CHECK と同じ
// ドメイン、00006_rules.sql）の範囲チェックを足して閉じた後もこの回帰が
// 起きないことをここで固定する。
func TestGenreLv1_EdgeCasesDoNotFailInsert(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	cases := []struct {
		name   string
		genres []byte // nil means SQL NULL
		want   []int16
	}{
		{"nil (SQL NULL)", nil, nil},
		{"empty array", []byte(`[]`), nil},
		{"jsonb null literal", []byte(`null`), nil},
		{"object, not array", []byte(`{"lv1":1}`), nil},
		{"bare string", []byte(`"foo"`), nil},
		{"bare number", []byte(`123`), nil},
		{"array of bare numbers", []byte(`[1,2,3]`), nil},
		{"lv1 is a string", []byte(`[{"lv1":"x"}]`), nil},
		{"lv1 negative", []byte(`[{"lv1":-1}]`), nil},
		{"lv1 out of smallint range", []byte(`[{"lv1":40000}]`), nil},
		{"lv1 out of int4 range too", []byte(`[{"lv1":99999999999999999999}]`), nil},
		{
			"valid values dedup, out-of-range entries dropped",
			[]byte(`[{"lv1":7},{"lv1":2},{"lv1":7},{"lv1":99999},{"lv1":-1}]`),
			[]int16{2, 7},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var genres json.RawMessage
			if tc.genres != nil {
				genres = json.RawMessage(tc.genres)
			}
			id, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              DefaultSite,
				NetworkID:         1,
				ServiceID:         1,
				EventID:           int32(1000 + i),
				ServiceName:       "test",
				ChannelType:       "GR",
				Channel:           "1",
				Title:             tc.name,
				Genres:            genres,
				ProgramStartAt:    time.Now(),
				ProgramDurationMs: 1000,
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("CreateRecording with genres=%s must not fail, got: %v", tc.genres, err)
			}

			var got []int16
			if err := pool.QueryRow(ctx,
				"SELECT genre_lv1 FROM recordings WHERE id = $1", id,
			).Scan(&got); err != nil {
				t.Fatalf("querying genre_lv1: %v", err)
			}
			if !equalInt16Slices(got, tc.want) {
				t.Errorf("genre_lv1 = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalInt16Slices(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
