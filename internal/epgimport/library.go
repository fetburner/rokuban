package epgimport

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/inplace"
	"github.com/fetburner/rokuban/internal/programid"
)

// LibraryItem は 1 件の EPGStation Recorded を表す import 入力。
//
// EPGStation の GET /api/recorded は VideoFile.filename を
// path.basename(filePath) にしてしまい（l3tnun/EPGStation
// src/model/api/RecordedItemUtil.ts で確認）、実ファイルの相対パス
// （サブディレクトリを含む）を返さない。REST だけでは media_dir 配下に
// マウントした実体を特定できないため、この構造体は EPGStation の DB
// （Recorded/VideoFile/Thumbnail の filePath 列）から運用者が一度だけ
// SELECT して書き出す JSON を入力として受け取る形にした
// （`rokuban import epgstation --library-json <file>`。設計判断・要確認:
// 下記コメント）。
type LibraryItem struct {
	// ProgramID は mirakc の programId（EPGStation Recorded.programId）。
	// 無ければ ChannelID + Name + EndAt から合成識別子を作る。
	ProgramID *int64 `json:"programId,omitempty"`
	// ChannelID は Mirakurun 互換の service id（= networkId*100000+serviceId）。
	ChannelID int64 `json:"channelId"`
	// ChannelType/ServiceName/Channel はいずれも EPGStation の
	// RecordedItem 自体は持たず Channel（放送局）リソース側の属性なので、
	// 運用者が SELECT 時に join して埋める前提（recordings 側はいずれも
	// NOT NULL）。省略すると "unknown"/GR にフォールバックして警告する
	// （落とさず埋めるのが安全。docs/runbook/import-epgstation.md 参照）。
	ChannelType string             `json:"channelType"`
	ServiceName string             `json:"serviceName"`
	Channel     string             `json:"channel"`
	StartAt     int64              `json:"startAt"` // UnixtimeMS
	EndAt       int64              `json:"endAt"`   // UnixtimeMS
	Name        string             `json:"name"`
	VideoFiles  []LibraryVideoFile `json:"videoFiles,omitempty"`
	Thumbnails  []LibraryThumbnail `json:"thumbnails,omitempty"`
}

// LibraryVideoFile は VideoFile 1 件。RelPath は media_dir をマウントした
// あとの相対パス（EPGStation の filePath を運用者が変換したもの）。
// Type は EPGStation の VideoFileType（"ts" | "encoded"）。
//
// サイズは持たない: inplace.Register が実ファイルを stat してサイズを
// 求める（internal/inplace/register.go の checkAsset）ので、ここで
// 運用者に計算させて JSON に書かせるフィールドを持つのは二重管理になる。
type LibraryVideoFile struct {
	Type    string `json:"type"`
	RelPath string `json:"relPath"`
}

// LibraryThumbnail は Thumbnail 1 件。
type LibraryThumbnail struct {
	RelPath string `json:"relPath"`
}

// LibraryImportResult は ImportLibrary の結果。
type LibraryImportResult struct {
	Registered int
	Skipped    int
	Warnings   []string
}

