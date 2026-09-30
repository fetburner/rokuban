package api

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/programid"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestExportIPTVIncludesLiveAndPlayableRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	seedEpgService(t, pool, 1, 2, 1, "放送局", "27")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id,
                          name, channel_type, channel, has_logo_data)
VALUES ('secondary', 1, 2, 1, 1, 1, '別の放送局', 'GR', '27', false)`); err != nil {
		t.Fatalf("seeding secondary site service: %v", err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	original := seedRecording(t, pool, "原本あり", start, "finished", 1)
	seedIngested(t, pool, original, 1024, nil)
	encoded := seedRecording(t, pool, "encoded のみ", start.Add(-time.Hour), "finished", 2)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
VALUES ($1, 'encoded', 'h264', $2, 512)`, encoded, "test/encoded.mkv"); err != nil {
		t.Fatalf("seeding encoded asset: %v", err)
	}
	trashed := seedRecording(t, pool, "ごみ箱", start.Add(-2*time.Hour), "finished", 3)
	seedIngested(t, pool, trashed, 100, nil)
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, trashed); err != nil {
		t.Fatalf("moving recording to trash: %v", err)
	}

	handler := NewRouter(RouterConfig{
		Pool:               pool,
		Sites:              []string{"default", "secondary"},
		LiveEnabled:        true,
		LiveProfiles:       []LiveProfileSummary{{Name: "hd"}},
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + iptvPlaylistPath + "?liveProfile=hd")
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read playlist: %v", readErr)
	}
	playlist := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, playlist)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/vnd.apple.mpegurl") {
		t.Errorf("Content-Type = %q", got)
	}
	for _, want := range []string{
		"tvg-id=\"rokuban.default.1.2\"",
		"tvg-id=\"rokuban.secondary.1.2\"",
		"/api/sites/default/networks/1/services/2/live/playlist.m3u8?profile=hd",
		"/api/sites/secondary/networks/1/services/2/live/playlist.m3u8?profile=hd",
		"/api/media/recordings/" + int64String(original) + "/file",
	} {
		if !strings.Contains(playlist, want) {
			t.Errorf("playlist does not contain %q:\n%s", want, playlist)
		}
	}
	if !strings.HasPrefix(playlist, "#EXTM3U\n") || strings.Contains(playlist, "url-tvg") {
		t.Errorf("header must be a bare #EXTM3U without url-tvg:\n%s", playlist)
	}
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "#EXTINF") && strings.Contains(line, "group-title=\"録画") && strings.Contains(line, "tvg-id") {
			t.Errorf("recording entry must not carry tvg-id: %s", line)
		}
	}
	if strings.Contains(playlist, "ごみ箱") || strings.Contains(playlist, "encoded のみ") {
		t.Errorf("default playlist contains non-original or trashed recording:\n%s", playlist)
	}

	resp, err = http.Get(srv.URL + iptvPlaylistPath + "?include=recordings&recordingProfile=h264")
	if err != nil {
		t.Fatalf("GET encoded playlist: %v", err)
	}
	body, readErr = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read encoded playlist: %v", readErr)
	}
	playlist = string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("encoded status = %d, body = %s", resp.StatusCode, playlist)
	}
	wantEncodedURL := "/api/media/recordings/" + int64String(encoded) + "/file?profile=h264"
	if !strings.Contains(playlist, wantEncodedURL) || strings.Contains(playlist, "原本あり") {
		t.Errorf("encoded playlist = %s, want %q and no original-only recording", playlist, wantEncodedURL)
	}

	if _, err := pool.Exec(context.Background(), `
UPDATE media_assets SET state = 'deleted', deleted_at = now()
WHERE recording_id = $1 AND kind = 'original'`, original); err != nil {
		t.Fatalf("deleting original asset: %v", err)
	}
	resp, err = http.Get(srv.URL + iptvPlaylistPath + "?include=recordings")
	if err != nil {
		t.Fatalf("GET after original deletion: %v", err)
	}
	body, readErr = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read playlist after original deletion: %v", readErr)
	}
	if strings.Contains(string(body), "/api/media/recordings/"+int64String(original)+"/file") {
		t.Errorf("playlist still includes deleted original:\n%s", body)
	}
}

func getPlaylist(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, err %v, body %s", url, resp.StatusCode, err, body)
	}
	return string(body)
}

func TestExportIPTVEscapesAttributeQuotes(t *testing.T) {
	pool := testutil.SetupDB(t)
	seedEpgService(t, pool, 1, 2, 1, `局 & "局"`, "27")
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, LiveEnabled: true}))
	t.Cleanup(srv.Close)
	playlist := getPlaylist(t, srv.URL+iptvPlaylistPath+"?include=live")
	if want := `tvg-name="局 & ”局”"`; !strings.Contains(playlist, want) {
		t.Errorf("playlist does not contain %q:\n%s", want, playlist)
	}
}

