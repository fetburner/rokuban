package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// transferSource は原本の転送元を、ローカルのファイルか内部 URL の streamer から読む形で抽象化する。
// 原本のコピーと照合・測定は、この 2 つだけを通して読む（hashSourceFrom と copyTransferSlice）。
type transferSource interface {
	// precheck は読まずに分かる転送元のサイズを返す。分からないときは -1 を返す。
	// ローカルはここで通常ファイルであることも確かめる。
	precheck() (int64, error)
	// open は offset から末尾までを読む reader を返す。offset が 0 なら先頭から読む。
	open(ctx context.Context, offset int64) (io.ReadCloser, error)
}

// localTransferSource は、マウントした置き場の原本ファイルを転送元にする。
type localTransferSource struct {
	path string
}

func (s localTransferSource) precheck() (int64, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		return -1, fmt.Errorf("stating transfer source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return -1, errors.New("transfer source is not a regular file")
	}
	return info.Size(), nil
}

func (s localTransferSource) open(_ context.Context, offset int64) (io.ReadCloser, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("opening transfer source: %w", err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("seeking transfer source to checkpoint offset %d: %w", offset, err)
		}
	}
	return f, nil
}

// remoteTransferSource は、マウントしていない原本を、転送元の置き場の internal_url の streamer から読む。
// 配信 route は、その置き場の有効な原本を返す。SourceRelPath と違うファイルを返しうるが、
// その場合もサイズと SHA-256 の照合（コピーの完了の後、または照合の終わり）が不一致として扱う。
type remoteTransferSource struct {
	url         string
	sourceStore string
}

// newRemoteTransferSource は internal_url の streamer 上の、置き場 sourceStore の録画 recordingID の原本 URL を組み立てる。
func newRemoteTransferSource(internalURL, sourceStore string, recordingID int64) remoteTransferSource {
	return remoteTransferSource{
		url: strings.TrimSuffix(internalURL, "/") + "/api/media/stores/" + url.PathEscape(sourceStore) +
			"/recordings/" + strconv.FormatInt(recordingID, 10) + "/file",
		sourceStore: sourceStore,
	}
}

// precheck は、リモートのサイズを読む前には知らないので -1 を返す。サイズは読み終えた後に照合する。
func (s remoteTransferSource) precheck() (int64, error) { return -1, nil }

// open は offset から末尾までを読む reader を返す。本体が途中で切れたら、切れた位置から張り直す。
func (s remoteTransferSource) open(ctx context.Context, offset int64) (io.ReadCloser, error) {
	body, err := s.openOnce(ctx, offset)
	if err != nil {
		return nil, err
	}
	return &resumingRemoteReader{ctx: ctx, source: s, body: body, offset: offset}, nil
}

// openOnce は offset > 0 のとき Range: bytes=offset- を付けて 1 回だけ要求する。応答は checkRemoteTransferResponse で検査する。
func (s remoteTransferSource) openOnce(ctx context.Context, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("building transfer source request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := transferHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting original from internal_url of store %q: %w", s.sourceStore, err)
	}
	if err := checkRemoteTransferResponse(resp, offset); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// remoteTransferMaxReconnects は、読み進めないまま張り直す回数の上限。読み進めた後の切断は数えない。
const remoteTransferMaxReconnects = 3

// remoteTransferReconnectDelay は張り直す前に待つ時間。テストで 0 にできるように var にしてある。
var remoteTransferReconnectDelay = time.Second

// resumingRemoteReader は、内部 URL の原本の本体が途中で切れたとき、切れた位置から Range で読み直す reader。
// 切断は body の Read のエラー（net/http では多くの場合 io.ErrUnexpectedEOF）で検出する。
// 張り直した後に 1 バイトでも進めば回数をリセットするので、長い原本が何度切れても転送は続く。
type resumingRemoteReader struct {
	ctx        context.Context
	source     remoteTransferSource
	body       io.ReadCloser
	offset     int64 // 原本の先頭からの、次に読む位置
	noProgress int   // 読み進めないまま張り直した回数
}

// Read は現在の body から読む。切断なら張り直してから続きを読む。io.EOF はそのまま返す。
// 張り直しに失敗し上限に達したら、その原因を包んだエラーを返す。
func (r *resumingRemoteReader) Read(p []byte) (int, error) {
	for {
		n, err := r.body.Read(p)
		r.offset += int64(n)
		if n > 0 {
			r.noProgress = 0
		}
		if err == nil || err == io.EOF {
			return n, err
		}
		if r.ctx.Err() != nil {
			return n, err
		}
		if rerr := r.reconnect(err); rerr != nil {
			return n, rerr
		}
		if n > 0 {
			return n, nil
		}
	}
}

