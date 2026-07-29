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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// contentType は録画原本の Content-Type。mirakc の record stream と揃える。
const contentType = "video/MP2T"

// thumbnailContentType はサムネイル JPEG の Content-Type。
const thumbnailContentType = "image/jpeg"

// Config は Streamer の設定。
type Config struct {
	// MediaDir はメディアストレージのルート。
	MediaDir string

	// AccelLocation が空でなければ X-Accel-Redirect でバイト転送を
	// リバースプロキシに委ねる（issue #1 の nginx コメント）。
	//
	// 認可判定はアプリ、バイト転送は nginx という分担で、アプリ側のコストは
	// ヘッダー 1 個。値は nginx の internal location（例: /_media/）で、
	// MediaDir 配下の相対パスを連結した URI を返す。
	AccelLocation string
}

// Streamer は録画ファイルを配信する。
type Streamer struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New は Streamer を生成する。
func New(pool *pgxpool.Pool, cfg Config) *Streamer {
	return &Streamer{pool: pool, cfg: cfg}
}

// Mount はルーターに配信エンドポイントを登録する。
func (s *Streamer) Mount(r chi.Router) {
	const path = "/api/recordings/{id}/file"
	r.Get(path, s.RecordingFile)
	// HEAD も登録する。VLC やブラウザはシーク前に HEAD で Content-Length と
	// Accept-Ranges を取るため、405 を返すとシーク再生に失敗しうる。
	// http.ServeContent は HEAD ならヘッダーだけを書くので実装は共通でよい。
	r.Head(path, s.RecordingFile)

	// サムネイルは openapi に載せない（原本 /file と同じ理由。バイナリ配信で
	// 生成クライアントが壊れる。issue #66 / docs/api.md）。
	const thumbPath = "/api/recordings/{id}/thumbnail"
	r.Get(thumbPath, s.RecordingThumbnail)
	r.Head(thumbPath, s.RecordingThumbnail)
}

// RecordingFile は GET /api/recordings/{id}/file を処理する。
//
// Range・If-Range・If-Modified-Since の扱いは http.ServeContent に任せる。
// *os.File を渡しているので sendfile が効き、家庭内 LAN を飽和させる用途では
// nginx を挟む必要がない（docs/api.md）。
func (s *Streamer) RecordingFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseRecordingID(r)
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

	s.serveAsset(w, r, id, asset.RelPath, asset.SizeBytes, asset.UpdatedAt, contentType)
}

// RecordingThumbnail は GET /api/recordings/{id}/thumbnail を処理する。
//
// kind = 'thumbnail' の active アセットを JPEG で返す。未生成・ごみ箱は 404。
// openapi には載せない（原本 /file と同じ。docs/api.md）。
func (s *Streamer) RecordingThumbnail(w http.ResponseWriter, r *http.Request) {
	id, err := parseRecordingID(r)
	if err != nil {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}

	asset, err := sqlcgen.New(s.pool).GetThumbnailMediaAssetForServing(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: looking up thumbnail asset", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.serveAsset(w, r, id, asset.RelPath, asset.SizeBytes, asset.UpdatedAt, thumbnailContentType)
}

func parseRecordingID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// serveAsset は rel_path のファイルを Content-Type 付きで配信する。
// original / thumbnail で共通（Range と X-Accel-Redirect も同じ経路）。
//
// rel_path は ingest / thumbnail 時に検証済みだが、配信側でも独立に検証する。
// DB に不正な行が入った場合に任意ファイルを読み出させないため。
func (s *Streamer) serveAsset(w http.ResponseWriter, r *http.Request, recordingID int64, relPath string, sizeBytes int64, _ time.Time, contentTypeHeader string) {
	path, err := mediapath.Resolve(s.cfg.MediaDir, relPath)
	if err != nil {
		slog.Error("streamer: rejecting rel_path outside the media directory",
			"recording_id", recordingID, "rel_path", relPath, "err", err)
		http.NotFound(w, r)
		return
	}

	// X-Accel-Redirect が有効なら、認可判定だけ済ませてバイト転送は
	// リバースプロキシに委ねる。Range の扱いも nginx 側になる。
	// パス検証を通した後に返すのが要点（検証前に返すと任意ファイルを配らせられる）。
	if s.cfg.AccelLocation != "" {
		w.Header().Set("Content-Type", contentTypeHeader)
		w.Header().Set("X-Accel-Redirect", accelURI(s.cfg.AccelLocation, relPath))
		return
	}

	f, err := os.Open(path)
	if err != nil {
		// コミット（DB 行）があるのにファイルが無いのは不整合。孤児回収や
		// 外部からの削除で起こりうるので、記録して 404 にする。
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("streamer: media asset row exists but the file is missing",
				"recording_id", recordingID, "rel_path", relPath)
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: opening media file", "recording_id", recordingID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		slog.Error("streamer: stat media file", "recording_id", recordingID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		slog.Error("streamer: rel_path points at a directory",
			"recording_id", recordingID, "rel_path", relPath)
		http.NotFound(w, r)
		return
	}

	// size_bytes はコミット時に書いた値。実ファイルと違うならコミット後に
	// 改変・切り詰めが起きている。配信は続けるが不整合として記録する。
	if info.Size() != sizeBytes {
		slog.Warn("streamer: file size differs from the committed size",
			"recording_id", recordingID, "rel_path", relPath,
			"committed", sizeBytes, "actual", info.Size())
	}

	// ServeContent は name から Content-Type を推測しようとするので明示する。
	w.Header().Set("Content-Type", contentTypeHeader)
	// 一度書いたら変わらないが、ごみ箱からの復元などで同じ URL の中身が
	// 入れ替わりうるので immutable は付けない。
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")

	// name は Content-Type 推測にしか使われない。上で明示済みなので空でよい。
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// accelURI は X-Accel-Redirect に載せる internal location の URI を組み立てる。
//
// nginx は X-Accel-Redirect の値を URI として解釈するため、パス要素は
// URL エスケープする。番組名由来のファイル名には空白・括弧・日本語が入る。
func accelURI(location, relPath string) string {
	segments := strings.Split(filepath.ToSlash(relPath), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.TrimSuffix(location, "/") + "/" + strings.Join(segments, "/")
}
