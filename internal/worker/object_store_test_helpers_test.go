package worker

import (
	"testing"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
)

func testFSObjectStore(root string) objstore.Store {
	objects, err := fsstore.New(root)
	if err != nil {
		panic(err)
	}
	return objects
}
