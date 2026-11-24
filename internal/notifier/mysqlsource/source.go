// Package mysqlsource implements notifier's MySQL source. MySQL has no LISTEN/NOTIFY,
// so it re-reads the notifications table and sends the rows it has not sent yet.
package mysqlsource

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	// lookback は通知表を読み直す遡る時間。これより前に created_at が付いた行は届かない。
	// 長いトランザクションの通知を落とす境界にもなる（通知はヒントなので許容する）。
	lookback = 30 * time.Second

	// pollInterval は通知表を読み直す間隔。読み直しの負荷は notifier レプリカ数と
	// この間隔で決まり、書き込み量には比例しない。
	pollInterval = time.Second
)

// Source は notifications 表をポーリングして payload を out へ送る。
//
// 遡る時間と間隔は設定キーにしない。保持期間・遡る時間・間隔は、この順に大きさの関係で
// 結ばれていて、設定できると外れた組み合わせで通知が黙って落ちるためである。
type Source struct {
	db *sql.DB
}

// New は MySQL の notifications 表を読む Source を生成する。
func New(db *sql.DB) *Source {
	return &Source{db: db}
}

// Listen は起動時に遡る時間の中の既存の行を既読にしてから ready を呼び、
// 以後は pollInterval ごとに未送信の行を out へ送る。
// 次の読み直しの「送った id」は、今回読んだ遡る時間の中の行の集合で置き換える。
// 遡る時間を過ぎた行は読まれなくなるので、そのとき自然に忘れられる。
func (s *Source) Listen(ctx context.Context, ready func(), out chan<- string) error {
	sent, err := s.poll(ctx, nil, nil)
	if err != nil {
		return err
	}
	ready()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		sent, err = s.poll(ctx, sent, out)
		if err != nil {
			return err
		}
	}
}

// poll は遡る時間の中の行を読み、sent に無い id を out へ送る（out が nil なら送らない）。
// 返すのは今回読んだ id の集合で、次回の sent になる。
func (s *Source) poll(ctx context.Context, sent map[int64]struct{}, out chan<- string) (map[int64]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, payload FROM notifications
		WHERE created_at >= NOW(6) - INTERVAL ? MICROSECOND
		ORDER BY id
	`, lookback.Microseconds())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("reading notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	next := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scanning notification: %w", err)
		}
		next[id] = struct{}{}
		if _, ok := sent[id]; ok || out == nil {
			continue
		}
		select {
		case out <- payload:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating notifications: %w", err)
	}
	return next, nil
}
