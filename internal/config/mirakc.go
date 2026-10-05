package config

/* mirakc site registry configuration. */

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MirakcSite は `mirakcs:` レジストリの 1 要素。site 名と URL の 2 つだけを持つ。
//
// **storage / worker / ingest 等のチューニング値は要素に入れない。** アーカイブは
// 単一（`media_assets` テーブルに site 列が無い）
// であり、`worker.queues` / `worker.periodic_jobs` 等はデプロイ時のパラメータで
// あって site の属性ではない。site ごとのチューニング値は、それを読むコードが
// できたときに足す（不変条件 11: 形を固定する前に判定基準を書く。issue #183 M4-11）。
type MirakcSite struct {
	// Site はこの mirakc インスタンスのサイト名。programId / record id は mirakc
	// インスタンス単位のスコープしか持たないため、DB の全テーブルと API のパスが
	// この名前でスコープされる（docs/schema.md §1-5、issue #31）。
	Site string `yaml:"site"`
	URL  string `yaml:"url"`
}

// Registry はこの Config が指す mirakc サイトの一覧を返す。
//
// **Load を経た Config では空にならない**（`mirakcs:` が空なら Load が起動エラー
// にする）。
func (c Config) Registry() []MirakcSite {
	return c.Mirakcs
}

// mirakcSiteNamePattern は site 名の構文制約。
//
// **River のキュー名の制約（`validateQueueName`、river@v0.47.0 client.go）と
// 同一で、緩めない。** M4-13 がキュー名を `ingest_<site>` の形に site で修飾する
// ため、ここを緩めると M4-13 が site 名をキュー名として弾くことになる
// （issue #183 の「罠」）。
var mirakcSiteNamePattern = regexp.MustCompile(`^[a-z0-9]([_-]?[a-z0-9])*$`)

// MirakcSiteNameMaxLen は site 名の最大長。
//
// River のキュー名の上限（internal/jobs.RiverQueueNameMaxLen、64 文字）から、
// site 単位のキュー（internal/jobs の siteBoundQueueNames = ingest/epg/
// reconciler/watcher）を jobs.PhysicalQueueName で `<base>_<site>` に修飾したときの
// prefix のうち最長のもの（`reconciler_`、11 文字。base の中で `reconciler` が
// 最長のため）を引いた値: 64 - 11 = 53。**siteBoundQueueNames に `reconciler`
// より長い論理名が増えたら、この 53 を引き直す必要がある**
// （internal/jobs.TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen が
// この関係を機械的に固定している）。
const MirakcSiteNameMaxLen = 53

// reservedSiteNames は実在する予約ディレクトリと衝突する site 名。
//
// `catalog/`（internal/catalog.Subdir）は削除 reconcile の孤児回収
// （internal/worker/delete_reconcile.go の walkMediaFiles）と rescue スキャン
// （internal/catalog/rescue_scan.go）が SkipDir する対象で、`thumbnails/` は
// サムネイルの名前空間（internal/worker/thumbnail.go）。M4-14 が `rel_path` に
// `{site}/` を前置するようになると、この 2 つと衝突する site 名はそのサイトの
// 原本が孤児回収からも rescue からも見えなくなる（issue #183 の「罠」）。
var reservedSiteNames = map[string]bool{
	"catalog":    true,
	"thumbnails": true,
}

// validateSiteName は site 名の構文制約・上限長・予約名を検査する。
// 見つかった問題を全件返す（規約 4: エラーは全件列挙）。
func validateSiteName(name string) []string {
	var errs []string
	if !mirakcSiteNamePattern.MatchString(name) {
		errs = append(errs, fmt.Sprintf("site name %q must match %s", name, mirakcSiteNamePattern.String()))
	}
	if len(name) > MirakcSiteNameMaxLen {
		errs = append(errs, fmt.Sprintf(
			"site name %q exceeds %d characters (River's queue name limit is 64 characters, and "+
				"site-bound queues carry a prefix such as \"reconciler_\"; the site name itself "+
				"must leave room for that prefix)",
			name, MirakcSiteNameMaxLen))
	}
	if reservedSiteNames[name] {
		errs = append(errs, fmt.Sprintf("site name %q is reserved", name))
	}
	return errs
}

// validateMirakcRegistry は `mirakcs:` の非空性、site 名の構文制約・予約名・
// 重複、各要素の url を検査する。見つかった問題を全件列挙して返す（規約 4）。
// 問題が無ければ nil を返す。
func (c Config) validateMirakcRegistry() error {
	var errs []string
	if len(c.Mirakcs) == 0 {
		errs = append(errs, "mirakcs is required")
	}
	seen := make(map[string]bool, len(c.Mirakcs))
	for i, s := range c.Mirakcs {
		label := fmt.Sprintf("mirakcs[%d]", i)
		for _, e := range validateSiteName(s.Site) {
			errs = append(errs, fmt.Sprintf("%s: %s", label, e))
		}
		if s.Site != "" && seen[s.Site] {
			errs = append(errs, fmt.Sprintf("%s: duplicate site %q", label, s.Site))
		}
		seen[s.Site] = true

		switch {
		case s.URL == "":
			errs = append(errs, fmt.Sprintf("%s.url is required", label))
		case !isAbsoluteURL(s.URL):
			errs = append(errs, fmt.Sprintf("%s.url %q is not a valid URL", label, s.URL))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("mirakc registry validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// isAbsoluteURL は mirakc の url として使える形か（scheme と host を持つ絶対 URL）
// を返す。**HTTP クライアントに渡せるか**だけを見る。
//
// scheme や host を欠いても mirakc.NewClient は文字列を保持するだけで失敗せず、
// `http.NewRequest` も通る。最初に失敗するのは `Client.Do`（RoundTrip）で、
// 実測ではメッセージも入力ごとに違う（`/api/tuners` は
// `unsupported protocol scheme ""`、`http://` は `http: no Host in request URL`）。
// つまり起動は通り、録画のたびに違う理由で失敗する。設定の誤りは起動時に出す。
//
// 到達性は検査しない（起動時に mirakc が落ちていても起動は通す。レベル
// トリガーで後から収束する）。
func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}
