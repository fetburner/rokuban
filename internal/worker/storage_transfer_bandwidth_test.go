package worker

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
)

// 転送の帯域上限は転送元の置き場ごとに掛かり、上限の無い置き場は絞られない。
// 上限を持つ置き場（edge）の読みは 4000 B を 8000 B/s で読むので約 500ms かかり、
// 上限の無い置き場（central）は即座に読み終わる。
func TestTransferSourceReaderLimitsOnlyTheConfiguredSourceStore(t *testing.T) {
	w := &StorageTransferWorker{Config: config.StorageConfig{Transfer: config.StorageTransferConfig{
		Bandwidth: map[string]string{"edge": "8KBps"},
	}}}
	for _, tc := range []struct {
		name        string
		sourceStore string
		check       func(t *testing.T, elapsed time.Duration)
	}{
		{
			name:        "edge",
			sourceStore: "edge",
			check: func(t *testing.T, elapsed time.Duration) {
				if elapsed < 400*time.Millisecond {
					t.Fatalf("edge read took %v, want at least 400ms under the 8KBps limit", elapsed)
				}
			},
		},
		{
			name:        "central",
			sourceStore: "central",
			check: func(t *testing.T, elapsed time.Duration) {
				if elapsed >= 100*time.Millisecond {
					t.Fatalf("central read took %v, want under 100ms: a store without a bandwidth entry must not be throttled", elapsed)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := w.transferSourceReader(context.Background(), tc.sourceStore, bytes.NewReader(make([]byte, 4000)))
			if err != nil {
				t.Fatalf("transferSourceReader: %v", err)
			}
			start := time.Now()
			copied, err := io.Copy(io.Discard, reader)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("copying through transferSourceReader: %v", err)
			}
			if copied != 4000 {
				t.Fatalf("copied %d bytes, want 4000", copied)
			}
			tc.check(t, elapsed)
		})
	}
}
