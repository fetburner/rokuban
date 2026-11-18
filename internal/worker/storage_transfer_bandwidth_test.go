package worker

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 転送の帯域上限は転送元の置き場ごとに掛かり、上限の無い置き場は絞られない。
// 上限を持つ置き場（edge）の読みは 4000 B を 8000 B/s で読むので約 500ms かかり、
// central は帯域制限ラッパーを持たないことを検査する。
func TestTransferSourceReaderLimitsOnlyTheConfiguredSourceStore(t *testing.T) {
	t.Parallel()
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
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := w.transferSourceReader(context.Background(), tc.sourceStore, bytes.NewReader(make([]byte, 4000)))
			testutil.RequireNoError(t, err, "transferSourceReader")
			if tc.sourceStore == "central" {
				if _, ok := reader.(*storageTransferContextReader); !ok {
					t.Fatalf("central reader = %T, want context reader without rate limit", reader)
				}
			}
			start := time.Now()
			copied, err := io.Copy(io.Discard, reader)
			elapsed := time.Since(start)
			testutil.RequireNoError(t, err, "copying through transferSourceReader")
			if copied != 4000 {
				t.Fatalf("copied %d bytes, want 4000", copied)
			}
			if tc.check != nil {
				tc.check(t, elapsed)
			}
		})
	}
}

// stallingReader は 1 回目の Read の後、2 回目の Read の前に stall だけ止まり、以降は chunk バイトずつ返す。
// 止まりが明けた時刻を resumed に残す。
type stallingReader struct {
	src     io.Reader
	chunk   int
	stall   time.Duration
	reads   int
	resumed time.Time
}

func (s *stallingReader) Read(p []byte) (int, error) {
	s.reads++
	if s.reads == 2 {
		time.Sleep(s.stall)
		s.resumed = time.Now()
	}
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	return s.src.Read(p)
}

// TestTransferSourceReaderDoesNotCatchUpAfterStall は、読みの途中で止まった後も帯域の上限が効くことを固定する。
// 切断から張り直しまでの無応答を経過時間に数えると、再開後に貯まった分を回線速度で読んでしまう。
func TestTransferSourceReaderDoesNotCatchUpAfterStall(t *testing.T) {
	t.Parallel()
	w := &StorageTransferWorker{Config: config.StorageConfig{Transfer: config.StorageTransferConfig{
		Bandwidth: map[string]string{"edge": "8KBps"},
	}}}
	src := &stallingReader{src: bytes.NewReader(make([]byte, 4100)), chunk: 400, stall: 1200 * time.Millisecond}
	reader, err := w.transferSourceReader(context.Background(), "edge", src)
	testutil.RequireNoError(t, err, "transferSourceReader")
	_, err = io.Copy(io.Discard, reader)
	testutil.RequireNoError(t, err, "copying through transferSourceReader")
	// 止まりが明けた後の 4000 バイトのうち、最初の 400 バイトは直ちに返ってよい。残り 3600 バイトは 8KBps で約 450ms かかる。
	if after := time.Since(src.resumed); after < 350*time.Millisecond {
		t.Fatalf("reading after the stall took %v, want at least 350ms under the 8KBps limit", after)
	}
}
