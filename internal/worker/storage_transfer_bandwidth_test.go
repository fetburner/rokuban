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
	w := &StorageTransferWorker{Config: config.StorageConfig{Transfer: config.StorageTransferConfig{
		Bandwidth: map[string]string{"edge": "8KBps"},
	}}}
	src := &stallingReader{src: bytes.NewReader(make([]byte, 4100)), chunk: 400, stall: 1200 * time.Millisecond}
	reader, err := w.transferSourceReader(context.Background(), "edge", src)
	if err != nil {
		t.Fatalf("transferSourceReader: %v", err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("copying through transferSourceReader: %v", err)
	}
	// 止まりが明けた後の 4000 バイトのうち、最初の 400 バイトは直ちに返ってよい。残り 3600 バイトは 8KBps で約 450ms かかる。
	if after := time.Since(src.resumed); after < 350*time.Millisecond {
		t.Fatalf("reading after the stall took %v, want at least 350ms under the 8KBps limit", after)
	}
}
