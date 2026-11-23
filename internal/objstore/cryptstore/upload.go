package cryptstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/nacl/secretbox"

	"github.com/fetburner/rokuban/internal/objstore"
)

// encryptedReader は平文のチャンクを読み、内側に渡す暗号文を作る。
// final の照合は、内側が宣言サイズを読んだ後の EOF 確認で必ず行う。
type encryptedReader struct {
	ctx        context.Context
	src        io.Reader
	key        *[32]byte
	initial    [24]byte
	hash       hash.Hash
	size, pos  int64
	headerSent bool
	skip       int
	want       *[32]byte
	checkpoint func() error
	pending    []byte
	plain      [chunkSize]byte
	sealed     [sealedChunkSize]byte
	final      bool
	err        error
}

func (r *encryptedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if len(r.pending) == 0 {
		r.err = r.fill()
		if r.err != nil {
			return 0, r.err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *encryptedReader) fill() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if !r.headerSent {
		copy(r.sealed[:], magic)
		copy(r.sealed[len(magic):], r.initial[:])
		r.pending = r.sealed[r.skip:headerSize]
		r.headerSent, r.skip = true, 0
		return nil
	}
	if r.pos < r.size {
		plain := r.plain[:min(r.size-r.pos, chunkSize)]
		if _, err := io.ReadFull(r.src, plain); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("crypt source shorter than declared: %w", objstore.ErrChecksumMismatch)
			}
			return fmt.Errorf("reading crypt source: %w", err)
		}
		_, _ = r.hash.Write(plain)
		nonce := chunkNonce(r.initial, r.pos/chunkSize)
		r.pos += int64(len(plain))
		if r.checkpoint != nil {
			if err := r.checkpoint(); err != nil {
				return err
			}
		}
		sealed := secretbox.Seal(r.sealed[:0], plain, &nonce, r.key)
		r.pending, r.skip = sealed[r.skip:], 0
		return nil
	}
	if err := sourceEOF(r.src); err != nil {
		return err
	}
	if r.want != nil && [32]byte(r.hash.Sum(nil)) != *r.want {
		return objstore.ErrChecksumMismatch
	}
	r.final = true
	return io.EOF
}

func sourceEOF(src io.Reader) error {
	var extra [1]byte
	n, err := io.ReadFull(src, extra[:])
	if n != 0 {
		return fmt.Errorf("crypt source longer than declared: %w", objstore.ErrChecksumMismatch)
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("checking end of crypt source: %w", err)
	}
	return nil
}

type uploadState struct {
	Version      int      `json:"version"`
	Inner        []byte   `json:"inner"`
	EmptyKey     string   `json:"empty_key,omitempty"`
	Size         int64    `json:"size"`
	Want         [32]byte `json:"want"`
	Nonce        [24]byte `json:"nonce"`
	CipherOffset int64    `json:"cipher_offset"`
	// Hash は内側に残った最後の完全なチャンクまでの状態。
	Hash []byte `json:"hash,omitempty"`
	// Consumed と Digest は、以前の試行で暗号化した全平文の範囲と SHA-256。
	// 内側が保持しなかった暗号文に対応する平文も含む。
	Consumed int64    `json:"consumed"`
	Digest   [32]byte `json:"digest"`
	Final    bool     `json:"final"`
}

// StartUpload は新しい nonce と内側のアップロード状態を保存する。
func (s *Store) StartUpload(ctx context.Context, key string, size int64, digest [32]byte, _ string) ([]byte, error) {
	if err := s.checkMarker(ctx); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, objstore.ErrReadOnly
	}
	sealed, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	length, err := encryptedSize(size)
	if err != nil {
		return nil, err
	}
	st := uploadState{Version: 1, Size: size, Want: digest, Digest: sha256.Sum256(nil)}
	if size == 0 {
		st.EmptyKey = sealed
	}
	if _, err := rand.Read(st.Nonce[:]); err != nil {
		return nil, fmt.Errorf("generating crypt upload nonce: %w", err)
	}
	st.Hash, err = sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("saving initial crypt upload hash: %w", err)
	}
	st.Inner, err = s.inner.StartUpload(ctx, sealed, length, digest, cipherContentType)
	if err != nil {
		return nil, err
	}
	return encodeState(st)
}

