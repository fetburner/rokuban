package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRescueFile_MalformedSnapshotIsSkippedAndAssetsSurvive は「壊れた 1 行で
// 災害復旧そのものを止めない」ことを固定する。
//
// catalog は DB より寿命の長いバックアップファイルで、手で編集された・書き込みが
// 途中で切れた・export にバグがある、といった経路で識別子を欠く行が混ざりうる。
// `program_snapshots.channel_type` には列挙の CHECK があるので、そのまま INSERT
// するとトランザクションごとロールバックし、**同じダンプに入っている recordings も
// 1 件も復元されない**。rescue が守るべきものは永続資産（recordings /
// media_assets / drop_stats / rules）なので、導出テーブルの 1 行のためにそれを
// 失ってはならない。
//
// この分岐を消す（validSnapshotIdentity を常に true にする）と、下の
// 「録画が 1 件復元されている」で落ちる。
func TestRescueFile_MalformedSnapshotIsSkippedAndAssetsSurvive(t *testing.T) {
	pool := testutil.SetupDB(t)
	dir := t.TempDir()

	doc := Document{
		Version:    Version,
		ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default",
			NetworkID: 32736, ServiceID: 1024, EventID: 1,
			ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
			Title: "救うべき録画", ProgramStartAt: fixedTime(), ProgramDurationMs: 1800000,
			Status: "finished", QualityEvents: json.RawMessage("[]"),
			CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
		ProgramSnapshots: []ProgramSnapshot{
			// DB が拒否する行（channel_type が CHECK の集合に無い）。
			{Site: "default", ProgramID: 123456, Title: "壊れた行",
				StartAt: fixedTime(), DurationMs: 1800000, UpdatedAt: fixedTime()},
			// **サービス名が空でも落としてはならない。** SDT が名前を持たない
			// 構成は実在し、`epg_services.name` にも `program_snapshots` にも
			// 非空制約は無い。ここを落とすと、この行に紐づく program_intents
			// （ユーザーが明示した「録れ / 録るな」）まで巻き添えで消える。
			{Site: "default", ProgramID: 777777, Title: "名前の無いサービス",
				StartAt: fixedTime(), DurationMs: 1800000,
				NetworkID: 32736, ServiceID: 1024, EventID: 3,
				ChannelType: "GR", Channel: "27", ServiceName: "",
				UpdatedAt: fixedTime()},
			// 正常な行。壊れた行の巻き添えにならないこと。
			{Site: "default", ProgramID: 654321, Title: "正常な行",
				StartAt: fixedTime(), DurationMs: 1800000,
				NetworkID: 32736, ServiceID: 1024, EventID: 2,
				ChannelType: "GR", Channel: "27", ServiceName: "NHK総合",
				UpdatedAt: fixedTime()},
		},
		// 壊れた snapshot を参照する intent。FK 違反でトランザクションを
		// 壊さないよう連動して落ちること。
		ProgramIntents: []ProgramIntent{
			// 落とす snapshot を参照する intent（連動して落ちる）。
			{Site: "default", ProgramID: 123456, Action: "record",
				CreatedAt: fixedTime(), UpdatedAt: fixedTime()},
			// 名前が空の snapshot を参照する intent（残らなければならない）。
			{Site: "default", ProgramID: 777777, Action: "skip",
				CreatedAt: fixedTime(), UpdatedAt: fixedTime()},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RescueFile(context.Background(), pool, path)
	if err != nil {
		t.Fatalf("RescueFile: %v（壊れた 1 行で rescue 全体を止めてはならない）", err)
	}
	if res.Recordings != 1 {
		t.Errorf("recordings = %d, want 1（永続資産は壊れた snapshot の巻き添えにしない）", res.Recordings)
	}
	if res.ProgramSnapshots != 2 {
		t.Errorf("program_snapshots = %d, want 2（正常な行とサービス名が空の行は復元する）",
			res.ProgramSnapshots)
	}
	if res.SkippedProgramSnapshots != 1 {
		t.Errorf("skipped snapshots = %d, want 1（黙って切り捨てず数える）", res.SkippedProgramSnapshots)
	}
	if res.SkippedProgramIntents != 1 {
		t.Errorf("skipped intents = %d, want 1（FK 先を失う行だけ連動して落とす）", res.SkippedProgramIntents)
	}
	if res.ProgramIntents != 1 {
		t.Errorf("program_intents = %d, want 1（サービス名が空でもユーザーの意図は捨てない）",
			res.ProgramIntents)
	}
}

func fixedTime() time.Time {
	return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
}
