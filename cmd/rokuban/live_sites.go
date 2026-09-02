package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/streamer"
)

// newLiveStreamersBySite は束縛サイトごとに streamer.LiveStreamer を 1 つ作る
// （issue #532 の「含むもの」4: streamer.NewLive を site ごとに作り、URL の
// {site} で選ぶ）。それぞれ自分の site の mirakc.Client を持つ。bound が空
// （0 サイト束縛）なら空の liveSites を返し、Mount/Run は何もしない。
func newLiveStreamersBySite(bound []config.MirakcSite, cfg streamer.LiveConfig) liveSites {
	sites := make(liveSites, len(bound))
	for _, s := range bound {
		mc := mirakc.NewClient(s.URL, nil)
		sites[s.Site] = streamer.NewLive(mc, s.Site, cfg)
	}
	return sites
}

// liveSites は site 名 → streamer.LiveStreamer の map で、api.Mounter を
// 実装する。ライブ視聴のリクエストを URL の `{site}` セグメントで選んだ
// LiveStreamer へ委譲する（issue #532 の「含むもの」4「URL の {site} で選ぶ
// （URL は既に site を運ぶ）」）。
//
// **ルートは 1 回だけ登録する。** LiveStreamer.Mount はどのインスタンスも
// 同じ固定パターン
// （`/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/...`）を
// 登録する。chi は同一パターン + メソッドの 2 度目の Handle 呼び出しを
// エラーにせず黙って上書きするため（実際に確認した最小再現: 同じパターンで
// 2 回 r.Get すると、後から登録したハンドラだけが応答する）、site ごとに
// LiveStreamer.Mount を N 回呼ぶと「最後に登録した 1 site の LiveStreamer が
// 全 site のリクエストに応答する」という壊れ方になる。liveSites はこれを
// 避けるため、ルートは自分で 1 回だけ登録し、実処理を site ごとの
// LiveStreamer に委譲する。
type liveSites map[string]*streamer.LiveStreamer

// Mount はライブ視聴のルートを登録する。bound sites が 0 なら何も登録しない
// （live.enabled かつ 0 サイト束縛の組み合わせは検査で弾いていないが、
// 中央プロセスに「このサイト」は無いので登録するルートも無い）。
func (ls liveSites) Mount(r chi.Router) {
	if len(ls) == 0 {
		return
	}
	const base = "/api/sites/{site}/networks/{networkId}/services/{serviceId}/live"
	r.Get(base+"/playlist.m3u8", ls.dispatch((*streamer.LiveStreamer).Playlist))
	r.Get(base+"/segments/{name}", ls.dispatch((*streamer.LiveStreamer).Segment))
	r.Post(base+"/leave", ls.dispatch((*streamer.LiveStreamer).Leave))
}

// dispatch は method（LiveStreamer.Playlist/Segment/Leave のいずれか）を、
// URL の {site} に対応する LiveStreamer に対して呼ぶ http.HandlerFunc を返す。
// 対応する site が無ければ 404 にする（LiveStreamer.resolveRequest が
// site 不一致を 404 にするのと同じ形に揃える）。
func (ls liveSites) dispatch(method func(*streamer.LiveStreamer, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := ls[chi.URLParam(r, "site")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		method(s, w, r)
	}
}

// Run は束ねた全 LiveStreamer の idle GC ループ（LiveStreamer.Run）を並行に
// 回す。ctx が Done になったら全員が後始末を終えるまで待って戻る
// （streamer.LiveStreamer.Run のコメント参照。チューナー解放のため）。
func (ls liveSites) Run(ctx context.Context) error {
	eg, egCtx := errgroup.WithContext(ctx)
	for _, s := range ls {
		eg.Go(func() error { return s.Run(egCtx) })
	}
	return eg.Wait()
}
