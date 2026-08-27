package mirakc

import (
	"encoding/json"
	"testing"
	"time"
)

// 実 mirakc（Mirakurun 互換）が返す形を**リテラルの JSON で**固定する。
//
// **これは型定義とは独立した主張である。** `Program` / `Service` / `Tuner` は
// 構造体のタグでしか wire 名を持たないので、タグを rename すると、それを
// 参照するコードもテストも一緒に動いてしまい**どこも赤くならない**。
// 実際、`deploy/k8s/e2e/mirakcmock` はこれらの型をそのまま組み立てて返す
// モックなので、モック側のテストは rename に対して対称に通る（実測:
// `json:"startAt"` を `json:"start_at"` に変えても緑のままだった）。
// 一方、実機ではフィールドが埋まらず、EPG 射影の projectable() /
// validChannelTypes が全件を捨てて「番組表が空」になる。
//
// ここで見るのは wire 名 1 点だけで、値の意味は見ない（それは各利用者の
// テストが持つ）。**リテラルを型から生成しないこと** --- 生成した瞬間に
// この主張は消える。
func TestProgramWireNames(t *testing.T) {
	const raw = `{
		"id": 3273601001,
		"eventId": 1001,
		"serviceId": 1024,
		"networkId": 32736,
		"startAt": 1756000000000,
		"duration": 1800000,
		"isFree": true,
		"name": "テスト番組",
		"description": "説明",
		"genres": [{"lv1": 7, "lv2": 0, "un1": 15, "un2": 15}],
		"video": {"type": "mpeg2", "resolution": "1080i", "streamContent": 1, "componentType": 179},
		"audios": [{"componentType": 3, "isMain": true, "samplingRate": 48000, "langs": ["jpn"]}]
	}`

	var p Program
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if p.ID != 3273601001 || p.EventID != 1001 || p.ServiceID != 1024 || p.NetworkID != 32736 {
		t.Errorf("ids decoded as %+v", p)
	}
	// startAt / name が落ちると epg_sync の projectable() が false になり、
	// 番組が 1 件も投影されない（internal/worker/epg.go）。
	if p.StartAt == nil {
		t.Fatal("startAt did not decode (epg_sync would project nothing)")
	}
	if want := time.UnixMilli(1756000000000); !p.StartAt.Time().Equal(want) {
		t.Errorf("startAt = %v, want %v", p.StartAt.Time(), want)
	}
	if p.Name == nil || *p.Name != "テスト番組" {
		t.Errorf("name = %v, want テスト番組", p.Name)
	}
	if p.Duration == nil || *p.Duration != 1800000 {
		t.Errorf("duration = %v, want 1800000", p.Duration)
	}
	if !p.IsFree {
		t.Error("isFree did not decode")
	}
	if len(p.Genres) != 1 || p.Genres[0].LV1 != 7 {
		t.Errorf("genres decoded as %+v", p.Genres)
	}
	if p.Video == nil || p.Video.StreamContent != 1 {
		t.Errorf("video decoded as %+v", p.Video)
	}
	if len(p.Audios) != 1 || p.Audios[0].SamplingRate != 48000 {
		t.Errorf("audios decoded as %+v", p.Audios)
	}
}

// TestProgramStartAtMarshalsAsUnixMillis は逆向き（エンコード）も固定する。
// モックはこの型を組み立てて返すので、ここが壊れると実 mirakc と違う形が出る。
func TestProgramStartAtMarshalsAsUnixMillis(t *testing.T) {
	start := Milliseconds(time.UnixMilli(1756000000000))
	b, err := json.Marshal(Program{ID: 1, StartAt: &start})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := decoded["startAt"]
	if !ok {
		t.Fatalf("startAt is not in the encoded object: %s", b)
	}
	if n, ok := got.(float64); !ok || int64(n) != 1756000000000 {
		t.Errorf("startAt encoded as %v, want the UNIX millisecond number", got)
	}
}

func TestServiceWireNames(t *testing.T) {
	const raw = `{
		"id": 3273601024,
		"serviceId": 1024,
		"networkId": 32736,
		"type": 1,
		"logoId": -1,
		"remoteControlKeyId": 1,
		"name": "テレビ局",
		"hasLogoData": false,
		"channel": {"type": "GR", "channel": "13"}
	}`

	var s Service
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.ID != 3273601024 || s.ServiceID != 1024 || s.NetworkID != 32736 {
		t.Errorf("ids decoded as %+v", s)
	}
	if s.Name != "テレビ局" {
		t.Errorf("name = %q", s.Name)
	}
	// channel.type が落ちると epg_sync の validChannelTypes が全サービスを
	// skip し、番組も 1 件も入らない（internal/worker/epg.go）。
	if s.Channel.Type != "GR" || s.Channel.Channel != "13" {
		t.Errorf("channel decoded as %+v", s.Channel)
	}
	if s.RemoteControlKeyID != 1 {
		t.Errorf("remoteControlKeyId = %d", s.RemoteControlKeyID)
	}
}

func TestTunerWireNames(t *testing.T) {
	const raw = `{
		"index": 0,
		"name": "GR0",
		"types": ["GR"],
		"command": "recdvb ...",
		"pid": 1234,
		"users": [],
		"isAvailable": true,
		"isRemote": false,
		"isFree": true,
		"isUsing": false,
		"isFault": false
	}`

	var tn Tuner
	if err := json.Unmarshal([]byte(raw), &tn); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// types が落ちると容量判定（internal/capacity）が GR 専用チューナーを
	// 数えられなくなる。
	if len(tn.Types) != 1 || tn.Types[0] != "GR" {
		t.Errorf("types decoded as %v", tn.Types)
	}
	if tn.Name != "GR0" || !tn.IsAvailable || tn.IsFault {
		t.Errorf("tuner decoded as %+v", tn)
	}
}

func TestScheduleWireNames(t *testing.T) {
	const raw = `{
		"state": "scheduled",
		"program": {"id": 3273601001, "eventId": 1001, "serviceId": 1024, "networkId": 32736, "startAt": 1756000000000, "duration": 1800000, "isFree": true},
		"options": {"contentPath": "a/b.m2ts", "priority": 3, "preFilters": [], "postFilters": []},
		"tags": ["rokuban"]
	}`

	var s Schedule
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.State != "scheduled" {
		t.Errorf("state = %q", s.State)
	}
	if s.Program.ID != 3273601001 {
		t.Errorf("program.id = %d", s.Program.ID)
	}
	if s.Options.ContentPath == nil || *s.Options.ContentPath != "a/b.m2ts" {
		t.Errorf("options.contentPath = %v", s.Options.ContentPath)
	}
	if s.Options.Priority != 3 {
		t.Errorf("options.priority = %d", s.Options.Priority)
	}
	if len(s.Tags) != 1 || s.Tags[0] != "rokuban" {
		t.Errorf("tags = %v", s.Tags)
	}
}
