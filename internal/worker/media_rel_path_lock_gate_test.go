package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMediaRelPathFileLock_GateBlocksUnlinkBetweenOpenAndFlock は取得側 B が旧 inode を
// open 済み・flock 前の窓にいるとき、owner A の Close が unlink しても B と C が同時に
// critical section に入らないことを確かめる。gate protocol が壊れると B は unlink 済みの
// 旧 inode を、C は新 inode を lock できてしまう。
func TestMediaRelPathFileLock_GateBlocksUnlinkBetweenOpenAndFlock(t *testing.T) {
	mediaDir := t.TempDir()
	const relPath = "race/window.m2ts"
	a, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatalf("locking owner A: %v", err)
	}

	parked := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	var armed atomic.Bool
	orig := afterOpenMediaRelPathLock
	afterOpenMediaRelPathLock = func(string) {
		if armed.CompareAndSwap(true, false) {
			close(parked)
			<-resume
		}
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		release()
		wg.Wait()
		afterOpenMediaRelPathLock = orig
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	armed.Store(true)
	bCh := make(chan *mediaRelPathFileLock, 1)
	bErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		b, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
		if err != nil {
			bErr <- err
			return
		}
		bCh <- b
	}()
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("B did not reach the open-to-flock window")
	}

	closeDone := make(chan error, 1)
	go func() {
		defer wg.Done()
		closeDone <- a.Close()
	}()
	// gate が効いていれば A の Close は B が gate を離すまで待つ。効いていなければここで
	// unlink まで進む。どちらでも B を進めて結果だけを判定する。
	time.Sleep(100 * time.Millisecond)
	release()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("closing owner A: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner A Close did not finish")
	}
	var b *mediaRelPathFileLock
	select {
	case b = <-bCh:
	case err := <-bErr:
		t.Fatalf("B acquiring: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("B did not acquire after A closed")
	}
	c, acquired, err := tryLockMediaRelPathFile(mediaDir, relPath)
	if err != nil {
		_ = b.Close()
		t.Fatalf("C trying lock: %v", err)
	}
	if acquired {
		_ = c.Close()
		_ = b.Close()
		t.Fatal("B and C hold the same rel_path lock at the same time")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("closing B: %v", err)
	}
}
