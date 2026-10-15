package db

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
)

// TestKeepRangesRoundTripThroughPostgres は書き込み（KeepRangesParam）と読み出し
// （KeepRangesFromMultirange）が PostgreSQL の int8multirange を挟んで往復し、
// 空の区間を捨て、並び替えと隣接区間の結合を DB の正規化に任せられることを固定する。
func TestKeepRangesRoundTripThroughPostgres(t *testing.T) {
	pool := setupTestDB(t)
	param, err := KeepRangesParam([]chapters.Range{
		{StartMs: 5000, EndMs: 6000},
		{StartMs: 1000, EndMs: 1000}, // 空。捨てる
		{StartMs: 0, EndMs: 1000},
		{StartMs: 1000, EndMs: 2000}, // 直前と隣接。DB が結合する
	})
	if err != nil {
		t.Fatalf("KeepRangesParam: %v", err)
	}
	var text string
	var back KeepRanges
	if err := pool.QueryRow(context.Background(),
		`SELECT $1::int8multirange::text, $1::int8multirange`, param).Scan(&text, &back); err != nil {
		t.Fatalf("round-tripping keep ranges: %v", err)
	}
	if text != "{[0,2000),[5000,6000)}" {
		t.Errorf("stored multirange = %s, want {[0,2000),[5000,6000)}", text)
	}
	got, err := KeepRangesFromMultirange(back)
	if err != nil {
		t.Fatalf("KeepRangesFromMultirange: %v", err)
	}
	want := []chapters.Range{{StartMs: 0, EndMs: 2000}, {StartMs: 5000, EndMs: 6000}}
	if !chapters.SameRanges(got, want) {
		t.Errorf("decoded keep ranges = %v, want %v", got, want)
	}
}

func TestKeepRangesParamRejectsEmpty(t *testing.T) {
	if _, err := KeepRangesParam([]chapters.Range{{StartMs: 10, EndMs: 10}}); err == nil {
		t.Error("KeepRangesParam(only empty ranges) error = nil, want error")
	}
}

func TestKeepRangesFromMultirangeRejectsUnboundedRange(t *testing.T) {
	pool := setupTestDB(t)
	var mr KeepRanges
	if err := pool.QueryRow(context.Background(),
		`SELECT '{[0,)}'::int8multirange`).Scan(&mr); err != nil {
		t.Fatalf("scanning unbounded multirange: %v", err)
	}
	if got, err := KeepRangesFromMultirange(mr); err == nil {
		t.Errorf("KeepRangesFromMultirange(unbounded) = %v, want error", got)
	}
}
