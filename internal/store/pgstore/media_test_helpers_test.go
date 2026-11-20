package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
)

func testMedia(pool *pgxpool.Pool, storeName, derivedStore string) *Media {
	policy := store.NewStoragePolicy(nil, nil, derivedStore)
	return NewMediaWithPolicy(pool, storeName, policy)
}

func testMediaWithStores(pool *pgxpool.Pool, storeName, originalStore, derivedStore string) *Media {
	policy := store.NewStoragePolicy(nil, nil, derivedStore)
	return NewMediaWithStoresAndPolicy(pool, storeName, originalStore, policy)
}
