// Package notifier は Postgres の LISTEN/NOTIFY を購読し、ブラウザ向けの
// SSE (/api/events) として配り直す notifier ロールを実装する。
//
// api ロールはリクエスト/レスポンスに徹し、mirakc にもファイルシステムにも
// 依存しない（不変条件 1）ため、長寿命接続を張り続ける SSE 配信はこの
// notifier に分離している（issue #24 M2-19、issue #25 §4）。
//
// notifier はシングルトンではない。複数レプリカが立っても各自が Postgres を
// LISTEN し、自分にぶら下がる SSE クライアントへ配るだけなので、Redis
// アダプタのような追加の配送基盤は要らない（docs/data.md §3）。
package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// notifyChannel は Postgres の LISTEN/NOTIFY チャネル名。
	notifyChannel = "rokuban"

	// coalesceWindow はトピックごとに通知を合流させる時間窓。
	// トリガーは行単位で発火するため、まとめ書きや updated_at だけが変わる UPDATE で
	// 同じトピックが連続して届く。ヒントとしては 1 回で十分なので合流させる。
	coalesceWindow = 200 * time.Millisecond

	// heartbeatInterval は SSE コメントによる keep-alive の間隔。
	// リバースプロキシ・CDN のアイドルタイムアウトで切られるのを防ぐ。
	heartbeatInterval = 25 * time.Second

	// clientBuffer は 1 クライアントあたりの送信バッファ。
	// 溢れたら捨てる（ヒントなので、取りこぼしは staleTime 経過後の再取得で回復する）。
	clientBuffer = 16

	// listenRetryInterval は LISTEN コネクションが切れたときの再接続間隔。
	listenRetryInterval = 5 * time.Second
)

// EventHub は Postgres の NOTIFY を購読し、接続中の SSE クライアントへ配る。
//
// notifier レプリカを増やしても、各レプリカが自分で LISTEN するだけなので
// Redis アダプタのような追加基盤は要らない（issue #5、docs/data.md §3）。
type EventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}

	// listening は LISTEN が確立するたびに閉じ直される合図。
	// NOTIFY は「その時点で LISTEN しているセッション」にしか届かず、後から来た
	// listener には配送されない。LISTEN 確立前に発行された通知は失われるため、
	// 通知を期待する側（テストや readiness チェック）が確立を待てるようにする。
	listeningMu sync.Mutex
	listening   chan struct{}
}

// NewEventHub は EventHub を生成する。
func NewEventHub() *EventHub {
	return &EventHub{
		clients:   make(map[chan string]struct{}),
		listening: make(chan struct{}),
	}
}

// listeningC は LISTEN 確立で閉じられるチャネルを返す。
func (h *EventHub) listeningC() chan struct{} {
	h.listeningMu.Lock()
	defer h.listeningMu.Unlock()
	return h.listening
}

// markListening は LISTEN が確立したことを通知する。
func (h *EventHub) markListening() {
	h.listeningMu.Lock()
	defer h.listeningMu.Unlock()
	select {
	case <-h.listening:
		// 既に閉じている（再接続）
	default:
		close(h.listening)
	}
}

