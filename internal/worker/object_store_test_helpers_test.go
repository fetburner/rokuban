package worker

import (
	"testing"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// testStorageObjects は FS の置き場 name を dir に置いた StorageObjects を返す。
func testStorageObjects(t *testing.T, name, dir string) map[string]objstore.Store {
	t.Helper()
	return map[string]objstore.Store{name: testFilesystemObjectStore(t, dir)}
}

func testFilesystemObjectStore(t *testing.T, root string) objstore.Store {
	t.Helper()
	objects, err := fsstore.New(root)
	testutil.RequireNoError(t, err, "creating filesystem object store")
	return objects
}