func (st uploadState) plainOffset(length int64) int64 {
	if st.CipherOffset == length {
		return st.Size
	}
	return max(0, st.CipherOffset-headerSize) / sealedChunkSize * chunkSize
}

func decodeState(raw []byte) (uploadState, int64, error) {
	var st uploadState
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, 0, fmt.Errorf("decoding crypt upload state: %w: %w", objstore.ErrUploadInvalid, err)
	}
	length, err := encryptedSize(st.Size)
	if err != nil || st.Version != 1 || len(st.Inner) == 0 || (st.Size == 0 && st.EmptyKey == "") || st.CipherOffset < 0 || st.CipherOffset > length ||
		st.Consumed < st.plainOffset(length) || st.Consumed > st.Size ||
		(st.Consumed != st.Size && st.Consumed%chunkSize != 0) || (st.Final && st.CipherOffset != length) {
		return st, 0, fmt.Errorf("inconsistent crypt upload state: %w", objstore.ErrUploadInvalid)
	}
	return st, length, nil
}

func encodeState(st uploadState) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("encoding crypt upload state: %w", err)
	}
	return raw, nil
}

func resumeHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return nil, fmt.Errorf("restoring crypt upload hash: %w: %w", objstore.ErrUploadInvalid, err)
	}
	return h, nil
}

// ContinueUpload は内側の offset を平文チャンクの先頭へ切り下げる。
// 既に暗号化した範囲を先に照合し、照合済みの同じバイトをバッファから再送する。
// 保存せず中断した試行は新しい StartUpload から始める（転送の lease 契約）。
func (s *Store) ContinueUpload(ctx context.Context, raw []byte, src io.Reader) ([]byte, int64, error) {
	st, length, err := decodeState(raw)
	if err != nil {
		return raw, 0, err
	}
	offset := st.plainOffset(length)
	if err := s.checkMarker(ctx); err != nil {
		return raw, offset, err
	}
	if s.readOnly {
		return raw, offset, objstore.ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return raw, offset, err
	}
	if st.CipherOffset == length {
		err := sourceEOF(src)
		st.Final = err == nil
		out, encErr := encodeState(st)
		return out, offset, errors.Join(err, encErr)
	}

	h, err := resumeHash(st.Hash)
	if err != nil {
		return raw, offset, err
	}
	if st.Consumed > offset {
		verify, err := resumeHash(st.Hash)
		if err != nil {
			return raw, offset, err
		}
		var replay bytes.Buffer
		if _, err := io.CopyN(io.MultiWriter(&replay, verify), src, st.Consumed-offset); err != nil {
			if ctx.Err() != nil {
				return raw, offset, ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return raw, offset, fmt.Errorf("crypt resume source is shorter than the encrypted prefix: %w", objstore.ErrUploadInvalid)
			}
			return raw, offset, fmt.Errorf("reading crypt resume source: %w", err)
		}
		if [32]byte(verify.Sum(nil)) != st.Digest {
			return raw, offset, fmt.Errorf("crypt resume source changed: %w", objstore.ErrUploadInvalid)
		}
		src = io.MultiReader(&replay, src)
	}

	r := &encryptedReader{ctx: ctx, src: src, key: &s.dataKey, initial: st.Nonce, size: st.Size, pos: offset, hash: h}
	if st.CipherOffset < headerSize {
		r.skip = int(st.CipherOffset)
	} else {
		r.headerSent = true
		r.skip = int((st.CipherOffset - headerSize) % sealedChunkSize)
	}
	// 内側がどのパートまで残したかは戻るまで分からない。チャンクごとに
	// SHA-256 の状態だけ保持し、保存するのは戻った境界の状態だけにする。
	checkpointSize := len(st.Hash)
	checkpoints := append([]byte(nil), st.Hash...)
	r.checkpoint = func() error {
		var err error
		checkpoints, err = h.(encoding.BinaryAppender).AppendBinary(checkpoints)
		if err != nil {
			return fmt.Errorf("checkpointing crypt upload hash: %w", err)
		}
		return nil
	}

	inner, cipherOffset, sendErr := s.inner.ContinueUpload(ctx, st.Inner, r)
	if cipherOffset < st.CipherOffset || cipherOffset > length {
		return raw, offset, fmt.Errorf("inner crypt upload offset moved outside the expected range: %w", objstore.ErrUploadInvalid)
	}
	st.Inner, st.CipherOffset = inner, cipherOffset
	newOffset := st.plainOffset(length)
	if newOffset > r.pos {
		return raw, offset, fmt.Errorf("inner crypt upload advanced beyond the encrypted prefix: %w", objstore.ErrUploadInvalid)
	}
	if r.pos >= st.Consumed {
		st.Consumed = r.pos
		st.Digest = [32]byte(h.Sum(nil))
	}
	st.Final = sendErr == nil && r.final && cipherOffset == length
	if cipherOffset == length {
		st.Hash = nil
	} else {
		index := (newOffset - offset) / chunkSize * int64(checkpointSize)
		st.Hash = checkpoints[index : index+int64(checkpointSize)]
	}
	out, err := encodeState(st)
	if err != nil {
		return raw, offset, errors.Join(sendErr, err)
	}
	if sendErr == nil && !st.Final {
		sendErr = fmt.Errorf("inner crypt upload did not finish reading: %w", objstore.ErrUploadInvalid)
	}
	return out, newOffset, sendErr
}

