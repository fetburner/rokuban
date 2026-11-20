package store_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

type allDerivedDeleteContracts struct {
	store.MediaDelete
	store.PendingCopyReclaim
	store.StorageObservation
}

func TestNewDerivedObjectDeleteStoresRejectsPartialContracts(t *testing.T) {
	all := &allDerivedDeleteContracts{}
	for _, tc := range []struct {
		name        string
		mediaDelete store.MediaDelete
		pending     store.PendingCopyReclaim
		observation store.StorageObservation
	}{
		{name: "missing media delete", pending: all, observation: all},
		{name: "missing pending reclaim", mediaDelete: all, observation: all},
		{name: "missing storage observation", mediaDelete: all, pending: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores, err := store.NewDerivedObjectDeleteStores(tc.mediaDelete, tc.pending, tc.observation)
			if err == nil {
				t.Fatal("NewDerivedObjectDeleteStores succeeded with a missing contract")
			}
			if stores != nil {
				t.Fatalf("NewDerivedObjectDeleteStores returned %#v with an error", stores)
			}
		})
	}
}

func TestNewDerivedObjectDeleteStoresRetainsEachContract(t *testing.T) {
	all := &allDerivedDeleteContracts{}
	stores, err := store.NewDerivedObjectDeleteStores(all, all, all)
	if err != nil {
		t.Fatal(err)
	}
	if stores.MediaDelete() != all || stores.PendingCopyReclaim() != all || stores.StorageObservation() != all {
		t.Fatal("NewDerivedObjectDeleteStores did not retain each role contract")
	}
}
