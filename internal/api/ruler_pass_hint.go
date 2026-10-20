package api

import (
	"context"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobs"
)

// enqueueRulerPassHint は永続化の commit 後に ruler_pass をヒントとして投入する。
// 投入に失敗しても、定期の ruler パスが永続状態から再計算する。
func (h *Server) enqueueRulerPassHint(ctx context.Context, site string) {
	if h.enqueuer == nil {
		return
	}
	if _, err := h.enqueuer.Enqueue(ctx, jobs.RulerPassArgs{Site: site}); err != nil {
		slog.ErrorContext(ctx, "api: failed to enqueue ruler_pass hint", "site", site, "err", err)
	}
}

// enqueueRulerPassHintsForRuleSites はルールの commit 後に対象 site へ ruler_pass を投入する。
// sites が空（指定なし = 全 site）の場合は、レジストリの全 site に展開する。
func (h *Server) enqueueRulerPassHintsForRuleSites(ctx context.Context, sites []string) {
	if len(sites) == 0 {
		sites = h.siteNames
	}
	for _, site := range sites {
		h.enqueueRulerPassHint(ctx, site)
	}
}
