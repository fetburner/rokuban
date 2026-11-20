package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
)

// testMedia は storeName の置き場に束縛した Media を作る。storeName が空なら置き場に束縛しない。
func testMedia(pool *pgxpool.Pool, storeName, derivedStore string) *Media {
	policy := store.NewStoragePolicy(nil, nil, derivedStore)
	if storeName == "" {
		return NewMediaUnboundWithPolicy(pool, policy)
	}
	return NewMediaInStore(pool, storeName, policy)
}

// testMediaWithStores は原本の置き場が storeName と違うときだけ派生物の置き場を分けて作る。
func testMediaWithStores(pool *pgxpool.Pool, storeName, originalStore, derivedStore string) *Media {
	if storeName == originalStore {
		return testMedia(pool, storeName, derivedStore)
	}
	policy := store.NewStoragePolicy(nil, nil, derivedStore)
	return NewMediaForDerivedStore(pool, storeName, originalStore, policy)
}
