package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

const rfc3339 = time.RFC3339

func newAPIServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(nil, nil, pool))
	t.Cleanup(srv.Close)
	return srv
}

// seedEpgService は epg_services に 1 行入れる。
func seedEpgService(t *testing.T, pool *pgxpool.Pool, networkID, serviceID, remoteKey int32, name, channel string) {
	t.Helper()
	err := sqlcgen.New(pool).UpsertEpgService(context.Background(), []sqlcgen.UpsertEpgServiceParams{{
		Site:               defaultSite,
		NetworkID:          networkID,
		ServiceID:          serviceID,
		Type:               1,
		LogoID:             remoteKey,
		RemoteControlKeyID: remoteKey,
		Name:               name,
		ChannelType:        "GR",
		Channel:            channel,
		HasLogoData:        true,
	}}).Close()
	if err != nil {
		t.Fatalf("seeding epg_service: %v", err)
	}
}

// testProgramDuration は seedEpgProgram が使う番組長。
const testProgramDuration = time.Hour

// seedEpgProgram は epg_programs に 1 行入れる。detail が true なら jsonb 詳細も入れる。
func seedEpgProgram(t *testing.T, pool *pgxpool.Pool, programID int64, networkID, serviceID, eventID int32,
	name string, start time.Time, detail bool,
) {
	t.Helper()
	p := sqlcgen.UpsertEpgProgramParams{
		Site:        defaultSite,
		ProgramID:   programID,
		NetworkID:   networkID,
		ServiceID:   serviceID,
		EventID:     eventID,
		StartAt:     start,
		DurationMs:  testProgramDuration.Milliseconds(),
		EndAt:       start.Add(testProgramDuration),
		IsFree:      true,
		Name:        name,
		Description: name + "の説明",
		GenreLv1:    []int16{7},
	}
	if detail {
		p.Extended = json.RawMessage(`{"出演者":"テスト太郎"}`)
		p.Genres = json.RawMessage(`[{"lv1":7,"lv2":1,"un1":15,"un2":15}]`)
		p.Video = json.RawMessage(`{"type":"mpeg2","resolution":"1080i","streamContent":1,"componentType":179}`)
		p.Audios = json.RawMessage(`[{"componentType":3,"isMain":true,"samplingRate":48000,"langs":["jpn"]}]`)
	}
	if err := sqlcgen.New(pool).UpsertEpgProgram(context.Background(), []sqlcgen.UpsertEpgProgramParams{p}).Close(); err != nil {
		t.Fatalf("seeding epg_program: %v", err)
	}
}

func getJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", url, err)
		}
	}
	return resp
}

func programsURL(base string, start, end time.Time, extra ...string) string {
	q := url.Values{}
	q.Set("start", start.Format(rfc3339))
	q.Set("end", end.Format(rfc3339))
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return base + "/api/programs?" + q.Encode()
}