// CompleteUpload は平文の長さと SHA-256 を照合した送信だけを公開する。
func (s *Store) CompleteUpload(ctx context.Context, raw []byte) error {
	if err := s.checkMarker(ctx); err != nil {
		return err
	}
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	st, _, err := decodeState(raw)
	if err != nil {
		return err
	}
	// 0 バイトの送信は Continue を挟まず完了できる。内側には形式上の
	// ヘッダが要るので、平文の宣言を照合してからここで送る。
	if st.Size == 0 && st.CipherOffset == 0 && !st.Final {
		if st.Want != sha256.Sum256(nil) {
			return objstore.ErrChecksumMismatch
		}
		existing, err := s.inner.Head(ctx, st.EmptyKey)
		switch {
		case err == nil:
			if existing.Size == headerSize && existing.SHA256 == fmt.Sprintf("%x", st.Want) {
				return nil
			}
			return objstore.ErrAlreadyExists
		case !errors.Is(err, objstore.ErrNotFound):
			return err
		}
		ready, _, err := s.ContinueUpload(ctx, raw, bytes.NewReader(nil))
		if err != nil {
			return err
		}
		st, _, err = decodeState(ready)
		if err != nil {
			return err
		}
	}
	if !st.Final {
		return fmt.Errorf("crypt upload source has not reached a verified EOF: %w", objstore.ErrUploadInvalid)
	}
	if st.Digest != st.Want {
		return objstore.ErrChecksumMismatch
	}
	return s.inner.CompleteUpload(ctx, st.Inner)
}

// AbortUpload は内側の未完了アップロードを破棄する。
func (s *Store) AbortUpload(ctx context.Context, raw []byte) (bool, error) {
	if err := s.checkMarker(ctx); err != nil {
		return false, err
	}
	if s.readOnly {
		return false, objstore.ErrReadOnly
	}
	st, _, err := decodeState(raw)
	if err != nil {
		return false, err
	}
	return s.inner.AbortUpload(ctx, st.Inner)
}
