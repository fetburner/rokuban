//go:build s3conformance

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/cryptstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
)

// stopUploadOnRead は送信 slice の中断を、既知のバイト位置で起こす。
type stopUploadOnRead context.CancelFunc

func (stop stopUploadOnRead) Read([]byte) (int, error) {
	stop()
	return 0, context.Canceled
}

func encryptedConformanceObjects(ctx context.Context, cfg s3store.Config, key string, size int64) (objstore.Store, error) {
	video, err := os.ReadFile(os.Getenv("E2E_S3_VIDEO_FILE"))
	if err != nil {
		return nil, fmt.Errorf("reading encrypted S3 video fixture: %w", err)
	}
	// S3 の最初の 5 MiB パートが確定し、次のパートを読んでいる途中で止まる位置。
	const stopAfter = 6 << 20
	if int64(len(video)) != size || size <= stopAfter {
		return nil, fmt.Errorf("encrypted S3 video fixture size = %d, want %d and greater than %d", len(video), size, stopAfter)
	}
	const password, salt = "browser conformance password", "browser conformance salt"
	raw, err := s3store.NewDeclaredSHA256(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	writer, err := cryptstore.New(raw, storeName, password, salt, false)
	if err != nil {
		return nil, err
	}
	state, err := writer.StartUpload(ctx, key, size, sha256.Sum256(video), "video/mp4")
	if err != nil {
		return nil, fmt.Errorf("starting encrypted fixture upload: %w", err)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if gone, err := writer.AbortUpload(cleanup, state); err != nil || !gone {
			log.Printf("cleaning up encrypted fixture upload: gone=%t, err=%v", gone, err)
		}
	}()

	slice, stop := context.WithCancel(ctx)
	firstSource := io.MultiReader(bytes.NewReader(video[:stopAfter]), stopUploadOnRead(stop))
	state, offset, err := writer.ContinueUpload(slice, state, firstSource)
	stop()
	if !errors.Is(err, context.Canceled) || offset <= 0 || offset >= stopAfter {
		return nil, fmt.Errorf("interrupted encrypted fixture upload retained %d bytes, want progress before %d: %w", offset, stopAfter, err)
	}
	if _, err := writer.Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
		return nil, fmt.Errorf("encrypted fixture key must be absent before completion: %w", err)
	}
	state, offset, err = writer.ContinueUpload(ctx, state, bytes.NewReader(video[offset:]))
	if err != nil {
		return nil, fmt.Errorf("resuming encrypted fixture upload: %w", err)
	}
	if offset != size {
		return nil, fmt.Errorf("resumed encrypted fixture upload offset = %d, want %d", offset, size)
	}
	if err := writer.CompleteUpload(ctx, state); err != nil {
		return nil, fmt.Errorf("completing encrypted fixture upload: %w", err)
	}
	completed = true

	reader, err := s3store.NewDeclaredSHA256(ctx, cfg, true)
	if err != nil {
		return nil, err
	}
	return cryptstore.New(reader, storeName, password, salt, true)
}