func TestListServices(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	seedEpgService(t, pool, 32678, 5168, 8, "ＯＨＫ", "27")
	seedEpgService(t, pool, 32676, 5152, 6, "ＲＳＫテレビ", "21")

	var got []Service
	resp := getJSON(t, srv.URL+"/api/services", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 2 {
		t.Fatalf("services = %d, want 2", len(got))
	}
	// remoteControlKeyId 昇順
	if got[0].Name != "ＲＳＫテレビ" || got[1].Name != "ＯＨＫ" {
		t.Errorf("order = %q, %q; want ＲＳＫテレビ, ＯＨＫ", got[0].Name, got[1].Name)
	}
	if got[0].ChannelType != "GR" || got[0].Channel != "21" || !got[0].HasLogoData {
		t.Errorf("service[0] = %+v", got[0])
	}
}

func TestListPrograms_Window(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour)
	seedEpgProgram(t, pool, 1, 32678, 5168, 1, "A-1", base, false)
	seedEpgProgram(t, pool, 2, 32678, 5168, 2, "A-2", base.Add(time.Hour), false)
	seedEpgProgram(t, pool, 3, 32676, 5152, 1, "B-1", base.Add(30*time.Minute), false)

	// 窓に一部でも重なる番組が入る（開区間）
	var got []ProgramListItem
	resp := getJSON(t, programsURL(srv.URL, base.Add(45*time.Minute), base.Add(75*time.Minute)), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 3 {
		t.Fatalf("programs = %d, want 3 (窓に重なるものすべて)", len(got))
	}
	// start_at 昇順
	if got[0].Name != "A-1" || got[1].Name != "B-1" || got[2].Name != "A-2" {
		t.Errorf("order = %q, %q, %q", got[0].Name, got[1].Name, got[2].Name)
	}
	// 一覧は軽い形。詳細 jsonb は含まない
	if got[0].Description != "A-1の説明" {
		t.Errorf("description = %q", got[0].Description)
	}
	if len(got[0].Genres) != 1 || got[0].Genres[0] != 7 {
		t.Errorf("genres = %v, want [7]", got[0].Genres)
	}
	if !got[0].EndAt.Equal(got[0].StartAt.Add(time.Hour)) {
		t.Errorf("endAt = %v, startAt = %v", got[0].EndAt, got[0].StartAt)
	}

	// 窓にちょうど接するだけの番組は入らない（A-1 は end == window_start）
	got = nil
	getJSON(t, programsURL(srv.URL, base.Add(time.Hour), base.Add(2*time.Hour)), &got)
	for _, p := range got {
		if p.Name == "A-1" {
			t.Error("A-1 should not overlap [01:00, 02:00)")
		}
	}
}

func TestListPrograms_ServiceFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour)
	seedEpgProgram(t, pool, 1, 32678, 5168, 1, "OHK", base, false)
	seedEpgProgram(t, pool, 2, 32676, 5152, 1, "RSK", base, false)

	var got []ProgramListItem
	getJSON(t, programsURL(srv.URL, base, base.Add(time.Hour), "serviceId", "5168"), &got)
	if len(got) != 1 || got[0].Name != "OHK" {
		t.Fatalf("service-filtered = %+v, want only OHK", got)
	}

	got = nil
	getJSON(t, programsURL(srv.URL, base, base.Add(time.Hour), "networkId", "32676"), &got)
	if len(got) != 1 || got[0].Name != "RSK" {
		t.Fatalf("network-filtered = %+v, want only RSK", got)
	}
}

// 広すぎる窓・逆順の窓は無言で切り詰めず 400 で拒否する。
func TestListPrograms_InvalidWindow(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour)
	tests := []struct {
		name       string
		start, end time.Time
	}{
		{"end == start", base, base},
		{"end before start", base.Add(time.Hour), base},
		{"window too wide", base, base.Add(maxProgramWindow + time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body ErrorResponse
			resp := getJSON(t, programsURL(srv.URL, tt.start, tt.end), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Error == "" {
				t.Error("error message is empty")
			}
		})
	}

	// 上限ちょうどは通る
	resp := getJSON(t, programsURL(srv.URL, base, base.Add(maxProgramWindow)), nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("exact max window status = %d, want 200", resp.StatusCode)
	}
}

func TestGetProgram(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Hour)
	seedEpgProgram(t, pool, 42, 32678, 5168, 1, "詳細あり", base, true)
	seedEpgProgram(t, pool, 43, 32678, 5168, 2, "詳細なし", base.Add(time.Hour), false)

	var got Program
	resp := getJSON(t, fmt.Sprintf("%s/api/programs/42", srv.URL), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.Name != "詳細あり" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Extended == nil || (*got.Extended)["出演者"] != "テスト太郎" {
		t.Errorf("extended = %v", got.Extended)
	}
	if got.Video == nil || got.Video.Resolution == nil || *got.Video.Resolution != "1080i" {
		t.Errorf("video = %+v", got.Video)
	}
	if got.Audios == nil || len(*got.Audios) != 1 || (*got.Audios)[0].SamplingRate != 48000 {
		t.Errorf("audios = %v", got.Audios)
	}
	if got.GenreDetails == nil || len(*got.GenreDetails) != 1 || (*got.GenreDetails)[0].Lv2 != 1 {
		t.Errorf("genreDetails = %v", got.GenreDetails)
	}

	// jsonb が NULL の番組は省略される（空オブジェクトを返さない）
	var bare Program
	getJSON(t, fmt.Sprintf("%s/api/programs/43", srv.URL), &bare)
	if bare.Extended != nil || bare.Video != nil || bare.Audios != nil || bare.GenreDetails != nil {
		t.Errorf("bare program should omit detail payloads, got %+v", bare)
	}

	resp = getJSON(t, fmt.Sprintf("%s/api/programs/999", srv.URL), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing program status = %d, want 404", resp.StatusCode)
	}
}
