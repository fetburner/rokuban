// Package queuecounthttp exposes deliverable queue counts for in-cluster KEDA polling.
package queuecounthttp

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/jobq"
)

// NewHandler は KEDA の metrics-api トリガ向け HTTP handler を返す。
// 常駐プロセスに載せ、配線側では notifier ロールを想定する。ScaledJob の worker は短命なため公開元にしない。
// アプリ内認証は /metrics と同じく持たない。KEDA operator はクラスタ内の Service 経由で Pod を直接呼び、
// nginx 経由では server 全体に設定された Basic 認証が適用される。Pod IP やサービス名は動的で固定できないため、
// 配線側で Host allowlist を免除する。
// KEDA が valueLocation で数値を取り出せる `{"value": n}` を返し、jobq の契約だけを受け取って adapter に依存しない。
func NewHandler(counter jobq.DeliverableCounter) http.Handler {
	router := chi.NewRouter()
	router.Get("/internal/queues/{queue}/deliverable-count", func(w http.ResponseWriter, r *http.Request) {
		count, err := counter.CountDeliverable(r.Context(), chi.URLParam(r, "queue"))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(struct {
				Error string `json:"error"`
			}{Error: "queue count unavailable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Value int64 `json:"value"`
		}{Value: count})
	})
	return router
}
