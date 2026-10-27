package fsink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

const verificationCheckpointSuffix = ".verify"

// VerificationCheckpointPath は既存 canonical file の検証用 checkpoint の基底パスを返す。
func VerificationCheckpointPath(tempPath string) string {
	return tempPath + verificationCheckpointSuffix
}

// RemoveVerificationCheckpoint は既存 canonical file の検証用 checkpoint を削除する。
func RemoveVerificationCheckpoint(tempPath string) error {
	if err := RemoveCheckpoint(VerificationCheckpointPath(tempPath)); err != nil {
		return fmt.Errorf("removing verification checkpoint: %w", err)
	}
	return nil
}

// VerifyExistingFile は destination が無ければ false を返し、存在する場合は expectedSHA256 と照合する。
// destination のハッシュ計算は context で中断でき、checkpoint から次回に再開する。
// 同じ destination を別のサイズ・時刻・filesystem identity で開いた場合は checkpoint を破棄する。
func VerifyExistingFile(ctx context.Context, tempPath, destinationPath string, expectedSize int64, expectedSHA256 []byte, check func() error) (bool, error) {
	if len(expectedSHA256) != sha256.Size {
		return false, fmt.Errorf("expected SHA-256 has %d bytes, want %d", len(expectedSHA256), sha256.Size)
	}
	info, err := os.Lstat(destinationPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := RemoveVerificationCheckpoint(tempPath); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stating existing destination %q: %w", destinationPath, err)
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return false, errDestinationExists(tempPath, destinationPath)
	}

	hasher := sha256.New()
	read, complete, err := ReplayFileWithCheckpoint(ctx, destinationPath, VerificationCheckpointPath(tempPath), hasher, check)
	if err != nil {
		return false, err
	}
	if !complete {
		return false, errors.New("existing destination verification ended without completion")
	}
	if read != expectedSize {
		return false, errDestinationExists(tempPath, destinationPath)
	}
	if !bytes.Equal(hasher.Sum(nil), expectedSHA256) {
		return false, errDestinationExists(tempPath, destinationPath)
	}
	return true, nil
}

func fileIdentity(path string, info os.FileInfo) string {
	parts := []string{
		filepath.Clean(path),
		info.Mode().String(),
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatInt(info.ModTime().UnixNano(), 10),
	}
	system := reflect.ValueOf(info.Sys())
	if system.IsValid() && system.Kind() == reflect.Pointer && !system.IsNil() {
		system = system.Elem()
	}
	if system.IsValid() && system.Kind() == reflect.Struct {
		// Device and inode (or their Windows equivalents) distinguish a replacement
		// file without including atime, which may change as this hash is read.
		for _, fieldName := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow"} {
			field := system.FieldByName(fieldName)
			if !field.IsValid() || !field.CanInterface() {
				continue
			}
			switch field.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				parts = append(parts, fieldName+"="+strconv.FormatInt(field.Int(), 10))
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				parts = append(parts, fieldName+"="+strconv.FormatUint(field.Uint(), 10))
			case reflect.Struct:
				if fieldName == "Ctim" || fieldName == "Ctimespec" {
					parts = append(parts, fieldName+"="+fmt.Sprintf("%#v", field.Interface()))
				}
			}
		}
	}
	return strings.Join(parts, "\x00")
}

type identifiedHash struct {
	hash.Hash
	identity string
}

func (h *identifiedHash) MarshalBinary() ([]byte, error) {
	marshaler, ok := h.Hash.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("hash %T cannot be marshaled", h.Hash)
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(h.identity) > 1<<16 {
		return nil, fmt.Errorf("file identity is too large: %d bytes", len(h.identity))
	}
	payload := make([]byte, 4+len(h.identity)+len(state))
	binary.BigEndian.PutUint32(payload[:4], uint32(len(h.identity)))
	copy(payload[4:], h.identity)
	copy(payload[4+len(h.identity):], state)
	return payload, nil
}

func (h *identifiedHash) UnmarshalBinary(payload []byte) error {
	if len(payload) < 4 {
		return errors.New("identified hash state is truncated")
	}
	identitySize := int(binary.BigEndian.Uint32(payload[:4]))
	if identitySize > len(payload)-4 {
		return errors.New("identified hash identity is truncated")
	}
	if string(payload[4:4+identitySize]) != h.identity {
		return errors.New("file identity changed since checkpoint")
	}
	unmarshaler, ok := h.Hash.(encoding.BinaryUnmarshaler)
	if !ok {
		return fmt.Errorf("hash %T cannot be restored", h.Hash)
	}
	return unmarshaler.UnmarshalBinary(payload[4+identitySize:])
}