// reconnect は切れた body を閉じ、待ってから r.offset から開き直す。
// 開き直しの失敗も読み進めない試行として数え、上限に達したら cause を包んで返す。
func (r *resumingRemoteReader) reconnect(cause error) error {
	_ = r.body.Close()
	for {
		if r.noProgress >= remoteTransferMaxReconnects {
			return fmt.Errorf("reading original from internal_url of store %q after %d reconnects: %w", r.source.sourceStore, r.noProgress, cause)
		}
		r.noProgress++
		timer := time.NewTimer(remoteTransferReconnectDelay)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return r.ctx.Err()
		case <-timer.C:
		}
		body, err := r.source.openOnce(r.ctx, r.offset)
		if err == nil {
			r.body = body
			return nil
		}
		cause = err
	}
}

// Close は現在の body を閉じる。
func (r *resumingRemoteReader) Close() error {
	return r.body.Close()
}

// checkRemoteTransferResponse は、Range の要求に対する応答が offset からの原本として使えるかを検査する。
// offset > 0 で 200 が返ると、先頭からの全体が offset 以降に書き込まれて内容が壊れるので、拒む。
func checkRemoteTransferResponse(resp *http.Response, offset int64) error {
	if resp.Header.Get("X-Accel-Redirect") != "" {
		return errors.New("internal_url answered with X-Accel-Redirect and no bytes; " +
			"point internal_url at a streamer that serves the original directly, not one behind X-Accel-Redirect")
	}
	if offset == 0 {
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			return nil
		}
		return fmt.Errorf("original request returned status %d, want 200", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("original request from offset %d returned status %d, want 206 Partial Content", offset, resp.StatusCode)
	}
	start, err := contentRangeStart(resp.Header.Get("Content-Range"))
	if err != nil {
		return err
	}
	if start != offset {
		return fmt.Errorf("original range starts at byte %d, want %d", start, offset)
	}
	return nil
}

// contentRangeStart は "bytes first-last/complete" の first を返す。
func contentRangeStart(v string) (int64, error) {
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, fmt.Errorf("range response has Content-Range %q, which is not a byte range", v)
	}
	first, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, fmt.Errorf("range response has Content-Range %q without a first byte", v)
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("range response has Content-Range %q: %w", v, err)
	}
	return start, nil
}

// transferHTTPClient は内部 URL の streamer から原本を読む。原本は数 GB になるので全体の timeout は付けず、
// 接続とレスポンスヘッダの待ちだけに上限を置く。
var transferHTTPClient = &http.Client{Transport: newTransferHTTPTransport()}

func newTransferHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

// transferSourceStores は PrepareMediaAssetTransfer に渡す転送元の置き場を返す。
// マウントしている置き場と、マウントしていないが internal_url を持つ置き場。
// internal_url の無い置き場は転送元にならない。派生物の転送元はマウントした置き場に限られる（SQL 側）。
func transferSourceStores(cfg config.StorageConfig, mounted []string) []string {
	sources := slices.Clone(mounted)
	for _, name := range cfg.FSStoreNames() {
		if cfg.Stores[name].InternalURL != "" && !slices.Contains(mounted, name) {
			sources = append(sources, name)
		}
	}
	return sources
}

// originalTransferSource は原本の転送元を選ぶ。マウントしていればローカルのファイル、していなければ
// 内部 URL の streamer。どちらも無い置き場は PrepareMediaAssetTransfer が転送元に選ばないので、ここに来たら内部の不整合。
func (w *StorageTransferWorker) originalTransferSource(transfer store.MediaAssetTransfer) (transferSource, error) {
	if root := w.Roots[transfer.SourceStore]; root != "" {
		if err := requireMediaStoreMarker(root, transfer.SourceStore, "reading storage transfer source"); err != nil {
			return nil, err
		}
		path, err := mediapath.Resolve(root, transfer.SourceRelPath)
		if err != nil {
			return nil, fmt.Errorf("resolving transfer source: %w", err)
		}
		return localTransferSource{path: path}, nil
	}
	internalURL := w.Config.Stores[transfer.SourceStore].InternalURL
	if internalURL == "" {
		return nil, fmt.Errorf("source store %q is not mounted and has no internal_url", transfer.SourceStore)
	}
	return newRemoteTransferSource(internalURL, transfer.SourceStore, transfer.RecordingID), nil
}
