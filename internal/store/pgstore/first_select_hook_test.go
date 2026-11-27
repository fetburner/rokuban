package pgstore

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type firstSelectKey struct{}

// firstSelectTracer は、接続の最初の SELECT の完了で fn を 1 回だけ呼ぶ pgx の tracer である。
// 読み取りの tx の中で、スナップショットが決まった後に割り込ませるために使う。
type firstSelectTracer struct {
	once sync.Once
	fn   func()
}

func (tr *firstSelectTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, firstSelectKey{}, isFirstSelectSQL(data.SQL))
}

func (tr *firstSelectTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if selected, _ := ctx.Value(firstSelectKey{}).(bool); selected {
		tr.once.Do(tr.fn)
	}
}

// newFirstSelectPool は pool と同じ DB に、最初の SELECT の完了で afterFirstSelect を 1 回だけ呼ぶプールを開く。
// プールはテスト終了時に閉じる。
func newFirstSelectPool(t *testing.T, pool *pgxpool.Pool, afterFirstSelect func()) *pgxpool.Pool {
	t.Helper()
	config := pool.Config()
	config.ConnConfig.Tracer = &firstSelectTracer{fn: afterFirstSelect}
	hooked, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("creating hooked database pool: %v", err)
	}
	t.Cleanup(hooked.Close)
	return hooked
}

// isFirstSelectSQL は問い合わせの先頭の文が SELECT なら true を返す。先頭の `--` のコメントと空白は飛ばす。
func isFirstSelectSQL(query string) bool {
	s := strings.TrimSpace(query)
	for strings.HasPrefix(s, "--") {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return false
		}
		s = strings.TrimSpace(s[i+1:])
	}
	word := s
	if i := strings.IndexAny(s, " \t\r\n("); i >= 0 {
		word = s[:i]
	}
	return strings.EqualFold(word, "SELECT")
}
