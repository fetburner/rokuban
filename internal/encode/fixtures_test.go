package encode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/webhook"
)

// webhookRecorder は webhook 先の httptest サーバが受け取ったイベントを記録する。
// ハンドラは別 goroutine で走るので internal/webhook のテストと同じく同期して持つ。
// encode_test.go からも使う。
type webhookRecorder struct {
	mu     sync.Mutex
	events []webhook.Event
}

// newWebhookRecorder は記録用サーバを立て、そこへ送る Client を返す。
func newWebhookRecorder(t *testing.T) (*webhookRecorder, *webhook.Client) {
	t.Helper()
	rec := &webhookRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading webhook body: %v", err)
		}
		var ev webhook.Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("unmarshalling webhook body: %v", err)
		}
		rec.mu.Lock()
		rec.events = append(rec.events, ev)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, webhook.New(config.WebhookConfig{URL: srv.URL})
}

// received は受信したイベントのコピーを返す。
func (r *webhookRecorder) received() []webhook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webhook.Event(nil), r.events...)
}

func centralTransferTestConfig() config.StorageConfig {
	return config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"edge":    {Kind: "fs"},
			"central": {Kind: "fs"},
		},
		Originals: []config.OriginalStoreRule{{Store: "central"}},
		Derived:   "central",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
}

func testFSObjectStore(root string) objstore.Store {
	objects, err := fsstore.New(root)
	if err != nil {
		panic(err)
	}
	return objects
}

func recordedArgs[T jobq.Args](t *testing.T, calls []jobqfaketest.EnqueueCall) []T {
	t.Helper()
	args := make([]T, 0, len(calls))
	for _, call := range calls {
		value, ok := call.Args.(T)
		if !ok {
			t.Fatalf("enqueued args type = %T, want %T", call.Args, *new(T))
		}
		args = append(args, value)
	}
	return args
}

// reset は記録を捨てる（2 パス目だけを見たいとき用）。
func (r *webhookRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func stringPtr(s string) *string { return &s }
