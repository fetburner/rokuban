package store

import "errors"

// DerivedObjectDeleteStores は、派生物の置き場を掃く独立した削除パスが使う 3 つの store 契約をまとめる。
// ポインタが nil なら、その削除パスは無い。フィールドを非公開にし、値を NewDerivedObjectDeleteStores でだけ
// 作るのは、3 つのうち一部だけが nil の状態を表現できなくするためである。ゼロ値の構造体へのポインタは使わない。
// 3 つの契約は MySQL 構成との境界なので、同じ値を渡すときも 1 つのインターフェースにまとめず役割ごとに持つ。
type DerivedObjectDeleteStores struct {
	mediaDelete    MediaDelete
	pendingReclaim PendingCopyReclaim
	storage        StorageObservation
}

// NewDerivedObjectDeleteStores は、独立した削除パスに必要な 3 つの契約をまとめる。
// どれか 1 つでも nil なら値を作らず、エラーを返す。
func NewDerivedObjectDeleteStores(mediaDelete MediaDelete, pendingReclaim PendingCopyReclaim, storage StorageObservation) (*DerivedObjectDeleteStores, error) {
	if mediaDelete == nil || pendingReclaim == nil || storage == nil {
		return nil, errors.New("derived object delete stores require all three store contracts")
	}
	return &DerivedObjectDeleteStores{
		mediaDelete:    mediaDelete,
		pendingReclaim: pendingReclaim,
		storage:        storage,
	}, nil
}

// MediaDelete は派生物の物理コピー削除を扱う契約を返す。
func (s *DerivedObjectDeleteStores) MediaDelete() MediaDelete { return s.mediaDelete }

// PendingCopyReclaim は未公開の pending コピー回収を扱う契約を返す。
func (s *DerivedObjectDeleteStores) PendingCopyReclaim() PendingCopyReclaim { return s.pendingReclaim }

// StorageObservation は派生物置き場の孤児観測を扱う契約を返す。
func (s *DerivedObjectDeleteStores) StorageObservation() StorageObservation { return s.storage }
