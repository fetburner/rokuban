package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobs"
)

// insertRulerPassHint は指定した site の RulerPassArgs を呼び出し元のトランザクションに
// EnqueueTx する。移設が残る API 操作が使う旧経路で、ルール CRUD は commit 後の
// enqueueRulerPassHint を使う。
//
//nolint:unused // 残る API 移設が tx 内ヒントを必要とする場合に再利用する。
func (h *Server) insertRulerPassHint(ctx context.Context, tx db.Tx, site string) error {
	if h.enqueuer == nil {
		return nil
	}
	if _, err := h.enqueuer.EnqueueTx(ctx, tx, jobs.RulerPassArgs{Site: site}); err != nil {
		return fmt.Errorf("enqueuing ruler_pass hint for site %s: %w", site, err)
	}
	return nil
}

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

// insertRulerPassHintsForRuleSites は呼び出し元のトランザクション内で、指定 site へ
// ruler_pass を投入する。sites が空（指定なし = 全 site）の場合は全 site に展開する。
// この経路を使う API 操作が残る間は insertRulerPassHint とともに保持する。
//
//nolint:unused // 残る API 移設が tx 内ヒントを必要とする場合に再利用する。
func (h *Server) insertRulerPassHintsForRuleSites(ctx context.Context, tx db.Tx, sites []string) error {
	if len(sites) == 0 {
		sites = h.siteNames
	}
	for _, site := range sites {
		if err := h.insertRulerPassHint(ctx, tx, site); err != nil {
			return err
		}
	}
	return nil
}
