package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestRecordingChapterSpansConstraints は「あってはいけない組み合わせ」を DB が
// 拒否することを確かめる。アプリ側の Validate と二重に持つのは、素の INSERT が
// 直接来ても壊れないようにするため。
func TestRecordingChapterSpansConstraints(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recordingID, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           2111,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "チャプター制約",
		ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("creating recording: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}

	insert := func(startMs, endMs int64, label *string, cut bool) error {
		_, err := pool.Exec(ctx, `
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES ($1, int8range($2, $3), $4, $5)`, recordingID, startMs, endMs, label, cut)
		return err
	}

	cutLabel := "CM"
	if err := insert(0, 1000, &cutLabel, true); err != nil {
		t.Fatalf("inserting a valid span: %v", err)
	}

	// 重なる区間。どの区間がその時刻を占めるか決められない。
	if err := insert(500, 1500, &cutLabel, true); !isConstraintViolation(err, "23P01") {
		t.Fatalf("overlapping span error = %v, want exclusion_violation", err)
	}
	// 空の区間。
	if err := insert(5000, 5000, &cutLabel, true); !isConstraintViolation(err, "23514") {
		t.Fatalf("empty span error = %v, want check_violation", err)
	}
	// ラベルも無く cut でもない = 意味を持たない行（本編は行を持たない）。
	if err := insert(5000, 6000, nil, false); !isConstraintViolation(err, "23514") {
		t.Fatalf("label-less main span error = %v, want check_violation", err)
	}
	// ラベルが無くても cut なら有効。
	if err := insert(5000, 6000, nil, true); err != nil {
		t.Fatalf("inserting a label-less cut span: %v", err)
	}
	// ラベルがあれば cut=false でも有効（OP / ED を切らずに印だけ付ける）。
	opLabel := "OP"
	if err := insert(7000, 8000, &opLabel, false); err != nil {
		t.Fatalf("inserting a labelled main span: %v", err)
	}

	// 所有の行を消すと区間も一緒に落ちる。
	if _, err := pool.Exec(ctx,
		`DELETE FROM recording_chapter_ownership WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatalf("resetting chapters: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_chapter_spans WHERE recording_id = $1`, recordingID).Scan(&remaining); err != nil {
		t.Fatalf("counting spans: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("spans after reset = %d, want 0 (ownership delete must cascade)", remaining)
	}
}

// TestRecordingChapterSpansRequireOwnership は、所有の行が無ければ区間を書けない
// ことを確かめる。所有の行の存在そのものが「ユーザーが確認済み」の主張なので、
// 区間だけが浮いた状態を作れない。
func TestRecordingChapterSpansRequireOwnership(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recordingID, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           2112,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "所有の行が要る",
		ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("creating recording: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES ($1, int8range(0, 1000), 'CM', true)`, recordingID); !isConstraintViolation(err, "23503") {
		t.Fatalf("span without ownership error = %v, want foreign_key_violation", err)
	}
}

// isConstraintViolation は err が code の SQLSTATE を持つかを返す。
func isConstraintViolation(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