// ImportLibrary は EPGStation の Recorded/VideoFile/Thumbnail を rokuban の
// recordings/media_assets へ in-place 登録する。ファイル本体はコピーせず
// （M3-9 / issue #71 の internal/inplace.Register をそのまま共有）、
// mediaDir 配下に既にマウントされている前提の相対パスを検証して登録する。
//
// 冪等性は inplace.Register が持つ 2 つの一意制約に乗る:
//   - media_assets.rel_path（state <> 'deleted' の部分一意）
//   - recordings (site, network_id, service_id, event_id)（deleted_at/
//     superseded_at が NULL の部分一意）
//
// EPGStation の VideoFile.type = "encoded" は rokuban の encoded profile
// 名前空間（rules.encode_profiles / config のプロファイル定義）と対応が
// 取れない —— EPGStation 側にはどのエンコード設定で作られたかを示す
// 名前が無い。誤った profile 名で登録すると配信経路（
// GetEncodedMediaAssetForServing 等）が不整合を起こすため、"encoded" は
// インポートせず警告して捨てる（"ts" だけを kind=original として登録する）。
func ImportLibrary(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string, items []LibraryItem) (LibraryImportResult, error) {
	var res LibraryImportResult
	for _, item := range items {
		var assets []inplace.Asset
		for _, vf := range item.VideoFiles {
			switch vf.Type {
			case "ts":
				assets = append(assets, inplace.Asset{Kind: db.AssetKindOriginal, RelPath: vf.RelPath})
			case "encoded":
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"recorded %q: videoFile %q is type=encoded, which has no equivalent rokuban encode profile name — skipped (only type=ts is imported)",
					item.Name, vf.RelPath))
			default:
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"recorded %q: videoFile %q has unknown type %q — skipped", item.Name, vf.RelPath, vf.Type))
			}
		}
		// media_assets is UNIQUE NULLS NOT DISTINCT (recording_id, kind,
		// profile), so a recording can only have one row with
		// kind='thumbnail' (profile is always NULL for thumbnails). EPGStation's
		// Thumbnail is 1:N on Recorded (one per video file, so encoded
		// recordings routinely have several) — importing more than the first
		// collides on that constraint and aborts the whole item's
		// registration, so only the first is kept and the rest are warned
		// about instead of silently dropped.
		if len(item.Thumbnails) > 0 {
			assets = append(assets, inplace.Asset{Kind: db.AssetKindThumbnail, RelPath: item.Thumbnails[0].RelPath})
		}
		if len(item.Thumbnails) > 1 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"recorded %q: only the first of %d thumbnails is imported (media_assets allows one thumbnail per recording) — the rest were skipped",
				item.Name, len(item.Thumbnails)))
		}

		if len(assets) == 0 {
			res.Skipped++
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"recorded %q has no importable asset (no type=ts videoFile, no thumbnail) — skipped (in-place registration requires at least one file)", item.Name))
			continue
		}

		networkID, serviceID, eventID := resolveEventIdentity(item.ProgramID, item.ChannelID, item.Name, item.EndAt)

		channelType := item.ChannelType
		if channelType == "" {
			channelType = "GR"
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"recorded %q has no channelType — defaulted to GR", item.Name))
		}
		serviceName := item.ServiceName
		if serviceName == "" {
			serviceName = "unknown"
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"recorded %q has no serviceName — defaulted to \"unknown\"", item.Name))
		}
		channel := item.Channel
		if channel == "" {
			channel = "unknown"
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"recorded %q has no channel — defaulted to \"unknown\"", item.Name))
		}

		in := inplace.Input{
			Recording: inplace.Recording{
				Source:            db.SourceManual,
				Site:              site,
				NetworkID:         networkID,
				ServiceID:         serviceID,
				EventID:           eventID,
				ServiceName:       serviceName,
				ChannelType:       channelType,
				Channel:           channel,
				Title:             item.Name,
				ProgramStartAt:    time.UnixMilli(item.StartAt),
				ProgramDurationMs: item.EndAt - item.StartAt,
				Status:            db.RecordingStatusFinished,
			},
			Assets: assets,
		}
		if _, err := inplace.Register(ctx, pool, mediaDir, in); err != nil {
			return res, fmt.Errorf("registering recorded %q: %w", item.Name, err)
		}
		res.Registered++
	}
	return res, nil
}

// resolveEventIdentity は recordings の (network_id, service_id, event_id) を
// 決める。programId があれば mirakc の合成規則（internal/programid.
// SplitProgramID）でそのまま分解する。無ければ channelId を分解し、
// event_id は name+endAt から決定的に合成する合成識別子にする（放送由来の
// 本物の event_id ではない —— 設計判断・要確認: internal/epgimport の
// パッケージコメントおよび import 結果レポート参照）。
func resolveEventIdentity(programID *int64, channelID int64, name string, endAt int64) (networkID, serviceID, eventID int32) {
	if programID != nil {
		n, s, e := programid.SplitProgramID(*programID)
		return int32(n), int32(s), int32(e)
	}
	n, s := programid.SplitServiceID(channelID)
	return int32(n), int32(s), syntheticEventID(name, endAt)
}

// syntheticEventID は放送由来の event_id を持たない行（programId の無い
// EPGStation Recorded）向けに、name/endAt から決定的に合成する負の擬似
// event_id を作る。同じ入力を再インポートしても同じ値になる
// （recordings_unique_active_event に乗って冪等になる）ための仕掛け。
// 本物の mirakc event_id と衝突しないよう常に負値にする。
func syntheticEventID(name string, endAt int64) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	_, _ = fmt.Fprintf(h, "|%d", endAt)
	// int32 の負範囲に収める（1..2^30 のどこか）。0 は avoid（-0 は無意味）。
	return -(int32(h.Sum32()%(1<<30)) + 1)
}
