package pgstore

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestKeepRangesRoundTripThroughPostgres(t *testing.T) {
	pool := testutil.SetupDB(t)
	param, err := KeepRangesParam([]chapters.Range{
		{StartMs: 5000, EndMs: 6000},
		{StartMs: 1000, EndMs: 1000},
		{StartMs: 0, EndMs: 1000},
		{StartMs: 1000, EndMs: 2000},
	})
	if err != nil {
		t.Fatalf("KeepRangesParam: %v", err)
	}
	var text string
	var back KeepRanges
	if err := pool.QueryRow(context.Background(), `SELECT $1::int8multirange::text, $1::int8multirange`, param).Scan(&text, &back); err != nil {
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
	pool := testutil.SetupDB(t)
	var mr KeepRanges
	if err := pool.QueryRow(context.Background(), `SELECT '{[0,)}'::int8multirange`).Scan(&mr); err != nil {
		t.Fatalf("scanning unbounded multirange: %v", err)
	}
	if got, err := KeepRangesFromMultirange(mr); err == nil {
		t.Errorf("KeepRangesFromMultirange(unbounded) = %v, want error", got)
	}
}