// waitListening は LISTEN が確立するまで待つ。確立済みなら即座に返る。
func (h *EventHub) waitListening(ctx context.Context) error {
	select {
	case <-h.listeningC():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe はトピックを受け取るチャネルと、購読を解除する関数を返す。
func (h *EventHub) Subscribe() (<-chan string, func()) {
	ch := make(chan string, clientBuffer)

	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish は全クライアントにトピックを配る。
// バッファが埋まっているクライアントには送らずに捨てる。
func (h *EventHub) Publish(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- topic:
		default:
			// 詰まっているクライアントのために全体を止めない。
			// 落とした通知はクライアントの staleTime 経過後の再取得で回復する。
		}
	}
}

// clientCount は接続中のクライアント数を返す。
func (h *EventHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Run は Postgres を LISTEN し、ctx がキャンセルされるまで通知を配り続ける。
// コネクションが切れた場合は待機して再接続する。
func (h *EventHub) Run(ctx context.Context, pool *pgxpool.Pool) error {
	for {
		if err := h.listenOnce(ctx, pool); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("event hub: listen failed, retrying", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(listenRetryInterval):
		}
	}
}

// listenOnce は 1 本のコネクションで LISTEN し、切断されるまで通知を配る。
func (h *EventHub) listenOnce(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return fmt.Errorf("LISTEN %s: %w", notifyChannel, err)
	}
	slog.Info("event hub: listening", "channel", notifyChannel)
	h.markListening()

	// 通知待ちはブロックするので専用の goroutine に出し、合流は呼び出し側のループで行う。
	ctx, cancel := context.WithCancel(ctx)

	// goroutine が conn.Conn() をまだ触っている間に conn.Release() でコネクションが
	// プールに返り、別の利用者に渡ってしまう窓を防ぐ。done は goroutine の終了で
	// 閉じる合図。
	//
	// defer は LIFO で実行されるので、ここで登録した defer は上の
	// `defer conn.Release()` より先に実行される。つまり実行順は
	// 「cancel() → goroutine の終了を待つ (<-done) → conn.Release()」になり、
	// Release が呼ばれる時点で goroutine は conn に触れていないことが保証される。
	done := make(chan struct{})
	defer func() {
		cancel()
		<-done
	}()

	topics := make(chan string)
	waitErr := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				waitErr <- err
				return
			}
			select {
			case topics <- n.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()

	return h.coalesce(ctx, topics, waitErr)
}

// coalesce は届いたトピックを coalesceWindow の間まとめてから Publish する。
//
// トリガーは行単位で発火するため同じトピックが連続して届く。クライアントにとっては
// 「このデータが変わった」が 1 回伝われば十分なので、窓の中で重複を潰す。
func (h *EventHub) coalesce(ctx context.Context, topics <-chan string, waitErr <-chan error) error {
	pending := make(map[string]struct{})
	var flushAt <-chan time.Time

	for {
		select {
		case topic := <-topics:
			pending[topic] = struct{}{}
			if flushAt == nil {
				flushAt = time.After(coalesceWindow)
			}

		case err := <-waitErr:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("waiting for notification: %w", err)

		case <-flushAt:
			for topic := range pending {
				h.Publish(topic)
				delete(pending, topic)
			}
			flushAt = nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Mount は SSE エンドポイント (/api/events) をルーターに登録する。
//
// internal/api.RouterConfig.Mounter の口に載せるためのメソッドで、これにより
// EventHub は streamer と同じ形（api の外で組み立て、同一プロセスなら
// Mounter 経由で相乗りする）で扱える。
func (h *EventHub) Mount(r chi.Router) {
	r.Get("/api/events", eventsHandler(h))
}

// eventsHandler は SSE (/api/events) を配信する http.Handler を返す。
//
// 配るのはトピック名だけで、変更内容は載せない。クライアントは該当クエリを
// invalidate して REST から取り直す（レベルトリガー）。
func eventsHandler(hub *EventHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// nginx がバッファリングしてイベントを溜め込むのを防ぐ（docs/api.md の
		// リバースプロキシ・フレンドリー要件）。
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// 再接続間隔のヒント。取りこぼしは再取得で回復するので短くてよい。
		_, _ = fmt.Fprint(w, "retry: 3000\n\n")
		flusher.Flush()

		topics, unsubscribe := hub.Subscribe()
		defer unsubscribe()

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()

		ctx := r.Context()
		for {
			select {
			case topic, open := <-topics:
				if !open {
					return
				}
				// EventSource は data が空のイベントを dispatch しないため、
				// event 名と併せて data も送る。
				if _, err := fmt.Fprintf(w, "event: %s\ndata: {\"topic\":%q}\n\n", topic, topic); err != nil {
					return
				}
				flusher.Flush()

			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()

			case <-ctx.Done():
				return
			}
		}
	}
}
