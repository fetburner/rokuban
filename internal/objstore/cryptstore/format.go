// Package cryptstore は派生物の名前と内容を rclone crypt 形式で暗号化する。
package cryptstore

import (
	"bytes"
	"crypto/aes"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/nacl/secretbox"
)

const (
	magic           = "RCLONE\x00\x00"
	headerSize      = 32
	chunkSize       = 64 << 10
	sealedChunkSize = chunkSize + secretbox.Overhead
	maxKeySize      = 1024
)

var (
	// base32hex のアルファベットは文書に無く、実 rclone との往復テストで確かめる。
	nameEncoding  = base32.HexEncoding.WithPadding(base32.NoPadding)
	versionSuffix = regexp.MustCompile(`-v[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}-[0-9]{3}$`)
	errKey        = errors.New("invalid crypt object key")
	errFormat     = errors.New("invalid rclone crypt object")
)

// objectKey は名前の各成分を暗号化する。S3 の 1,024 バイトを超えるキーは
// 送信前に拒む。短縮すると一意性や rclone による復号を失う（TestContentPathKeyLimit）。
func (s *Store) objectKey(key string) (string, error) {
	if len(key) > maxKeySize || !validKey(key) {
		return "", errKey
	}
	sealed := s.sealPath(key, false)
	if len(sealed) > maxKeySize {
		return "", fmt.Errorf("encrypted object key exceeds %d bytes: %w", maxKeySize, errKey)
	}
	return sealed, nil
}

func validKey(key string) bool {
	if strings.HasPrefix(key, foreignPrefix) || !utf8.ValidString(key) || strings.ContainsAny(key, "\x00\\") {
		return false
	}
	for part := range strings.SplitSeq(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (s *Store) sealPath(key string, directory bool) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		var version string
		if !directory && i == len(parts)-1 {
			part, version = splitVersion(part)
		}
		if part == "" {
			parts[i] = version
			continue
		}
		pad := aes.BlockSize - len(part)%aes.BlockSize
		padded := append([]byte(part), bytes.Repeat([]byte{byte(pad)}, pad)...)
		parts[i] = strings.ToLower(nameEncoding.EncodeToString(s.names.Encrypt(s.tweak[:], padded))) + version
	}
	return strings.Join(parts, "/")
}

func (s *Store) openPath(key string) (string, bool) {
	if len(key) > maxKeySize {
		return "", false
	}
	parts := strings.Split(key, "/")
	for i, part := range parts {
		var version string
		if i == len(parts)-1 {
			part, version = splitVersion(part)
		}
		if part == "" && version != "" {
			parts[i] = version
			continue
		}
		data, err := nameEncoding.DecodeString(strings.ToUpper(part))
		if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
			return "", false
		}
		padded := s.names.Decrypt(s.tweak[:], data)
		pad := int(padded[len(padded)-1])
		if pad == 0 || pad > aes.BlockSize || !bytes.Equal(padded[len(padded)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
			return "", false
		}
		plain := string(padded[:len(padded)-pad])
		if strings.Contains(plain, "/") {
			return "", false
		}
		ext := fileExtension(plain)
		parts[i] = strings.TrimSuffix(plain, ext) + version + ext
	}
	plain := strings.Join(parts, "/")
	// 大文字や非正規 base32 など、再暗号化して同じキーにならないものは孤児として扱う。
	return plain, validKey(plain) && s.sealPath(plain, false) == key
}

// splitVersion の拡張子の位置と日時の検査は crypt の形式文書に無く、
// TestS3CryptRcloneNames の拡張子付きファイルで実 rclone の backend encode / decode と照合する。
func splitVersion(name string) (unversioned, version string) {
	ext := fileExtension(name)
	stem := strings.TrimSuffix(name, ext)
	suffix := versionSuffix.FindString(stem)
	if suffix == "" {
		return name, ""
	}
	millis := len(suffix) - 4
	_, err := time.Parse("-v2006-01-02-150405.000", suffix[:millis]+"."+suffix[millis+1:])
	if err != nil {
		return name, ""
	}
	return strings.TrimSuffix(stem, suffix) + ext, suffix
}

func fileExtension(name string) string {
	ext := path.Ext(name)
	if ext == name {
		return ""
	}
	return ext
}

func encryptedSize(size int64) (int64, error) {
	if size < 0 {
		return 0, errors.New("negative crypt object size")
	}
	chunks := size / chunkSize
	if size%chunkSize != 0 {
		chunks++
	}
	overhead := headerSize + chunks*secretbox.Overhead
	if size > math.MaxInt64-overhead {
		return 0, errors.New("crypt object size overflows")
	}
	return size + overhead, nil
}

func plaintextSize(size int64) (int64, error) {
	if size < headerSize {
		return 0, errFormat
	}
	chunks, tail := (size-headerSize)/sealedChunkSize, (size-headerSize)%sealedChunkSize
	if tail != 0 && tail <= secretbox.Overhead {
		return 0, errFormat
	}
	if tail != 0 {
		tail -= secretbox.Overhead
	}
	return chunks*chunkSize + tail, nil
}

// chunkNonce のリトルエンディアン加算は文書に無く、実 rclone との往復テストで確かめる。
func chunkNonce(initial [24]byte, chunk int64) [24]byte {
	carry := uint64(chunk)
	for i := 0; i < len(initial); i += 8 {
		word := binary.LittleEndian.Uint64(initial[i:])
		sum := word + carry
		binary.LittleEndian.PutUint64(initial[i:], sum)
		if sum >= word {
			break
		}
		carry = 1
	}
	return initial
}

func readHeader(src io.Reader) ([24]byte, error) {
	var nonce [24]byte
	var header [headerSize]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return nonce, fmt.Errorf("reading crypt header: %w: %w", errFormat, err)
	}
	if string(header[:len(magic)]) != magic {
		return nonce, errFormat
	}
	copy(nonce[:], header[len(magic):])
	return nonce, nil
}

type decryptedReader struct {
	io.ReadCloser
	key     *[32]byte
	initial [24]byte
	chunk   int64
	pending []byte
	skip    int
	sealed  [sealedChunkSize]byte
	plain   [chunkSize]byte
	err     error
}

func (r *decryptedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if len(r.pending) == 0 {
		n, err := io.ReadFull(r.ReadCloser, r.sealed[:])
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			r.err = err
			return 0, err
		}
		nonce := chunkNonce(r.initial, r.chunk)
		plain, ok := secretbox.Open(r.plain[:0], r.sealed[:n], &nonce, r.key)
		if !ok || len(plain) <= r.skip {
			r.err = errFormat
			return 0, r.err
		}
		r.pending = plain[r.skip:]
		r.skip = 0
		r.chunk++
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
