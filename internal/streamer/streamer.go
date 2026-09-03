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

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// contentTypeOriginal は録画原本の Content-Type。mirakc の record stream と揃える。
const contentTypeOriginal = "video/MP2T"

// contentTypeMP4 は encoded 派生物（MP4 progressive）の Content-Type。
const contentTypeMP4 = "video/mp4"

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

// serveAsset は DB から解決した 1 アセットをディスクから（または X-Accel で）配信する。
type serveAsset struct {
	relPath   string
	sizeBytes int64
	// sidecar は WebVTT 字幕サイドカーかどうか。サイドカーは encoded 行の
	// 隣接ファイルであり、独立した media_assets 行（size_bytes・存在の保証）を
	// 持たない --- 字幕を使っていない全 encoded 再生で発生する既定状態なので、
	// 欠損やサイズ不一致を「コミットがあるのにファイルが無い」不整合の WARN
	// として記録しない（通常アセットは既定 false のまま配線不要）。
	sidecar bool
	// contentType は明示する（ServeContent の拡張子推測に頼らない）。
	contentType string
}

// RecordingFile は GET/HEAD /api/recordings/{id}/file を処理する。
//
// profile クエリが無ければ原本（kind=original）、あれば encoded 派生物。
// Range・If-Range・If-Modified-Since の扱いは http.ServeContent に任せる。
// *os.File を渡しているので sendfile が効き、家庭内 LAN を飽和させる用途では
// nginx を挟む必要がない（docs/api.md）。
func (s *Streamer) RecordingFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseRecordingID(r)
	if err != nil {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}

	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	track := strings.TrimSpace(r.URL.Query().Get("track"))
	if track != "" && (track != "subtitles" || profile == "") {
		http.Error(w, "invalid track", http.StatusBadRequest)
		return
	}
	asset, err := s.lookupAsset(r, id, profile)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: looking up media asset", "recording_id", id, "profile", profile, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if track == "subtitles" {
		subPath, subErr := mediapath.SubtitleSibling(asset.relPath)
		if subErr != nil {
			// encoded の rel_path が拡張子を持たない等、サイドカーパスを
			// 構成できない。字幕サイドカーは無いのと同じ扱いで 404。
			http.NotFound(w, r)
			return
		}
		asset.relPath = subPath
		asset.contentType = "text/vtt; charset=utf-8"
		asset.sidecar = true
	}

	s.serveAsset(w, r, id, asset)
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

	row, err := sqlcgen.New(s.pool).GetThumbnailMediaAssetForServing(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: looking up thumbnail asset", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.serveAsset(w, r, id, serveAsset{
		relPath:     row.RelPath,
		sizeBytes:   row.SizeBytes,
		contentType: thumbnailContentType,
	})
}

func parseRecordingID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// serveAsset は解決済みアセットをディスクから（または X-Accel で）配信する。
// original / encoded / thumbnail で共通（Range と X-Accel-Redirect も同じ経路）。
//
// rel_path は ingest/encode/thumbnail 時に検証済みだが、配信側でも独立に検証する。
// DB に不正な行が入った場合に任意ファイルを読み出させないため。
func (s *Streamer) serveAsset(w http.ResponseWriter, r *http.Request, recordingID int64, asset serveAsset) {
	path, err := mediapath.Resolve(s.cfg.MediaDir, asset.relPath)
	if err != nil {
		slog.Error("streamer: rejecting rel_path outside the media directory",
			"recording_id", recordingID, "rel_path", asset.relPath, "err", err)
		http.NotFound(w, r)
		return
	}

	// X-Accel-Redirect が有効なら、認可判定だけ済ませてバイト転送は
	// リバースプロキシに委ねる。Range の扱いも nginx 側になる。
	// パス検証を通した後に返すのが要点（検証前に返すと任意ファイルを配らせられる）。
	if s.cfg.AccelLocation != "" {
		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Set("X-Accel-Redirect", accelURI(s.cfg.AccelLocation, asset.relPath))
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if asset.sidecar {
				// WebVTT サイドカーは独立した media_assets 行を持たず、字幕の無い
				// 番組では最初から作られない（docs/api/media.md）。存在しなくても
				// 「コミットがあるのにファイルが無い」不整合ではないので WARN しない。
				http.NotFound(w, r)
				return
			}
			// コミット（DB 行）があるのにファイルが無いのは不整合。孤児回収や
			// 外部からの削除で起こりうるので、記録して 404 にする。
			slog.Warn("streamer: media asset row exists but the file is missing",
				"recording_id", recordingID, "rel_path", asset.relPath)
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
			"recording_id", recordingID, "rel_path", asset.relPath)
		http.NotFound(w, r)
		return
	}

	// size_bytes は commit 時に照合した値。実ファイルと違うならコミット後に
	// 改変・切り詰めが起きている。配信は続けるが（ユーザーは録画を見たい）
	// 不整合として記録する。サイドカーは size_bytes を持たない（常に 0）ので
	// 対象外。
	if !asset.sidecar && info.Size() != asset.sizeBytes {
		slog.Warn("streamer: file size differs from the committed size",
			"recording_id", recordingID, "rel_path", asset.relPath,
			"committed", asset.sizeBytes, "actual", info.Size())
	}

	// ServeContent は name から Content-Type を推測しようとするので明示する。
	w.Header().Set("Content-Type", asset.contentType)
	// 録画は一度書いたら変わらないが、ごみ箱からの復元などで同じ URL の
	// 中身が入れ替わりうるので immutable は付けない。
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")

	// name は Content-Type 推測にしか使われない。上で明示済みなので空でよい。
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// lookupAsset は profile の有無で original / encoded を解決する。
func (s *Streamer) lookupAsset(r *http.Request, recordingID int64, profile string) (serveAsset, error) {
	q := sqlcgen.New(s.pool)
	if profile == "" {
		row, err := q.GetOriginalMediaAssetForServing(r.Context(), recordingID)
		if err != nil {
			return serveAsset{}, err
		}
		return serveAsset{
			relPath:     row.RelPath,
			sizeBytes:   row.SizeBytes,
			contentType: contentTypeOriginal,
		}, nil
	}

	row, err := q.GetEncodedMediaAssetForServing(r.Context(), sqlcgen.GetEncodedMediaAssetForServingParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if err != nil {
		return serveAsset{}, err
	}
	return serveAsset{
		relPath:     row.RelPath,
		sizeBytes:   row.SizeBytes,
		contentType: contentTypeForPath(row.RelPath),
	}, nil
}

// contentTypeForPath は派生物の拡張子から Content-Type を決める。
// 不明なら application/octet-stream（ブラウザは <video> で拒否する）。
func contentTypeForPath(relPath string) string {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".mp4", ".m4v":
		return contentTypeMP4
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".ts", ".m2ts":
		return contentTypeOriginal
	default:
		return "application/octet-stream"
	}
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
