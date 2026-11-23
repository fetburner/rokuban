package cryptstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/rfjakob/eme"
	"golang.org/x/crypto/scrypt"

	"github.com/fetburner/rokuban/internal/objstore"
)

const (
	markerKey         = ".rokuban-crypt"
	foreignPrefix     = "~crypt-foreign/"
	cipherContentType = "application/octet-stream"
)

// rangeCacheLimit は置き場ごとに保持するヘッダを最大 256 件にする。
// シークで同じ動画を繰り返し読む間は再利用でき、キーが最大長でもキー文字列は合計 256 KiB に収まる。
const rangeCacheLimit = 256

type rangeHeader struct {
	nonce [24]byte
	size  int64
}

// Store は平文のストレージ契約を提供する。署名 URL やローカル配信の能力は公開しない。
type Store struct {
	inner    objstore.DeclaredSHA256Store
	dataKey  [32]byte
	names    *eme.EMECipher
	tweak    [16]byte
	marker   [32]byte
	readOnly bool

	markerMu  sync.Mutex
	checked   bool
	markerErr error
	rangeMu   sync.Mutex
	headers   map[string]rangeHeader
}

var (
	_ objstore.Store       = (*Store)(nil)
	_ objstore.Reader      = (*Store)(nil)
	_ objstore.RangeReader = (*Store)(nil)
	_ objstore.Resumable   = (*Store)(nil)
)

// New は password と salt から鍵を作り、最初の操作で内側の鍵の目印を確認する。
// readOnly の置き場は目印を作らない。name は同じ bucket を別名で設定したときの混同を防ぐ。
func New(inner objstore.DeclaredSHA256Store, name, password, salt string, readOnly bool) (*Store, error) {
	if inner == nil {
		return nil, errors.New("crypt store requires a declared SHA-256 backend")
	}
	if password == "" || salt == "" {
		return nil, errors.New("crypt password and salt are required")
	}
	keys, err := scrypt.Key([]byte(password), []byte(salt), 16384, 8, 1, 80)
	if err != nil {
		return nil, fmt.Errorf("deriving crypt keys: %w", err)
	}
	defer clear(keys)

	// データ鍵、名前鍵、tweak の順序は文書に無く、実 rclone との往復テストで確かめる。
	block, err := aes.NewCipher(keys[32:64])
	if err != nil {
		return nil, fmt.Errorf("creating crypt name cipher: %w", err)
	}
	s := &Store{inner: inner, names: eme.New(block), readOnly: readOnly, headers: make(map[string]rangeHeader)}
	copy(s.dataKey[:], keys[:32])
	copy(s.tweak[:], keys[64:])
	proof := hmac.New(sha256.New, s.dataKey[:])
	_, _ = io.WriteString(proof, "rokuban crypt bucket marker v1\x00"+name)
	copy(s.marker[:], proof.Sum(nil))
	return s, nil
}

func (s *Store) checkMarker(ctx context.Context) error {
	s.markerMu.Lock()
	defer s.markerMu.Unlock()
	if s.checked {
		return s.markerErr
	}

	marker, err := s.inner.Head(ctx, markerKey)
	if errors.Is(err, objstore.ErrNotFound) {
		if s.readOnly {
			// Worker がまだ初期化していない場合、次の操作で確認し直せる。
			return errors.New("crypt store key marker has not been initialized")
		}
		objects, listErr := s.inner.List(ctx, "")
		if listErr != nil {
			return fmt.Errorf("checking that crypt store is empty: %w", listErr)
		}
		if len(objects) == 0 {
			// 同時初期化でも先に確定した目印が残るよう、内側が条件付きで作る。
			putErr := s.inner.Put(ctx, markerKey, strings.NewReader(""), 0, s.marker, cipherContentType)
			if putErr != nil && !errors.Is(putErr, objstore.ErrAlreadyExists) {
				return fmt.Errorf("creating crypt store key marker: %w", putErr)
			}
		}
		// List の間に別の worker が初期化した場合も、確定した目印を照合する。
		marker, err = s.inner.Head(ctx, markerKey)
		if len(objects) != 0 && errors.Is(err, objstore.ErrNotFound) {
			s.checked = true
			s.markerErr = errors.New("crypt store contains objects without a key marker")
			return s.markerErr
		}
	}
	if err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			return errors.New("crypt store key marker is missing after initialization")
		}
		return fmt.Errorf("reading crypt store key marker: %w", err)
	}

	actual, err := hex.DecodeString(marker.SHA256)
	if err != nil || marker.Size != 0 || !hmac.Equal(s.marker[:], actual) {
		s.markerErr = errors.New("crypt store key marker does not match")
	}
	s.checked = true
	return s.markerErr
}

// Put は平文を照合し、rclone crypt のオブジェクトが完全に公開されてから返る。
func (s *Store) Put(ctx context.Context, key string, src io.Reader, size int64, digest [32]byte, _ string) error {
	if err := s.checkMarker(ctx); err != nil {
		return err
	}
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	sealed, err := s.objectKey(key)
	if err != nil {
		return err
	}
	length, err := encryptedSize(size)
	if err != nil {
		return err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generating crypt nonce: %w", err)
	}
	r := &encryptedReader{ctx: ctx, src: src, key: &s.dataKey, initial: nonce, size: size, hash: sha256.New(), want: &digest}
	return s.inner.Put(ctx, sealed, r, length, digest, cipherContentType)
}

