package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/inplace"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// statRescueCandidate lets tests remove a candidate after the walk observes it.
var statRescueCandidate = os.Lstat

// rescueStorage は catalog が 1 世代も残っていないときに動画ファイルをスキャンする。
// ファイル確認から RegisterInPlace の commit まで相対パスのロックを保持する。
func rescueStorage(ctx context.Context, mediaPublish store.MediaPublish, mediaDir string, registrySites []string) (*RescueResult, error) {
	realMediaDir, err := filepath.EvalSymlinks(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("resolving media_dir symlinks for rescue: %w", err)
	}

	result := &RescueResult{}
	catalogDir := filepath.Clean(Dir(realMediaDir))
	unknownSiteCounts := map[string]int{}

	err = filepath.WalkDir(realMediaDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if filepath.Clean(path) == catalogDir || filepath.Clean(path) == filepath.Join(realMediaDir, mediapath.MediaRelPathLockDirName) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || mediapath.IsIngestTempFile(entry.Name()) ||
			mediapath.IsMediaRelPathLockFile(entry.Name()) || mediapath.IsEncodeTempFile(entry.Name()) ||
			mediapath.IsGeneratedAssetTempFile(entry.Name()) {
			return nil
		}

		kind, profile, ok := rescueAssetKind(entry.Name())
		if !ok {
			return nil
		}
		info, err := statRescueCandidate(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
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

		fileSite, unknownSite := classifySiteForRescuedFile(relPath, registrySites)
		if fileSite == "" {
			slog.Warn("rescue: skipping file without a sites/{site}/ prefix; site cannot be inferred",
				"rel_path", relPath)
			result.SkippedFilesWithoutSitePrefix++
			return nil
		}

		networkID, serviceID, eventID := syntheticBroadcastIdentity(relPath)
		at := info.ModTime().UTC()
		title := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
		if title == "" {
			title = relPath
		}
		_, err = inplace.Register(ctx, mediaPublish, realMediaDir, inplace.Input{
			Recording: store.InPlaceRecording{
				// 再走査には予約も program_intents も残っていない。ユーザーの明示的な
				// 意図を示す材料が無いので manual ではなく unattributed とする。
				Source: reservation.SourceUnattributed,
				Site:   fileSite, NetworkID: networkID, ServiceID: serviceID, EventID: eventID,
				ServiceName: "Recovered file (metadata unavailable)",
				ChannelType: "GR", Channel: "unknown", Title: title, ProgramStartAt: at,
				Status: db.RecordingStatusFinished, StartedAt: &at, EndedAt: &at,
			},
			Assets: []inplace.Asset{{Kind: kind, Profile: profile, RelPath: relPath}},
		})
		if errors.Is(err, os.ErrNotExist) {
			result.MissingMediaFiles++
			return nil
		}
		if err != nil {
			return fmt.Errorf("registering rescue candidate %q: %w", relPath, err)
		}
		result.Recordings++
		result.MediaAssets++
		if unknownSite {
			unknownSiteCounts[fileSite]++
		}
		return nil
	})

	for _, site := range slices.Sorted(maps.Keys(unknownSiteCounts)) {
		slog.Warn("rescue: recovered files under a sites/ prefix that is not in the mirakc registry; using it anyway",
			"site", site, "count", unknownSiteCounts[site])
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
		p := config.RescueEncodedProfilePrefix + strings.TrimPrefix(ext, ".")
		return db.AssetKindEncoded, &p, true
	default:
		return "", nil, false
	}
}

// SiteRelPathPrefix は原本 rel_path の site 名前空間の固定 1 段目。
const SiteRelPathPrefix = "sites/"

// SiteRelPath は原本の rel_path に site 名前空間を前置する。
func SiteRelPath(site, relPath string) (string, error) {
	if relPath == "" || relPath == "." || relPath == "/" || strings.HasSuffix(relPath, "/") {
		return "", fmt.Errorf("rel_path %q does not name a file", relPath)
	}
	base := filepath.Base(relPath)
	if base == "." || base == ".." {
		return "", fmt.Errorf("rel_path %q does not name a file", relPath)
	}
	if mediapath.IsIngestTempFile(base) || mediapath.IsMediaRelPathLockFile(base) ||
		mediapath.IsEncodeTempFile(base) || mediapath.IsGeneratedAssetTempFile(base) {
		return "", fmt.Errorf("rel_path %q uses reserved storage filename %q", relPath, base)
	}
	return SiteRelPathPrefix + site + "/" + relPath, nil
}

// classifySiteForRescuedFile は走査で見つかったファイルの site を決める。
func classifySiteForRescuedFile(relPath string, registrySites []string) (site string, unknownSite bool) {
	rest, ok := strings.CutPrefix(relPath, SiteRelPathPrefix)
	if !ok {
		return "", false
	}
	prefixSite, _, ok := strings.Cut(rest, "/")
	if !ok || prefixSite == "" {
		return "", false
	}
	return prefixSite, !slices.Contains(registrySites, prefixSite)
}

// syntheticBroadcastIdentity は相対パスを安定した負の放送 identity に写像する。
func syntheticBroadcastIdentity(relPath string) (networkID, serviceID, eventID int32) {
	sum := sha256.Sum256([]byte(relPath))
	part := func(offset int) int32 {
		return -int32(binary.BigEndian.Uint32(sum[offset:offset+4])&0x3fffffff) - 1
	}
	return part(0), part(4), part(8)
}