func TestExportIPTVRecordingFilters(t *testing.T) {
	pool := testutil.SetupDB(t)
	seedEpgService(t, pool, 1, 2, 1, "放送局", "27")
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ok := seedRecording(t, pool, "完了", start, "finished", 1)
	seedIngested(t, pool, ok, 1, nil)
	failed := seedRecording(t, pool, "失敗", start.Add(-time.Hour), "failed", 2)
	seedIngested(t, pool, failed, 1, nil)
	recording := seedRecording(t, pool, "録画中", start.Add(-2*time.Hour), "recording", 3)
	seedIngested(t, pool, recording, 1, nil)
	superseded := seedRecording(t, pool, "置換済み", start.Add(-3*time.Hour), "finished", 4)
	seedIngested(t, pool, superseded, 1, nil)
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET superseded_at = now() WHERE id = $1`, superseded); err != nil {
		t.Fatalf("superseding recording: %v", err)
	}

	// ライブ無効の include=all は局エントリを出さず録画だけを返す。
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	t.Cleanup(srv.Close)
	playlist := getPlaylist(t, srv.URL+iptvPlaylistPath+"?include=all")
	if strings.Contains(playlist, "/live/playlist.m3u8") {
		t.Errorf("live entries must be omitted when live is disabled:\n%s", playlist)
	}
	if !strings.Contains(playlist, "/api/media/recordings/"+int64String(ok)+"/file") {
		t.Errorf("finished recording is missing:\n%s", playlist)
	}
	for _, id := range []int64{failed, recording, superseded} {
		if strings.Contains(playlist, "/api/media/recordings/"+int64String(id)+"/file") {
			t.Errorf("recording %d must not be listed:\n%s", id, playlist)
		}
	}
}

func TestExportIPTVRejectsInvalidProfilesAndDisabledLive(t *testing.T) {
	pool := testutil.SetupDB(t)
	handler := NewRouter(RouterConfig{
		Pool:               pool,
		LiveProfiles:       []LiveProfileSummary{{Name: "hd"}},
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?include=live", http.StatusNotFound},
		{"?include=other", http.StatusBadRequest},
		{"?include=all&liveProfile=unknown", http.StatusBadRequest},
		{"?include=recordings&recordingProfile=unknown", http.StatusBadRequest},
		{"?include=live&include=recordings", http.StatusBadRequest},
	} {
		resp, err := http.Get(srv.URL + iptvPlaylistPath + tc.query)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.query, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s status = %d, want %d", tc.query, resp.StatusCode, tc.want)
		}
	}
}

func TestExportXMLTVUsesRokubanResourceKeys(t *testing.T) {
	pool := testutil.SetupDB(t)
	seedEpgService(t, pool, 1, 2, 1, `局 & "局"`, "27")
	programID := programid.ComposeProgramID(1, 2, 3)
	start := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	seedEpgProgram(t, pool, programID, 1, 2, 3, `番組 <A & B>`, start, false)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id,
                          name, channel_type, channel, has_logo_data)
VALUES ('secondary', 1, 2, 1, 1, 1, '別の局', 'GR', '27', false)`); err != nil {
		t.Fatalf("seeding secondary site service: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at,
                          duration_ms, end_at, name, description)
VALUES ('secondary', $1, 1, 2, 3, $2, $3, $4, '別サイトの番組', '')`,
		programID, start, testProgramDuration.Milliseconds(), start.Add(testProgramDuration)); err != nil {
		t.Fatalf("seeding secondary site program: %v", err)
	}
	zeroDurationID := programid.ComposeProgramID(1, 2, 4)
	zeroDurationStart := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	seedEpgProgram(t, pool, zeroDurationID, 1, 2, 4, "尺未定", zeroDurationStart, false)
	if _, err := pool.Exec(context.Background(), `
UPDATE epg_programs SET duration_ms = 0, end_at = start_at
WHERE site = 'default' AND program_id = $1`, zeroDurationID); err != nil {
		t.Fatalf("setting zero duration: %v", err)
	}

	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, Sites: []string{"default", "secondary"}}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + xmltvGuidePath)
	if err != nil {
		t.Fatalf("GET XMLTV: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var doc struct {
		Channels []struct {
			ID          string   `xml:"id,attr"`
			DisplayName []string `xml:"display-name"`
		} `xml:"channel"`
		Programmes []struct {
			Start   string `xml:"start,attr"`
			Stop    string `xml:"stop,attr"`
			Channel string `xml:"channel,attr"`
			Title   string `xml:"title"`
			Desc    string `xml:"desc"`
			URL     struct {
				System string `xml:"system,attr"`
				Value  string `xml:",chardata"`
			} `xml:"url"`
		} `xml:"programme"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode XMLTV: %v", err)
	}
	wantChannelID := exportChannelID("default", 1, 2)
	wantSecondaryChannelID := exportChannelID("secondary", 1, 2)
	if len(doc.Channels) != 2 || doc.Channels[0].ID != wantChannelID || doc.Channels[1].ID != wantSecondaryChannelID {
		t.Fatalf("channels = %+v, want id %q", doc.Channels, wantChannelID)
	}
	if len(doc.Programmes) != 3 {
		t.Fatalf("programmes = %d, want 3", len(doc.Programmes))
	}
	type programmeResult struct {
		channel string
		title   string
		desc    string
		start   string
		stop    string
	}
	programmesByURL := make(map[string]programmeResult)
	for _, programme := range doc.Programmes {
		if programme.URL.System != "rokuban-api" {
			t.Errorf("programme URL system = %q", programme.URL.System)
		}
		programmesByURL[programme.URL.Value] = programmeResult{
			channel: programme.Channel,
			title:   programme.Title,
			desc:    programme.Desc,
			start:   programme.Start,
			stop:    programme.Stop,
		}
	}
	first := programmesByURL[programAPIURL("default", programID)]
	if first.channel != wantChannelID || first.title != `番組 <A & B>` || first.desc == "" || first.stop == "" {
		t.Errorf("default programme = %+v", first)
	}
	if _, ok := programmesByURL[programAPIURL("secondary", programID)]; !ok {
		t.Errorf("secondary site API key is missing from XMLTV")
	}
	zero := programmesByURL[programAPIURL("default", zeroDurationID)]
	if zero.title != "尺未定" || zero.stop != "" {
		t.Errorf("zero-duration programme = %+v, want stop omitted", zero)
	}
	parsedStart, err := time.Parse("20060102150405 -0700", first.start)
	if err != nil || !parsedStart.Equal(start) {
		t.Errorf("programme start = %q, parse err %v, want %s", first.start, err, start)
	}
}

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}
