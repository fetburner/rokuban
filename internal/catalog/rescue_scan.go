package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/inplace"
	"github.com/fetburner/rokuban/internal/reservation"
)

// rescueStorage は catalog が 1 世代も残っていないときに、認識可能な動画ファイルをスキャンする。
// 各ファイルは 1 件の recording になり、既知の事実は意図的に相対パス・ファイル名・サイズ・mtime
// のみに絞られる。`catalog/` は決してスキャンしない: catalog の世代はメタデータであり、
// media asset ではないため。`sites/` は SkipDir にしない（walk 対象。原本の名前空間で、
// site 付きで走査する対象そのもの。docs/storage/contract.md §「原本の sites/{site}/ 前置」）。
//
// site は `--site` の値で、`sites/{site}/` 前置を持たないファイル（前置導入前の
// 単一 site 時代の ingest）にだけ使う。前置を持つファイルは
// classifySiteForRescuedFile が prefix から site を決める。registrySites は
// `mirakcs:` レジストリの site 名一覧（typo/ゴミディレクトリの検出用。
// classifySiteForRescuedFile 参照）。
//
// 前置が site と食い違うファイル・レジストリに無い site を持つファイルは
// ファイルごとではなく site ごとに 1 回だけ log する（`rescue --site tokyo` を
// 大半 takamatsu のアーカイブに対して実行すると、ファイルの数だけ Info が出て
// 読みにくくなるため）。
func rescueStorage(ctx context.Context, pool *pgxpool.Pool, mediaDir, site string, registrySites []string) (*RescueResult, error) {
	realMediaDir, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("resolving media_dir symlinks for rescue: %w", err)
	}

	result := &RescueResult{}
	catalogDir := filepath.Clean(Dir(realMediaDir))
	crossSiteCounts := map[string]int{}
	unknownSiteCounts := map[string]int{}

	err = filepath.WalkDir(realMediaDir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		relPath, err := filepath.Rel(realMediaDir, path)
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

		fileSite, crossSite, unknownSite := classifySiteForRescuedFile(relPath, site, registrySites)

		_, err = inplace.Register(ctx, pool, realMediaDir, inplace.Input{
			Recording: inplace.Recording{
				// ストレージ再スキャンには予約も program_intents も残っていない。
				// ユーザーの明示的な意図を示す材料が無いので manual ではなく
				// unattributed として永続化する。
				Source:    reservation.SourceUnattributed,
				Site:      fileSite,
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
		// 登録が成功した後でだけ数える --- 途中で失敗したファイルまで
		// summary の count に含めると、実際に復元できた件数と log の
		// count が食い違う。
		switch {
		case unknownSite:
			unknownSiteCounts[fileSite]++
		case crossSite:
			crossSiteCounts[fileSite]++
		}
		return nil
	})

	// site の内訳は walk が最後まで終わらなかった run でこそ運用者が読みたい
	// （何が起きて途中で止まったかの手がかりになる）ので、エラーで return する
	// 前に必ず出す。
	for _, s := range slices.Sorted(maps.Keys(crossSiteCounts)) {
		slog.Info("rescue: recovered files under a different site's sites/ prefix than --site; using the prefix",
			"site", s, "count", crossSiteCounts[s], "flag_site", site)
	}
	for _, s := range slices.Sorted(maps.Keys(unknownSiteCounts)) {
		slog.Warn("rescue: recovered files under a sites/ prefix that is not in the mirakc registry; using it anyway",
			"site", s, "count", unknownSiteCounts[s])
	}

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

// SiteRelPathPrefix は原本 rel_path の site 名前空間の固定 1 段目
// （docs/storage/contract.md §「原本の sites/{site}/ 前置」）。書き手は
// internal/worker/ingest.go の determineRelPath、読み手はここ
// （classifySiteForRescuedFile）の 2 つだけなので、リテラルの重複を避けてこの定数で
// 揃える。
const SiteRelPathPrefix = "sites/"

// classifySiteForRescuedFile は走査で見つかったファイルの site を決める
// （issue #533）。戻り値の site が録画として登録する値、crossSite は site が
// flagSite と異なること、unknownSite は site が registrySites に無いこと
// （crossSite の場合のみ意味を持つ）を表す。
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
// takamatsu として復元されるべきである。除外すると単に「site を復元し忘れる」
// では済まない --- 台帳が知らないファイルは削除エンジンの通常の掃除対象になり、
// 2 週間ほどで無条件に消える（docs/storage.md §8、理由は
// docs/storage/rescue.md 参照）。
//
// prefix の site が registrySites に無ければ typo やゴミディレクトリの疑いがある
// （正規の site は必ずレジストリに載っている）。それでも録画としては復元する ---
// `internal/api` の一覧・削除は site 非依存なので、間違った行として見えても
// 運用者が UI から消せる。消してから復元し直す方が、消えたことに気付けない
// より安全（呼び出し元がこの区別を Warn/Info の使い分けに使う）。
func classifySiteForRescuedFile(relPath, flagSite string, registrySites []string) (site string, crossSite, unknownSite bool) {
	rest, ok := strings.CutPrefix(relPath, SiteRelPathPrefix)
	if !ok {
		return flagSite, false, false
	}
	prefixSite, _, ok := strings.Cut(rest, "/")
	if !ok || prefixSite == "" {
		return flagSite, false, false
	}
	if prefixSite == flagSite {
		return prefixSite, false, false
	}
	return prefixSite, true, !slices.Contains(registrySites, prefixSite)
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
