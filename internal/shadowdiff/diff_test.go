package shadowdiff_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/shadowdiff"
)

func ptr[T any](v T) *T { return &v }

func at(h int) time.Time {
	return time.Date(2026, 8, 1, h, 0, 0, 0, time.UTC)
}

//nolint:funlen // 比較規則を網羅する表駆動テスト。分割は epic #585 のスコープ外（既存超過分を一括では直さない）
func TestCompare(t *testing.T) {
	tests := []struct {
		name           string
		rokuban        []shadowdiff.RokubanReservation
		epg            []epgstation.Reserve
		wantBoth       []int64
		wantRokuban    []int64
		wantEPG        []int64
		wantExpectedN  int
		wantReasonHint string // Expected 内のどれかの Reason に含まれるべき文字列（空なら無視）
	}{
		{
			name: "全一致",
			rokuban: []shadowdiff.RokubanReservation{
				{ProgramID: 1, Title: "番組1", StartAt: at(21)},
			},
			epg: []epgstation.Reserve{
				{ID: 100, ProgramID: ptr(int64(1)), Name: "番組1", StartAt: at(21).UnixMilli()},
			},
			wantBoth:      []int64{1},
			wantExpectedN: 0,
		},
		{
			name: "Rokuban だけにある",
			rokuban: []shadowdiff.RokubanReservation{
				{ProgramID: 2, Title: "番組2", StartAt: at(22)},
			},
			epg:           nil,
			wantRokuban:   []int64{2},
			wantExpectedN: 0,
		},
		{
			name:    "EPGStation だけにある（説明なし）",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 101, ProgramID: ptr(int64(3)), Name: "番組3", StartAt: at(23).UnixMilli()},
			},
			wantEPG:       []int64{3},
			wantExpectedN: 0,
		},
		{
			name:    "IsConflict は allowlist に入らない",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 102, ProgramID: ptr(int64(4)), Name: "番組4", IsConflict: true, StartAt: at(0).UnixMilli()},
			},
			wantEPG:       []int64{4},
			wantExpectedN: 0,
		},
		{
			name:    "時刻指定予約は Expected（IsTimeSpecified）",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 103, ProgramID: nil, IsTimeSpecified: true, Name: "時刻指定", StartAt: at(1).UnixMilli()},
			},
			wantExpectedN:  1,
			wantReasonHint: "時刻指定予約",
		},
		{
			name:    "programId が無い予約も Expected",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 104, ProgramID: nil, IsTimeSpecified: false, Name: "programId無し", StartAt: at(2).UnixMilli()},
			},
			wantExpectedN:  1,
			wantReasonHint: "時刻指定予約",
		},
		{
			name:    "EPGStation の IsSkip は Expected",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 105, ProgramID: ptr(int64(5)), IsSkip: true, Name: "番組5", StartAt: at(3).UnixMilli()},
			},
			wantExpectedN:  1,
			wantReasonHint: "isSkip",
		},
		{
			name:    "EPGStation の IsOverlap は Expected",
			rokuban: nil,
			epg: []epgstation.Reserve{
				{ID: 106, ProgramID: ptr(int64(6)), IsOverlap: true, Name: "番組6", StartAt: at(4).UnixMilli()},
			},
			wantExpectedN:  1,
			wantReasonHint: "isOverlap",
		},
		{
			name: "Rokuban が Skipped なら Expected",
			rokuban: []shadowdiff.RokubanReservation{
				{ProgramID: 7, Title: "番組7", StartAt: at(5), Skipped: true},
			},
			epg: []epgstation.Reserve{
				{ID: 107, ProgramID: ptr(int64(7)), Name: "番組7", StartAt: at(5).UnixMilli()},
			},
			wantExpectedN:  1,
			wantReasonHint: "skip",
		},
		{
			name: "Rokuban が Skipped でも EPGStation 側に無ければ何も出ない",
			rokuban: []shadowdiff.RokubanReservation{
				{ProgramID: 8, Title: "番組8", StartAt: at(6), Skipped: true},
			},
			epg:           nil,
			wantExpectedN: 0,
		},
		{
			name: "複合: 一致・片方だけ・allowlist が混在",
			rokuban: []shadowdiff.RokubanReservation{
				{ProgramID: 1, Title: "番組1", StartAt: at(21)},
				{ProgramID: 2, Title: "番組2", StartAt: at(22)},
				{ProgramID: 7, Title: "番組7", StartAt: at(5), Skipped: true},
			},
			epg: []epgstation.Reserve{
				{ID: 100, ProgramID: ptr(int64(1)), Name: "番組1", StartAt: at(21).UnixMilli()},
				{ID: 101, ProgramID: ptr(int64(3)), Name: "番組3", StartAt: at(23).UnixMilli()},
				{ID: 103, ProgramID: nil, IsTimeSpecified: true, Name: "時刻指定", StartAt: at(1).UnixMilli()},
				{ID: 107, ProgramID: ptr(int64(7)), Name: "番組7", StartAt: at(5).UnixMilli()},
			},
			wantBoth:      []int64{1},
			wantRokuban:   []int64{2},
			wantEPG:       []int64{3},
			wantExpectedN: 2, // 時刻指定 + skip
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := shadowdiff.Compare(tc.rokuban, tc.epg)

			assertProgramIDs(t, "Both", report.Both, tc.wantBoth)
			assertProgramIDs(t, "RokubanOnly", report.RokubanOnly, tc.wantRokuban)
			assertProgramIDs(t, "EPGStationOnly", report.EPGStationOnly, tc.wantEPG)

			if len(report.Expected) != tc.wantExpectedN {
				t.Errorf("len(Expected) = %d, want %d (%+v)", len(report.Expected), tc.wantExpectedN, report.Expected)
			}
			for _, item := range report.Expected {
				if item.Reason == "" {
					t.Errorf("Expected item %+v has empty Reason", item)
				}
			}
			if tc.wantReasonHint != "" {
				found := false
				for _, item := range report.Expected {
					if strings.Contains(item.Reason, tc.wantReasonHint) {
						found = true
					}
				}
				if !found {
					t.Errorf("no Expected item reason contains %q: %+v", tc.wantReasonHint, report.Expected)
				}
			}

			wantUnexplained := len(tc.wantRokuban) > 0 || len(tc.wantEPG) > 0
			if report.HasUnexplained() != wantUnexplained {
				t.Errorf("HasUnexplained() = %v, want %v", report.HasUnexplained(), wantUnexplained)
			}
		})
	}
}

func assertProgramIDs(t *testing.T, label string, items []shadowdiff.Item, want []int64) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("%s: len = %d, want %d (%+v)", label, len(items), len(want), items)
	}
	for i, id := range want {
		if items[i].ProgramID != id {
			t.Errorf("%s[%d].ProgramID = %d, want %d", label, i, items[i].ProgramID, id)
		}
	}
}

func TestReport_HasUnexplained(t *testing.T) {
	if (shadowdiff.Report{}).HasUnexplained() {
		t.Error("empty report should not have unexplained diffs")
	}
	if !(shadowdiff.Report{RokubanOnly: []shadowdiff.Item{{}}}).HasUnexplained() {
		t.Error("RokubanOnly should count as unexplained")
	}
	if !(shadowdiff.Report{EPGStationOnly: []shadowdiff.Item{{}}}).HasUnexplained() {
		t.Error("EPGStationOnly should count as unexplained")
	}
	if (shadowdiff.Report{Expected: []shadowdiff.Item{{}}}).HasUnexplained() {
		t.Error("Expected alone should not count as unexplained")
	}
}
