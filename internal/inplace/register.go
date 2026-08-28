// Package inplace は storage.media_dir 配下に既に存在するメディアファイルを登録する。
//
// ディザスタリカバリのストレージスキャンと外部ライブラリのインポートの両方から共用される。
// バイト列のコピーや書き換えは一切行わない: 公開とは、検証済みの相対パスに対して recordings と
// media_assets の行をコミットするトランザクションそのものである。
package inplace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// Recording は in-place のアセットグループに紐づく永続メタデータを表す。
// Site と放送 identity タプルが冪等性キーである。
type Recording struct {
	Source            string
	Site              string
	NetworkID         int32
	ServiceID         int32
	EventID           int32
	ServiceName       string
	ChannelType       string
	Channel           string
	Title             string
	ProgramStartAt    time.Time
	ProgramDurationMs int64
	Status            string
	StartedAt         *time.Time
	EndedAt           *time.Time
}

// Asset は既存の 1 ファイルを表す。RelPath は mediaDir からの相対パスである。
// Profile は Kind が encoded のときに限り必須である。
type Asset struct {
	Kind    string
	Profile *string
	RelPath string
}

// Input は 1 件の recording と、それに属する既存ファイル全てを表す。
// Assets は空であってはならない: アセットを持たない recording には復旧上の意味がない。
type Input struct {
	Recording Recording
	Assets    []Asset
}

// Result は Register が公開した行を報告する。
type Result struct {
	RecordingID int64
	AssetIDs    []int64
}

type checkedAsset struct {
	kind     string
	profile  *string
	relPath  string
	sizeByte int64
}

// Register は mediaDir 配下のファイルを検証し、その recording/media_assets 行を 1 つの
// トランザクションで公開する。同じ放送 identity とアセットタプルを繰り返した場合は、
// 重複を作らず既存行を更新する。
func Register(ctx context.Context, pool *pgxpool.Pool, mediaDir string, in Input) (*Result, error) {
	if len(in.Assets) == 0 {
		return nil, fmt.Errorf("in-place registration requires at least one asset")
	}
	if in.Recording.Site == "" {
		return nil, fmt.Errorf("in-place recording site is required")
	}
	realMediaDir, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("resolving media_dir symlinks: %w", err)
	}

	assets := make([]checkedAsset, 0, len(in.Assets))
	for i, asset := range in.Assets {
		checked, err := checkAsset(mediaDir, realMediaDir, asset)
		if err != nil {
			return nil, fmt.Errorf("validating asset %d: %w", i, err)
		}
		assets = append(assets, checked)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning in-place registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// rel_path は保存済みファイルの最も強い identity。既に公開済みなら、その録画が
	// ごみ箱にあっても復元・複製せず同じ recording_id を使う。これにより rescue の
	// 再実行がユーザーの deleted_at を覆さず、rel_path unique にも衝突しない。
	existing := make([]*sqlcgen.GetPublishedInPlaceAssetByRelPathRow, len(assets))
	var recordingID int64
	for i, asset := range assets {
		row, err := q.GetPublishedInPlaceAssetByRelPath(ctx, asset.relPath)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("looking up in-place asset %q: %w", asset.relPath, err)
		}
		if row.Kind != asset.kind || !sameStringPointer(row.Profile, asset.profile) {
			return nil, fmt.Errorf("published asset %q is %s/%v, requested %s/%v",
				asset.relPath, row.Kind, row.Profile, asset.kind, asset.profile)
		}
		if recordingID != 0 && recordingID != row.RecordingID {
			return nil, fmt.Errorf("in-place assets belong to different recordings: %d and %d",
				recordingID, row.RecordingID)
		}
		recordingID = row.RecordingID
		rowCopy := row
		existing[i] = &rowCopy
	}

	if recordingID == 0 {
		recordingID, err = q.UpsertInPlaceRecording(ctx, sqlcgen.UpsertInPlaceRecordingParams{
			Source:            in.Recording.Source,
			Site:              in.Recording.Site,
			NetworkID:         in.Recording.NetworkID,
			ServiceID:         in.Recording.ServiceID,
			EventID:           in.Recording.EventID,
			ServiceName:       in.Recording.ServiceName,
			ChannelType:       in.Recording.ChannelType,
			Channel:           in.Recording.Channel,
			Title:             in.Recording.Title,
			ProgramStartAt:    in.Recording.ProgramStartAt,
			ProgramDurationMs: in.Recording.ProgramDurationMs,
			Status:            in.Recording.Status,
			StartedAt:         in.Recording.StartedAt,
			EndedAt:           in.Recording.EndedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("upserting in-place recording: %w", err)
		}
	}

	result := &Result{RecordingID: recordingID, AssetIDs: make([]int64, len(assets))}
	for i, asset := range assets {
		if existing[i] != nil {
			result.AssetIDs[i] = existing[i].ID
			continue
		}
		assetID, err := q.UpsertInPlaceMediaAsset(ctx, sqlcgen.UpsertInPlaceMediaAssetParams{
			RecordingID: recordingID,
			Kind:        asset.kind,
			Profile:     asset.profile,
			RelPath:     asset.relPath,
			SizeBytes:   asset.sizeByte,
		})
		if err != nil {
			return nil, fmt.Errorf("upserting in-place asset %q: %w", asset.relPath, err)
		}
		result.AssetIDs[i] = assetID
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing in-place registration: %w", err)
	}
	return result, nil
}

func checkAsset(mediaDir, realMediaDir string, asset Asset) (checkedAsset, error) {
	relPath := filepath.ToSlash(filepath.Clean(asset.RelPath))
	if relPath == "." || relPath == "" || filepath.IsAbs(asset.RelPath) {
		return checkedAsset{}, fmt.Errorf("rel_path %q must name a file relative to media_dir", asset.RelPath)
	}

	switch asset.Kind {
	case db.AssetKindEncoded:
		if asset.Profile == nil || strings.TrimSpace(*asset.Profile) == "" {
			return checkedAsset{}, fmt.Errorf("encoded asset %q requires a profile", relPath)
		}
	case db.AssetKindOriginal, db.AssetKindThumbnail:
		if asset.Profile != nil {
			return checkedAsset{}, fmt.Errorf("%s asset %q must not have a profile", asset.Kind, relPath)
		}
	default:
		return checkedAsset{}, fmt.Errorf("unknown asset kind %q", asset.Kind)
	}

	fullPath, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return checkedAsset{}, err
	}
	// Lstat で symlink を拒否する。字句上 media_dir 内でもリンク先が外なら、
	// streamer が後で任意ファイルを開けてしまうため。
	info, err := os.Lstat(fullPath)
	if err != nil {
		return checkedAsset{}, fmt.Errorf("stating %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return checkedAsset{}, fmt.Errorf("%q is not a regular file", relPath)
	}
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return checkedAsset{}, fmt.Errorf("resolving symlinks for %q: %w", relPath, err)
	}
	realRel, err := filepath.Rel(realMediaDir, realPath)
	if err != nil {
		return checkedAsset{}, fmt.Errorf("checking resolved path for %q: %w", relPath, err)
	}
	if realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return checkedAsset{}, fmt.Errorf("resolved path %q escapes media_dir", relPath)
	}

	return checkedAsset{
		kind:     asset.Kind,
		profile:  asset.Profile,
		relPath:  relPath,
		sizeByte: info.Size(),
	}, nil
}

func sameStringPointer(a, b *string) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}
