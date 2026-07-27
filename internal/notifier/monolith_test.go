package notifier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/streamer"
	"github.com/fetburner/rokuban/internal/testutil"
)

// monolith（--all 相当）で REST・SSE・メディア配信のすべてが同一ルーターに
// 同居して動くこと。streamer と notifier を api.Mounters で束ねて mount する
// （cmd/rokuban/server.go が --all のときに行うのと同じ構成）。
func TestMonolith_RESTAndSSEAndMediaCoexist(t *testing.T) {
	pool := testutil.SetupDB(t)

	mediaDir := t.TempDir()
	content := []byte("dummy ts data for monolith test")
	if err := os.WriteFile(filepath.Join(mediaDir, "r.m2ts"), content, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	recordingID := seedRecording(t, pool, "モノリスの録画", time.Now().Truncate(time.Second), "finished", 1)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "r.m2ts",
		SizeBytes:   int64(len(content)),
	}); err != nil {
		t.Fatalf("seeding media_asset: %v", err)
	}

	hub := NewEventHub()
	runHub(t, hub, pool)

	router := api.NewRouter(api.RouterConfig{
		Pool: pool,
		Mounter: api.Mounters{
			streamer.New(pool, streamer.Config{MediaDir: mediaDir}),
			hub,
		},
	})
	srv := httptest.NewServer(router)
	// t.Cleanup（defer ではなく）で登録する。openSSE が登録する「レスポンス
	// ボディを閉じる」クリーンアップより先に登録しておくことで、cleanup の
	// LIFO 実行順で「SSE 接続を閉じる → サーバーを閉じる」の順になる。
	// defer だとテスト関数の return 時点（openSSE の cleanup 登録より前）に
	// 即座に Close が走り、まだ開いている SSE 接続を待って固まる。
	t.Cleanup(srv.Close)

	// REST: oapi-codegen 生成ルート
	resp, err := http.Get(srv.URL + "/api/recordings")
	if err != nil {
		t.Fatalf("GET /api/recordings: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/recordings status = %d, want 200", resp.StatusCode)
	}

	// メディア配信: streamer の Mounter
	fileResp, err := http.Get(fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID))
	if err != nil {
		t.Fatalf("GET recording file: %v", err)
	}
	defer func() { _ = fileResp.Body.Close() }()
	if fileResp.StatusCode != http.StatusOK {
		t.Fatalf("GET recording file status = %d, want 200", fileResp.StatusCode)
	}
	got, err := io.ReadAll(fileResp.Body)
	if err != nil {
		t.Fatalf("reading file body: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("file content = %q, want %q", got, content)
	}

	// SSE: notifier の Mounter。REST 書き込みが NOTIFY 経由で届くこと。
	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)
	waitListening(t, hub)

	if err := sqlcgen.New(pool).NotifyTopic(context.Background(), "epg"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	if got := readSSEEvent(t, reader); got != "epg" {
		t.Errorf("SSE event = %q, want epg", got)
	}
}
