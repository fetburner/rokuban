package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/inplace"
)

// rescueStorage scans recognizable video files when no catalog survives. Each file becomes one
// deliberately sparse recording whose known facts are the relative path, filename, size and mtime.
// `catalog/` is never scanned: catalog generations are metadata, not media assets.
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
				Site:      site,
				NetworkID: networkID, ServiceID: serviceID, EventID: eventID,
				ServiceName: "Recovered file (metadata unavailable)",
				// recordings.channel_type currently has no unknown value. The negative synthetic
				// identity and explicit service/channel labels prevent this placeholder from being
				// mistaken for real EPG metadata.
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

// syntheticBroadcastIdentity maps a relative path to a stable negative identity tuple. Real
// broadcast IDs are non-negative; using 93 hash bits keeps rescued paths separate without adding
// a second identity table solely for the fallback path.
func syntheticBroadcastIdentity(relPath string) (networkID, serviceID, eventID int32) {
	sum := sha256.Sum256([]byte(relPath))
	part := func(offset int) int32 {
		return -int32(binary.BigEndian.Uint32(sum[offset:offset+4])&0x3fffffff) - 1
	}
	return part(0), part(4), part(8)
}
