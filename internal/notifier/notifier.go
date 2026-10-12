// Package notifier は通知元を購読し、ブラウザ向けの SSE (/api/events) として
// 配り直す notifier ロールを実装する。
//
// api ロールはリクエスト/レスポンスに徹し、mirakc にもファイルシステムにも
// 依存しない（不変条件 1）ため、長寿命接続を張り続ける SSE 配信はこの
// notifier に分離している（issue #24 M2-19、issue #25 §4）。
//
// notifier はシングルトンではない。複数レプリカが立っても各自が通知元を購読し、
// 自分にぶら下がる SSE クライアントへ配るだけなので、Redis アダプタのような
// 追加の配送基盤は要らない（docs/data.md §3）。
package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/serverevent"
)

const (
	// coalesceWindow はトピックごとに通知を合流させる時間窓。
	// トリガーは行単位で発火するため、まとめ書きや updated_at だけが変わる UPDATE で
	// 同じトピックが連続して届く。ヒントとしては 1 回で十分なので合流させる。
	coalesceWindow = 200 * time.Millisecond

	// heartbeatInterval は SSE コメントによる keep-alive の間隔。
	// リバースプロキシ・CDN のアイドルタイムアウトで切られるのを防ぐ。
	heartbeatInterval = 25 * time.Second

	// clientBuffer は 1 クライアントあたりの送信バッファ。
	// 溢れたら捨てる。invalidate は定期再取得で回復し、進捗は次の値で上書きされる。
	clientBuffer = 16

	// listenRetryInterval は LISTEN コネクションが切れたときの再接続間隔。
	listenRetryInterval = 5 * time.Second
)

type serverEvent struct {
	name        string
	data        string
	coalesceKey string
}

// Source は EventHub に通知 payload を供給する。
//
// Listen は購読が確立した後に ready を呼ぶ。これ以降に発行された通知は out へ
// 届く。再接続のたびに ready を呼んでよい。接続が壊れたら error を返し、ctx が
// 終わったら ctx.Err() を返す。out への送信中に ctx が終わった場合も送信を止めて
// 戻る。通知の配送保証はない。
type Source interface {
	Listen(ctx context.Context, ready func(), out chan<- string) error
}

func decodeNotification(payload string) serverEvent {
	var progress serverevent.EncodeProgressEvent
	if json.Unmarshal([]byte(payload), &progress) == nil && progress.Type == serverevent.EncodeProgressEventType {
		return serverEvent{
			name:        serverevent.EncodeProgressEventType,
			data:        payload,
			coalesceKey: fmt.Sprintf("%s:%d:%s", serverevent.EncodeProgressEventType, progress.RecordingID, progress.Profile),
		}
	}
	return serverEvent{
		name:        payload,
		data:        fmt.Sprintf(`{"topic":%q}`, payload),
		coalesceKey: "topic:" + payload,
	}
}

// EventHub は Source から通知を受け取り、接続中の SSE クライアントへ配る。
//
// notifier レプリカを増やしても、各レプリカが通知元を購読して自分の SSE
// クライアントへ配るだけなので Redis アダプタのような追加基盤は要らない
// （docs/data.md §3）。
type EventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}

	retryInterval time.Duration

	// listening は Source が購読確立を知らせた合図。
	// 確立前に発行された通知は届かないことがあるため、通知を期待する側が
	// Source の ready 前後を区別できるようにする。
	listeningMu sync.Mutex
	listening   chan struct{}
}

// NewEventHub は EventHub を生成する。
func NewEventHub() *EventHub {
	return &EventHub{
		clients:       make(map[chan string]struct{}),
		retryInterval: listenRetryInterval,
		listening:     make(chan struct{}),
	}
}

// listeningC は LISTEN 確立で閉じられるチャネルを返す。
func (h *EventHub) listeningC() chan struct{} {
	h.listeningMu.Lock()
	defer h.listeningMu.Unlock()
	return h.listening
}

// markListening は Source の購読が確立したことを通知する。
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

// waitListening は Source の購読が確立するまで待つ。確立済みなら即座に返る。
func (h *EventHub) waitListening(ctx context.Context) error {
	select {
	case <-h.listeningC():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe は NOTIFY payload を受け取るチャネルと、購読を解除する関数を返す。
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

// Publish は全クライアントに NOTIFY payload を配る。
// バッファが埋まっているクライアントには送らずに捨てる。
func (h *EventHub) Publish(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- topic:
		default:
			// 詰まっているクライアントのために全体を止めない。
			// invalidate は定期再取得、進捗は次の値で回復する。
		}
	}
}

// clientCount は接続中のクライアント数を返す。
func (h *EventHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Run は Source を購読し、ctx がキャンセルされるまで通知を配り続ける。
// Source が error を返した場合は待機して再接続する。
func (h *EventHub) Run(ctx context.Context, src Source) error {
	for {
		if err := h.listenOnce(ctx, src); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("event hub: source failed, retrying", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.retryInterval):
		}
	}
}

// listenOnce は Source.Listen を 1 回実行し、切断されるまで通知を配る。
func (h *EventHub) listenOnce(ctx context.Context, src Source) error {
	topics := make(chan string)
	waitErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitErr <- src.Listen(ctx, h.markListening, topics)
	}()

	err := h.coalesce(ctx, topics, waitErr)
	// 次の Listen を始める前に前の Source goroutine が戻るのを待つ。
	<-done
	return err
}

// coalesce は届いた payload を coalesceWindow の間まとめてから Publish する。
//
// 通常トピックはトピック名、進捗は recording_id + profile をキーに重複を潰す。
// 進捗を payload 全体でキーにすると値が変わるたびに別イベントとして溜まるため、
// 区間中の最新値だけを残す。
func (h *EventHub) coalesce(ctx context.Context, topics <-chan string, waitErr <-chan error) error {
	pending := make(map[string]string)
	var flushAt <-chan time.Time

	for {
		select {
		case payload := <-topics:
			event := decodeNotification(payload)
			pending[event.coalesceKey] = payload
			if flushAt == nil {
				flushAt = time.After(coalesceWindow)
			}

		case err := <-waitErr:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return fmt.Errorf("source stopped without an error")
			}
			return fmt.Errorf("waiting for notification: %w", err)

		case <-flushAt:
			for key, payload := range pending {
				h.Publish(payload)
				delete(pending, key)
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
// 通常トピックは event 名と topic data にし、クライアントが REST を invalidate する。
// encode-progress だけは型付き JSON data をそのまま配送する揮発テレメトリ。
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
			case payload, open := <-topics:
				if !open {
					return
				}
				event := decodeNotification(payload)
				// EventSource は data が空のイベントを dispatch しないため、
				// event 名と併せて data も送る。
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data); err != nil {
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
