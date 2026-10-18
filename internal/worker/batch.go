package worker

import "fmt"

// batchResults は sqlc が :batchexec 用に生成する型が満たすインターフェース。
type batchResults interface {
	Exec(f func(int, error))
	Close() error
}

// execBatch はバッチを実行し、最初のエラーを返す。Close は常に呼ぶ。
func execBatch(br batchResults) error {
	var firstErr error
	br.Exec(func(i int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d: %w", i, err)
		}
	})
	if closeErr := br.Close(); closeErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing batch: %w", closeErr)
	}
	return firstErr
}
