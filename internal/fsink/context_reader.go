package fsink

import (
	"context"
	"io"
)

// ContextReader は各 Read の前に context の取消しを確認してから、下位の reader を読む。
// ファイルのハッシュ計算とストレージ転送は、読み取りを同じ条件で止める。
// TestContextReaderReturnsCancellationBeforeReading は取消し後の Read が下位へ進まないことを固定する。
type ContextReader struct {
	ctx    context.Context
	reader io.Reader
}

// NewContextReader は各 Read の前に ctx を確認する reader を作る。
func NewContextReader(ctx context.Context, reader io.Reader) *ContextReader {
	return &ContextReader{ctx: ctx, reader: reader}
}

// Read は context が生きている場合だけ下位の reader を読む。
func (r *ContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var _ io.Reader = (*ContextReader)(nil)
