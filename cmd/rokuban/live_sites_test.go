package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/streamer"
)

// TestLiveSites_DispatchesByURLSite は issue #532 の受け入れ基準 5 を固定する:
// 2 site 束縛かつ live.enabled のとき、
// `/api/sites/{site}/networks/.../live/...` が URL の site の mirakc から
// 引くこと。
//
// LiveStreamer.Mount はどのインスタンスも同じ固定パターンを登録するため、
// 素朴に N 回 Mount すると chi が黙って上書きし「最後に登録した 1 site の
// LiveStreamer が全リクエストに応答する」壊れ方になる（liveSites の doc
// コメント参照。chi の重複登録が panic しないことは別途確認済み）。このテストは
// その壊れ方を検出する: tokyo 宛のリクエストが tokyo の mirakc スタブに届き、
// takamatsu のスタブには一切届かないこと（逆方向も見る）。
//
// mirakc への到達を StreamService の 1 呼び出しで確認できるところまでで
// 止める --- 実際の ffmpeg 起動やセグメント配信までは検証しない
// （そこは internal/streamer 自身のテストの責務。ffmpeg の exec は
// worker/streamer パッケージのみという不変条件 4 の境界内で完結している）。
//
// このテストが検出すべき変異: liveSites.Mount が同じ *streamer.LiveStreamer を
// 全 site 分登録する（重複 Mount に戻す）、または dispatch が
// chi.URLParam(r, "site") を見ずに固定の 1 site を返す。いずれも
// takamatsuRequests（または tokyoRequests）が意図せず増える形で検出できる。
func TestLiveSites_DispatchesByURLSite(t *testing.T) {
	var tokyoRequests, takamatsuRequests atomic.Int32
	newStub := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			http.Error(w, "no tuner in this test", http.StatusServiceUnavailable)
		}))
	}
	tokyoSrv := newStub(&tokyoRequests)
	defer tokyoSrv.Close()
	takamatsuSrv := newStub(&takamatsuRequests)
	defer takamatsuSrv.Close()

	cfg := streamer.LiveConfig{
		Enabled:     true,
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles:    []streamer.LiveProfile{{Name: "default"}},
	}
	sites := newLiveStreamersBySite([]config.MirakcSite{
		{Site: "tokyo", URL: tokyoSrv.URL},
		{Site: "takamatsu", URL: takamatsuSrv.URL},
	}, cfg)

	r := chi.NewRouter()
	sites.Mount(r)
	ts := httptest.NewServer(r)
	defer ts.Close()

	get := func(t *testing.T, site string) int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/sites/" + site + "/networks/1/services/2/live/playlist.m3u8")
		if err != nil {
			t.Fatalf("GET (site=%s): %v", site, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if status := get(t, "tokyo"); status == http.StatusNotFound {
		t.Errorf("tokyo request status = %d, want it to reach the tokyo LiveStreamer (not 404)", status)
	}
	if tokyoRequests.Load() == 0 {
		t.Error("tokyo mirakc stub received 0 requests, want >=1 (the playlist request should reach it)")
	}
	if got := takamatsuRequests.Load(); got != 0 {
		t.Errorf("takamatsu mirakc stub received %d requests, want 0 (a tokyo request must not leak to it)", got)
	}

	if status := get(t, "takamatsu"); status == http.StatusNotFound {
		t.Errorf("takamatsu request status = %d, want it to reach the takamatsu LiveStreamer (not 404)", status)
	}
	if takamatsuRequests.Load() == 0 {
		t.Error("takamatsu mirakc stub received 0 requests, want >=1")
	}
	if got := tokyoRequests.Load(); got != 1 {
		t.Errorf("tokyo mirakc stub received %d requests after the takamatsu call, want 1 (unchanged)", got)
	}

	if status := get(t, "osaka"); status != http.StatusNotFound {
		t.Errorf("unbound site status = %d, want 404", status)
	}
}

// TestLiveSites_MountRegistersNothingWhenUnbound は 0 サイト束縛（中央の
// 録画配信プロセス）で live.enabled でも streamer が落ちないこと、かつ
// ライブのルートを一切登録しないことを確認する（issue #532 の「含むもの」4:
// 旧「ちょうど 1 サイト」の事前検査を消した後もこの組み合わせが安全であること）。
func TestLiveSites_MountRegistersNothingWhenUnbound(t *testing.T) {
	sites := newLiveStreamersBySite(nil, streamer.LiveConfig{Enabled: true})
	if len(sites) != 0 {
		t.Fatalf("newLiveStreamersBySite(nil, ...) = %d streamers, want 0", len(sites))
	}

	r := chi.NewRouter()
	sites.Mount(r)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/sites/tokyo/networks/1/services/2/live/playlist.m3u8")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no live routes registered for an unbound process)", resp.StatusCode)
	}
}
