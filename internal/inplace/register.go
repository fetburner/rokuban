// Package inplace は storage.media_dir 配下に既に存在するメディアファイルを登録する。
//
// ディザスタリカバリのストレージスキャンと外部ライブラリのインポートの両方から共用される。
// バイト列のコピーや書き換えは一切行わない: 公開とは、検証済みの相対パスに対して recordings と
// media_assets の行をコミットするトランザクションそのものである。
package inplace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fetburner/rokuban/internal/medialock"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// Recording は in-place 登録する録画の永続メタデータを表す。
type Recording = store.InPlaceRecording

// Asset は既存の 1 ファイルを表す。RelPath は mediaDir からの相対パスである。
type Asset = store.InPlaceAsset

// Input は 1 件の recording と、それに属する既存ファイル全てを表す。
type Input = store.InPlaceInput

// Result は Register が公開した行を報告する。
type Result = store.InPlaceResult

// Register は mediaDir 配下のファイルを検証し、その recording/media_assets 行を 1 つの
// トランザクションで公開する。同じ放送 identity とアセットタプルを繰り返した場合は、
// 重複を作らず既存行を更新する。
func Register(ctx context.Context, publisher store.MediaPublish, mediaDir string, in Input) (*Result, error) {
	if len(in.Assets) == 0 {
		return nil, fmt.Errorf("in-place registration requires at least one asset")
	}
	if publisher == nil {
		return nil, fmt.Errorf("in-place registration requires a media publisher")
	}
	if in.Recording.Site == "" {
		return nil, fmt.Errorf("in-place recording site is required")
	}
	realMediaDir, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("resolving media_dir symlinks: %w", err)
	}

	paths := make([]string, len(in.Assets))
	for i, asset := range in.Assets {
		// checkAsset と同じ正規化。LockPaths は非正規 path を拒否する。
		paths[i] = filepath.ToSlash(filepath.Clean(asset.RelPath))
	}
	release, err := medialock.LockPaths(ctx, mediaDir, paths)
	if err != nil {
		return nil, fmt.Errorf("locking in-place assets: %w", err)
	}
	defer func() { _ = release() }()

	assets := make([]store.InPlaceAsset, 0, len(in.Assets))
	for i, asset := range in.Assets {
		checked, err := checkAsset(mediaDir, realMediaDir, asset)
		if err != nil {
			return nil, fmt.Errorf("validating asset %d: %w", i, err)
		}
		assets = append(assets, checked)
	}
	storeInput := in
	storeInput.Assets = assets
	result, err := publisher.RegisterInPlace(ctx, storeInput)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func checkAsset(mediaDir, realMediaDir string, asset Asset) (store.InPlaceAsset, error) {
	relPath := filepath.ToSlash(filepath.Clean(asset.RelPath))
	if relPath == "." || relPath == "" || filepath.IsAbs(asset.RelPath) {
		return store.InPlaceAsset{}, fmt.Errorf("rel_path %q must name a file relative to media_dir", asset.RelPath)
	}

	switch asset.Kind {
	case store.AssetKindEncoded:
		if asset.Profile == nil || strings.TrimSpace(*asset.Profile) == "" {
			return store.InPlaceAsset{}, fmt.Errorf("encoded asset %q requires a profile", relPath)
		}
	case store.AssetKindOriginal, store.AssetKindThumbnail, store.AssetKindSeekTiles:
		if asset.Profile != nil {
			return store.InPlaceAsset{}, fmt.Errorf("%s asset %q must not have a profile", asset.Kind, relPath)
		}
	default:
		return store.InPlaceAsset{}, fmt.Errorf("unknown asset kind %q", asset.Kind)
	}

	fullPath, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return store.InPlaceAsset{}, err
	}
	// Lstat で symlink を拒否する。字句上 media_dir 内でもリンク先が外なら、
	// streamer が後で任意ファイルを開けてしまうため。
	info, err := os.Lstat(fullPath)
	if err != nil {
		return store.InPlaceAsset{}, fmt.Errorf("stating %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return store.InPlaceAsset{}, fmt.Errorf("%q is not a regular file", relPath)
	}
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return store.InPlaceAsset{}, fmt.Errorf("resolving symlinks for %q: %w", relPath, err)
	}
	realRel, err := filepath.Rel(realMediaDir, realPath)
	if err != nil {
		return store.InPlaceAsset{}, fmt.Errorf("checking resolved path for %q: %w", relPath, err)
	}
	if realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return store.InPlaceAsset{}, fmt.Errorf("resolved path %q escapes media_dir", relPath)
	}

	return store.InPlaceAsset{
		Kind:      asset.Kind,
		Profile:   asset.Profile,
		RelPath:   relPath,
		SizeBytes: info.Size(),
	}, nil
}