// Head は暗号文のサイズを平文のサイズへ換算する。
func (s *Store) Head(ctx context.Context, key string) (objstore.Object, error) {
	if err := s.checkMarker(ctx); err != nil {
		return objstore.Object{}, err
	}
	if encoded, ok := strings.CutPrefix(key, foreignPrefix); ok {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(raw) == 0 || string(raw) == markerKey {
			return objstore.Object{}, errKey
		}
		objects, err := s.inner.List(ctx, string(raw))
		if err != nil {
			return objstore.Object{}, err
		}
		for _, object := range objects {
			if object.Key == string(raw) {
				object.Key = key
				return object, nil
			}
		}
		return objstore.Object{}, objstore.ErrNotFound
	}
	sealed, err := s.objectKey(key)
	if err != nil {
		return objstore.Object{}, err
	}
	object, err := s.inner.Head(ctx, sealed)
	if err != nil {
		return objstore.Object{}, err
	}
	object.Size, err = plaintextSize(object.Size)
	object.Key = key
	return object, err
}

// Delete は指定した平文キーに対応するオブジェクトを削除する。
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.checkMarker(ctx); err != nil {
		return err
	}
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	sealed, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if err := s.inner.Delete(ctx, sealed); err != nil {
		return err
	}
	s.rangeMu.Lock()
	delete(s.headers, sealed)
	s.rangeMu.Unlock()
	return nil
}

// List は名前とサイズを復号して平文の prefix で絞る。目印は内部資産なので除く。
// 復号不能な名前やサイズは予約名前空間の報告用キーとして返す。孤児監査が
// 一覧全体を失わず、異物も見逃さないためである（TestMarkerAndForeignObjects）。
// 報告用キーは Head で存在を確認できる。読み取り・書き込み・削除の対象にはしない。
func (s *Store) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if err := s.checkMarker(ctx); err != nil {
		return nil, err
	}
	var sealedPrefix string
	// EME では成分の途中までの接頭辞は探索できない。確定した親ディレクトリだけを使う。
	if i := strings.LastIndexByte(prefix, '/'); i > 0 {
		dir := prefix[:i]
		if validKey(dir) && len(dir) <= maxKeySize && !strings.HasPrefix(prefix, foreignPrefix) {
			sealedPrefix = s.sealPath(dir, true) + "/"
		}
	}
	objects, err := s.inner.List(ctx, sealedPrefix)
	if err != nil {
		return nil, err
	}
	out := objects[:0]
	for _, object := range objects {
		if object.Key == markerKey {
			continue
		}
		name, ok := s.openPath(object.Key)
		size, sizeErr := plaintextSize(object.Size)
		if !ok || sizeErr != nil {
			object.Key = foreignPrefix + base64.RawURLEncoding.EncodeToString([]byte(object.Key))
		} else {
			object.Key, object.Size = name, size
		}
		if strings.HasPrefix(object.Key, prefix) {
			out = append(out, object)
		}
	}
	return out, nil
}

// Get は平文を先頭から読む。
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetFrom(ctx, key, 0)
}

// GetFrom は対象チャンクから認証と復号を行い、チャンク内の前置バイトを捨てる。
func (s *Store) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if err := s.checkMarker(ctx); err != nil {
		return nil, err
	}
	sealed, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("negative crypt read offset")
	}
	if offset == 0 {
		body, err := s.inner.Get(ctx, sealed)
		if err != nil {
			return nil, err
		}
		nonce, err := readHeader(body)
		if err != nil {
			return nil, errors.Join(err, body.Close())
		}
		return &decryptedReader{ReadCloser: body, key: &s.dataKey, initial: nonce}, nil
	}

	s.rangeMu.Lock()
	header, cached := s.headers[sealed]
	s.rangeMu.Unlock()
	if !cached {
		var prefix [headerSize]byte
		length, err := s.inner.ReadPrefix(ctx, sealed, prefix[:])
		if err != nil {
			return nil, err
		}
		header.size, err = plaintextSize(length)
		if err != nil {
			return nil, err
		}
		header.nonce, err = readHeader(bytes.NewReader(prefix[:]))
		if err != nil {
			return nil, err
		}
		s.rangeMu.Lock()
		if len(s.headers) == rangeCacheLimit {
			for old := range s.headers {
				delete(s.headers, old)
				break
			}
		}
		s.headers[sealed] = header
		s.rangeMu.Unlock()
	}
	if cached && offset >= header.size {
		// この分岐は本文を開かないため、他のプロセスが削除したキーは Head で検出する。
		_, err := s.inner.Head(ctx, sealed)
		if err != nil {
			return nil, err
		}
	}
	if offset > header.size {
		return nil, errors.New("crypt read offset exceeds object size")
	}
	if offset == header.size {
		return io.NopCloser(strings.NewReader("")), nil
	}
	chunk := offset / chunkSize
	body, err := s.inner.GetFrom(ctx, sealed, headerSize+chunk*sealedChunkSize)
	if err != nil {
		return nil, err
	}
	return &decryptedReader{ReadCloser: body, key: &s.dataKey, initial: header.nonce, chunk: chunk, skip: int(offset % chunkSize)}, nil
}
