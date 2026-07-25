// Package streamer は録画ファイルのバイト配信を担う。
//
// api ロールはファイルシステムに依存しない（不変条件 1）ため、バイト転送は
// この streamer の所有物として分けている。monolith では同一プロセスなので
// ルーターから直接マウントするが、境界を最初から引いておくことで
// 分散構成でロールを分けるときにコードを動かさずに済む。
package streamer

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// contentType は録画原本の Content-Type。mirakc の record stream と揃える。
const contentType = "video/MP2T"

// Streamer は録画ファイルを配信する。
type Streamer struct {
	pool     *pgxpool.Pool
	mediaDir string
}

// New は Streamer を生成する。
func New(pool *pgxpool.Pool, mediaDir string) *Streamer {
	return &Streamer{pool: pool, mediaDir: mediaDir}
}

// Mount はルーターに配信エンドポイントを登録する。
func (s *Streamer) Mount(r chi.Router) {
	r.Get("/api/recordings/{id}/file", s.RecordingFile)
}

// RecordingFile は GET /api/recordings/{id}/file を処理する。
//
// Range・If-Range・If-Modified-Since の扱いは http.ServeContent に任せる。
// *os.File を渡しているので sendfile が効き、家庭内 LAN を飽和させる用途では
// nginx を挟む必要がない（docs/api.md）。
func (s *Streamer) RecordingFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}

	asset, err := sqlcgen.New(s.pool).GetOriginalMediaAssetForServing(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: looking up media asset", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// rel_path は ingest 時に検証済みだが、配信側でも独立に検証する。
	// DB に不正な行が入った場合に任意ファイルを読み出させないため。
	path, err := mediapath.Resolve(s.mediaDir, asset.RelPath)
	if err != nil {
		slog.Error("streamer: rejecting rel_path outside the media directory",
			"recording_id", id, "rel_path", asset.RelPath, "err", err)
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		// コミット（DB 行）があるのにファイルが無いのは不整合。孤児回収や
		// 外部からの削除で起こりうるので、記録して 404 にする。
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("streamer: media asset row exists but the file is missing",
				"recording_id", id, "rel_path", asset.RelPath)
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: opening media file", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		slog.Error("streamer: stat media file", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		slog.Error("streamer: rel_path points at a directory",
			"recording_id", id, "rel_path", asset.RelPath)
		http.NotFound(w, r)
		return
	}

	// ServeContent は name から Content-Type を推測しようとするので明示する。
	w.Header().Set("Content-Type", contentType)
	// 録画原本は一度書いたら変わらないが、ごみ箱からの復元などで同じ URL の
	// 中身が入れ替わりうるので immutable は付けない。
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")

	// name は Content-Type 推測にしか使われない。上で明示済みなので空でよい。
	http.ServeContent(w, r, "", info.ModTime(), f)
}
