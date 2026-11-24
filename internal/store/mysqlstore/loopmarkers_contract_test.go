package mysqlstore

import (
	"context"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestLoopMarkersContract(t *testing.T) {
	contracttest.RunLoopMarkers(t, func(t *testing.T) contracttest.LoopMarkerFixture {
		db := mysqltest.SetupDB(t)
		return contracttest.LoopMarkerFixture{
			Store: NewLoopMarkers(db),
			FailMarkFor: func(t *testing.T, loop, scope string) {
				// 失敗の形は書き込みの側に作る。読み取りには影響しない。
				// MySQL のトリガーは引数を取れないので、対象の loop と scope をリテラルで埋める。
				cond := "NEW.loop = " + mysqlLiteral(loop) + " AND NEW.scope = " + mysqlLiteral(scope)
				for _, timing := range []string{"INSERT", "UPDATE"} {
					name := "reject_loop_marker_" + strings.ToLower(timing)
					if _, err := db.ExecContext(context.Background(), `
						CREATE TRIGGER `+name+` BEFORE `+timing+` ON loop_success_markers FOR EACH ROW
						BEGIN
							IF `+cond+` THEN
								SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'loop marker write rejected';
							END IF;
						END
					`); err != nil {
						t.Fatalf("creating loop marker %s trigger: %v", timing, err)
					}
					t.Cleanup(func() {
						if _, err := db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS "+name); err != nil {
							t.Errorf("dropping loop marker %s trigger: %v", timing, err)
						}
					})
				}
			},
		}
	})
}

// mysqlLiteral は MySQL の文字列リテラルを作る。NO_BACKSLASH_ESCAPES が無いので \ も escape する。
func mysqlLiteral(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v) + "'"
}
