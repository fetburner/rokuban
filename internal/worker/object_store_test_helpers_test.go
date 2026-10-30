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

// testStorageObjects は FS の置き場 name を dir に置いた StorageObjects を返す。
func testStorageObjects(t *testing.T, name, dir string) map[string]objstore.Store {
	t.Helper()
	return map[string]objstore.Store{name: testFilesystemObjectStore(t, dir)}
}
