package api

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// GetStorage はストレージ観測の射影を返す (GET /api/storage、issue #238 M7-5)。
//
// api ロールはファイルシステムに依存しない（不変条件 1）ので、worker が
// storage_sync に書いた最新観測をそのまま読むだけ --- ここではファイルにも
// mirakc にも触れない。
func (h *Server) GetStorage(ctx context.Context, _ GetStorageRequestObject) (GetStorageResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListStorageSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing storage sync: %w", err)
	}

	result := make([]StorageRoot, 0, len(rows))
	for _, r := range rows {
		result = append(result, StorageRoot{
			Root:           StorageRootRoot(r.Root),
			Path:           r.Path,
			TotalBytes:     r.TotalBytes,
			UsedBytes:      r.UsedBytes,
			AvailableBytes: r.AvailableBytes,
			ObservedAt:     r.ObservedAt,
		})
	}
	return GetStorage200JSONResponse(result), nil
}
