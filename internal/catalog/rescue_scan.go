package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/inplace"
)

// rescueStorage は catalog が 1 世代も残っていないときに、認識可能な動画ファイルをスキャンする。
// 各ファイルは 1 件の recording になり、既知の事実は意図的に相対パス・ファイル名・サイズ・mtime
// のみに絞られる。`catalog/` は決してスキャンしない: catalog の世代はメタデータであり、
// media asset ではないため。`sites/` は SkipDir にしない（walk 対象。原本の名前空間で、
// site 付きで走査する対象そのもの。docs/storage/contract.md §「原本の sites/{site}/ 前置」）。
//
// site は `--site` の値で、`sites/{site}/` 前置を持たないファイル（前置導入前の
// 単一 site 時代の ingest）にだけ使う。前置を持つファイルは
// siteForRescuedFile が prefix から site を決める。
func rescueStorage(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string) (*RescueResult, error) {
	result := &RescueResult{}
	catalogDir := filepath.Clean(Dir(mediaDir))

	err := filepath.WalkDir(mediaDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if filepath.Clean(path) == catalogDir {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		kind, profile, ok := rescueAssetKind(entry.Name())
		if !ok {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stating rescue candidate %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relPath, err := filepath.Rel(mediaDir, path)
		if err != nil {
			return fmt.Errorf("making rescue path relative for %q: %w", path, err)
		}
		relPath = filepath.ToSlash(relPath)
		networkID, serviceID, eventID := syntheticBroadcastIdentity(relPath)
		at := info.ModTime().UTC()
		title := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
		if title == "" {
			title = relPath
		}

		_, err = inplace.Register(ctx, pool, mediaDir, inplace.Input{
			Recording: inplace.Recording{
				Source:    "manual",
				Site:      siteForRescuedFile(relPath, site),
				NetworkID: networkID, ServiceID: serviceID, EventID: eventID,
				ServiceName: "Recovered file (metadata unavailable)",
				// recordings.channel_type には現状 unknown 値が無い。負の synthetic identity と
				// 明示的な service/channel ラベルにより、このプレースホルダーが実際の EPG
				// メタデータと誤認されることを防いでいる。
				ChannelType:    "GR",
				Channel:        "unknown",
				Title:          title,
				ProgramStartAt: at,
				Status:         db.RecordingStatusFinished,
				StartedAt:      &at,
				EndedAt:        &at,
			},
			Assets: []inplace.Asset{{Kind: kind, Profile: profile, RelPath: relPath}},
		})
		if err != nil {
			return fmt.Errorf("registering rescue candidate %q: %w", relPath, err)
		}
		result.Recordings++
		result.MediaAssets++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning media_dir for rescue: %w", err)
	}
	return result, nil
}

func rescueAssetKind(name string) (kind string, profile *string, ok bool) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".ts", ".m2ts":
		return db.AssetKindOriginal, nil, true
	case ".mp4", ".mkv", ".webm":
		p := "rescue-" + strings.TrimPrefix(ext, ".")
		return db.AssetKindEncoded, &p, true
	default:
		return "", nil, false
	}
}

// siteRelPathPrefix は原本 rel_path の site 名前空間の固定 1 段目
// （docs/storage/contract.md §「原本の sites/{site}/ 前置」）。
const siteRelPathPrefix = "sites/"

// siteForRescuedFile は走査で見つかったファイルの site を決める（issue #533）。
//
// relPath が `sites/{site}/...` 前置を持てば、その prefix を正として使う ---
// 前置は ingest（determineRelPath）が実際に書いた site の記録であり、
// 呼び出し側が渡した flagSite（`rescue --site` の値）より確からしい。前置が
// 無ければ前置導入（M4-14）より前の ingest なので flagSite にフォールバックする
// （前置導入前は単一 site 構成でしか運用できなかったので、単一の site 名を
// `--site` に渡す運用で正しく復元できる）。
//
// **prefix と flagSite が食い違っても止めない。** アーカイブは全 site で共有される
// 単一のストレージ（docs/configuration.md §mirakc レジストリ）なので、1 回の
// スキャンに複数 site のファイルが混在するのは正常 --- `rescue --site tokyo` を
// 実行しても、ディスク上に takamatsu の前置ファイルがあればそれも見つかり、
// takamatsu として復元されるべきである。除外すると災害復旧でその site の録画
// だけ黙って復元されなくなる（docs/storage.md §8「黙って切り捨てない」に反する）。
// 食い違いは運用者が結果に驚かないよう Info ログにだけ残す。
func siteForRescuedFile(relPath, flagSite string) string {
	rest, ok := strings.CutPrefix(relPath, siteRelPathPrefix)
	if !ok {
		return flagSite
	}
	prefixSite, _, ok := strings.Cut(rest, "/")
	if !ok || prefixSite == "" {
		return flagSite
	}
	if prefixSite != flagSite {
		slog.Info("rescue: recovered file's sites/ prefix differs from --site; using the prefix",
			"rel_path", relPath, "prefix_site", prefixSite, "flag_site", flagSite)
	}
	return prefixSite
}

// syntheticBroadcastIdentity は相対パスを安定した負の identity タプルへ写像する。実際の
// 放送 ID は非負であり、93 ビットのハッシュを使うことで、このフォールバック専用の第 2 の
// identity テーブルを追加せずに rescue されたパス同士を分離できる。
func syntheticBroadcastIdentity(relPath string) (networkID, serviceID, eventID int32) {
	sum := sha256.Sum256([]byte(relPath))
	part := func(offset int) int32 {
		return -int32(binary.BigEndian.Uint32(sum[offset:offset+4])&0x3fffffff) - 1
	}
	return part(0), part(4), part(8)
}
