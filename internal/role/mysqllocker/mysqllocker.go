// Package mysqllocker は MySQL の GET_LOCK で role.Locker を実装する。database/sql だけを使い、
// MySQL ドライバは import しない（接続プールは呼び出し側が開いて渡す）。
package mysqllocker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"

	"github.com/fetburner/rokuban/internal/role"
)

// New は MySQL の接続プールからロック専用の接続を専有する role.Locker を返す。具象型は role.Locker 以外の
// メソッドを持たないので、型を公開しても使い道が無い。
// GET_LOCK はセッション単位で、同じ接続で 2 回取ると解放も 2 回要る。接続をプールに戻すと、
// 他の処理がその接続で同じ名前のロックを取り、ロックを保持したまま成功しうる。そのため
// リーダーである間は接続を返さない。
func New(db *sql.DB) role.Locker {
	return mysqlLocker{db: db}
}

type mysqlLocker struct{ db *sql.DB }

func (l mysqlLocker) Checkout(ctx context.Context) (role.Session, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring connection for leader lock: %w", err)
	}
	return &mysqlSession{conn: conn}, nil
}

// mysqlSession は専有した接続と、TryLock で決まった GET_LOCK の名前を持つ。
type mysqlSession struct {
	conn     *sql.Conn
	lockName string
}

// mysqlLockName は GET_LOCK の名前を返す。GET_LOCK の名前はサーバー全体で共有され、PostgreSQL の
// advisory lock と違って DB ごとに分かれない。そこで現在の DB 名とロール名を SHA-256 に含める。
// 同じ MySQL サーバーの別 DB のリーダーが互いを妨げないようにするためである。名前は 64 文字以内で
// なければ拒まれるので、先頭 8 文字の接頭辞と 52 桁の hex で 60 文字に収める（watcher のロール名は
// サイト名（最長 53 文字）を含み、そのままでは長すぎる）。
func mysqlLockName(database, role string) string {
	sum := sha256.Sum256([]byte("rokuban-leader\x00" + database + "\x00" + role))
	return "rokuban:" + hex.EncodeToString(sum[:26])
}

// TryLock は現在の DB 名を読み、その名前で GET_LOCK を取る。名前は後の Unlock のために保持する。
func (s *mysqlSession) TryLock(ctx context.Context, role string) (bool, error) {
	var database sql.NullString
	if err := s.conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return false, fmt.Errorf("reading current database for leader lock %s: %w", role, err)
	}
	if !database.Valid || database.String == "" {
		return false, fmt.Errorf("current database is empty for leader lock %s", role)
	}
	name := mysqlLockName(database.String, role)
	var got sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		return false, fmt.Errorf("trying GET_LOCK for %s: %w", role, err)
	}
	// 1 は取得、0 は他者が保持、NULL はエラー。
	if got.Valid && got.Int64 == 1 {
		s.lockName = name
		return true, nil
	}
	return false, nil
}

// Unlock は TryLock で取った名前を解放する。
func (s *mysqlSession) Unlock(ctx context.Context, role string) error {
	if s.lockName == "" {
		return fmt.Errorf("releasing GET_LOCK for %s: lock was not taken by this session", role)
	}
	var got sql.NullInt64
	err := s.conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", s.lockName).Scan(&got)
	if err == nil && got.Valid && got.Int64 == 1 {
		return nil
	}
	// 解放できなかったら、ロックを持つセッションを残さない。(*sql.Conn).Raw の doc comment が定める
	// とおり、f が driver.ErrBadConn を返すと、それ以降 Conn は使えず Close で接続を捨てる。
	// サーバー側のセッションが終わって GET_LOCK が解放される（自分で RELEASE_LOCK を重ねない）。
	_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil {
		return fmt.Errorf("releasing GET_LOCK for %s: %w", role, err)
	}
	return fmt.Errorf("releasing GET_LOCK for %s: lock was not held by this session", role)
}

func (s *mysqlSession) Ping(ctx context.Context) error { return s.conn.PingContext(ctx) }

func (s *mysqlSession) Release() { _ = s.conn.Close() }
