package mysqltest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/mysqldb"
)

// OpenHookedPool は db と同じテスト DB に、本番と同じ接続設定（mysqldb.PoolConfig）のプールを開く。
// プールが最初に実行する SELECT（接続設定の検査の SELECT を除く）の結果を読み終えた直後に、
// afterFirstSelect を 1 回だけ同期的に呼ぶ。読み取りの tx の中で、スナップショットが決まった後に
// 別の接続から書き込むための同期点である。
//
// database/sql の引数付きの問い合わせは Prepare・Stmt の経路に落ちるので、直接の問い合わせと
// Stmt の両方で数える。プールはテスト終了時に閉じる。
func OpenHookedPool(t *testing.T, db *sql.DB, afterFirstSelect func()) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatalf("reading test database name: %v", err)
	}
	dsn, err := dsnForDatabase(TestDSN(t), name)
	if err != nil {
		t.Fatalf("building hooked pool DSN: %v", err)
	}
	cfg, err := mysqldb.PoolConfig(dsn)
	if err != nil {
		t.Fatalf("building hooked pool config: %v", err)
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatalf("creating hooked MySQL connector: %v", err)
	}
	hook := &selectHook{fn: afterFirstSelect}
	pool := sql.OpenDB(&hookConnector{inner: connector, hook: hook})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing hooked MySQL pool: %v", err)
		}
	})
	if err := mysqldb.CheckConnectionSettings(ctx, pool); err != nil {
		t.Fatalf("hooked MySQL pool does not match the production connection settings: %v", err)
	}
	hook.armed.Store(true)
	return pool
}

// selectHook は最初の SELECT の完了で fn を 1 回だけ呼ぶ。armed になる前の SELECT は数えない。
type selectHook struct {
	armed atomic.Bool
	once  sync.Once
	fn    func()
}

func (h *selectHook) selectDone() {
	if !h.armed.Load() {
		return
	}
	h.once.Do(h.fn)
}

// isSelect は問い合わせの先頭の文が SELECT なら true を返す。先頭の空白と `-- `・`/* */` のコメントは飛ばす。
func isSelect(query string) bool {
	s := strings.TrimSpace(query)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+1:])
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+2:])
		default:
			word := s
			if i := strings.IndexAny(s, " \t\r\n("); i >= 0 {
				word = s[:i]
			}
			return strings.EqualFold(word, "SELECT")
		}
	}
}

// hookConnector は mysql.NewConnector の接続を包み、接続ごとに hookConn を返す。
type hookConnector struct {
	inner driver.Connector
	hook  *selectHook
}

func (c *hookConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &hookConn{inner: conn, hook: c.hook}, nil
}

func (c *hookConnector) Driver() driver.Driver {
	return c.inner.Driver()
}

// hookConn は内側の接続の optional interface を転送する。
// 内側が実装しない interface には、ErrSkip（database/sql の既定の経路へ落とす）か既定の値で応答する。
// go-sql-driver v1.10.1 の mysqlConn は、ここで転送する interface をすべて実装しており、既定の応答は通らない。
type hookConn struct {
	inner driver.Conn
	hook  *selectHook
}

var errNoConnBeginTx = errors.New("mysqltest: inner connection does not implement driver.ConnBeginTx")

func (c *hookConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &hookStmt{inner: stmt, query: query, hook: c.hook}, nil
}

func (c *hookConn) Close() error {
	return c.inner.Close()
}

func (c *hookConn) Begin() (driver.Tx, error) {
	return c.inner.Begin() //nolint:staticcheck // driver.Conn の必須メソッドを転送する。database/sql は BeginTx を優先する。
}

func (c *hookConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.inner.(driver.ConnBeginTx)
	if !ok {
		return nil, errNoConnBeginTx
	}
	return bt.BeginTx(ctx, opts)
}

func (c *hookConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	pc, ok := c.inner.(driver.ConnPrepareContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	stmt, err := pc.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &hookStmt{inner: stmt, query: query, hook: c.hook}, nil
}

func (c *hookConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return wrapRows(rows, query, c.hook), nil
}

func (c *hookConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, query, args)
}

func (c *hookConn) Ping(ctx context.Context) error {
	p, ok := c.inner.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

func (c *hookConn) ResetSession(ctx context.Context) error {
	s, ok := c.inner.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return s.ResetSession(ctx)
}

func (c *hookConn) IsValid() bool {
	v, ok := c.inner.(driver.Validator)
	if !ok {
		return true
	}
	return v.IsValid()
}

func (c *hookConn) CheckNamedValue(nv *driver.NamedValue) error {
	n, ok := c.inner.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return n.CheckNamedValue(nv)
}

// hookStmt は内側の Stmt を包む。Stmt 経由の SELECT も最初の SELECT として数える。
type hookStmt struct {
	inner driver.Stmt
	query string
	hook  *selectHook
}

func (s *hookStmt) Close() error {
	return s.inner.Close()
}

func (s *hookStmt) NumInput() int {
	return s.inner.NumInput()
}

func (s *hookStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args) //nolint:staticcheck // driver.Stmt の必須メソッドを転送する。
}

func (s *hookStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.inner.Query(args) //nolint:staticcheck // driver.Stmt の必須メソッドを転送する。
	if err != nil {
		return nil, err
	}
	return wrapRows(rows, s.query, s.hook), nil
}

func (s *hookStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	e, ok := s.inner.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, args)
}

func (s *hookStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.inner.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return wrapRows(rows, s.query, s.hook), nil
}

func (s *hookStmt) CheckNamedValue(nv *driver.NamedValue) error {
	n, ok := s.inner.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return n.CheckNamedValue(nv)
}

// wrapRows は SELECT の結果を、閉じた後に hook へ知らせる rows で包む。SELECT 以外は包まない。
func wrapRows(rows driver.Rows, query string, hook *selectHook) driver.Rows {
	if !isSelect(query) {
		return rows
	}
	return &hookRows{inner: rows, hook: hook}
}

// hookRows は内側の結果を読み、Close の後に最初の SELECT として hook へ知らせる。
type hookRows struct {
	inner driver.Rows
	hook  *selectHook
}

func (r *hookRows) Columns() []string {
	return r.inner.Columns()
}

func (r *hookRows) Next(dest []driver.Value) error {
	return r.inner.Next(dest)
}

func (r *hookRows) Close() error {
	err := r.inner.Close()
	r.hook.selectDone()
	return err
}
