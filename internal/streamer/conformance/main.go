//go:build s3conformance

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/streamer"
)

const (
	storeName = "s3main"
	profile   = "s3conformance"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	databaseURL := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	endpoint := os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")
	bucket := os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET")
	videoKey := os.Getenv("E2E_S3_VIDEO_KEY")
	videoSize, err := strconv.ParseInt(os.Getenv("E2E_S3_VIDEO_SIZE"), 10, 64)
	if databaseURL == "" || endpoint == "" || bucket == "" || videoKey == "" || err != nil || videoSize <= 0 {
		return errors.New("ROKUBAN_TEST_DATABASE_URL, ROKUBAN_S3_CONFORMANCE_ENDPOINT, ROKUBAN_S3_CONFORMANCE_BUCKET, E2E_S3_VIDEO_KEY, and positive E2E_S3_VIDEO_SIZE are required")
	}
	if !strings.HasPrefix(videoKey, "encoded/") || !strings.HasSuffix(videoKey, ".mp4") {
		return fmt.Errorf("E2E_S3_VIDEO_KEY %q must be an encoded MP4 key", videoKey)
	}
	region := os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if err := db.MigrateUp(ctx, databaseURL); err != nil {
		return fmt.Errorf("migrating S3 browser conformance database: %w", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to S3 browser conformance database: %w", err)
	}
	defer pool.Close()
	objects, err := s3store.New(ctx, s3store.Config{Endpoint: endpoint, Region: region, Bucket: bucket})
	if err != nil {
		return fmt.Errorf("creating conformance object store: %w", err)
	}
	queries := sqlcgen.New(pool)
	startAt := time.Now().Truncate(time.Second)
	recordingID, err := queries.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source: "manual", Site: "default", NetworkID: 32678, ServiceID: 5168,
		EventID: int32(5000 + time.Now().UnixNano()%60000), ServiceName: "S3 browser conformance",
		ChannelType: "GR", Channel: "27", Title: "S3 browser conformance",
		IsFree: true, ProgramStartAt: startAt, ProgramDurationMs: 180_000, Status: "finished",
	})
	if err != nil {
		return fmt.Errorf("creating conformance recording: %w", err)
	}
	profileName := profile
	assetID, err := queries.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profileName,
		RelPath: videoKey, SizeBytes: videoSize,
	})
	if err != nil {
		return fmt.Errorf("creating conformance encoded asset: %w", err)
	}
	if _, err := queries.UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: storeName, RelPath: videoKey, Generation: 0, State: "active",
	}); err != nil {
		return fmt.Errorf("creating conformance S3 copy: %w", err)
	}

	media := pgstore.NewMediaWithStores(pool, storeName, storeName, storeName)
	router := chi.NewRouter()
	router.Get("/__health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"recordingId": recordingID})
	})
	streamer.New(media, streamer.Config{
		MediaDir: os.TempDir(), ObjectStores: map[string]objstore.Store{storeName: objects},
		SignedURLExpiry: 24 * time.Hour,
	}).Mount(router)
	server := &http.Server{Addr: "127.0.0.1:4176", Handler: router, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("stopping conformance streamer: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("conformance streamer: %w", err)
		}
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("starting conformance streamer: %w", err)
		}
	}
	return nil
}
