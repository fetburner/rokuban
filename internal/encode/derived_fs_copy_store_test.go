package encode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// putCommitPublisher は Prepare と PublishEncoded を成功させるだけの派生物の公開契約。
type putCommitPublisher struct {
	store.DerivedMediaPublisher
}

func (*putCommitPublisher) PreparePendingMediaAssetCopy(context.Context, store.PendingMediaAssetCopy) error {
	return nil
}

func (*putCommitPublisher) PublishEncoded(_ context.Context, _ store.EncodedPublishInput, before func() error) (bool, error) {
	return true, before()
}

// beforeEncodeObjectPut は本体の Put の前に 1 回だけ呼ぶ。字幕の Put と commit の前では呼ばない。
func TestPublishEncodedCallsBeforeEncodeObjectPutOnlyBeforeTheBodyPut(t *testing.T) {
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "tokyo"), "mediapath.WriteStoreMarker")
	scratch := filepath.Join(t.TempDir(), "out.mp4")
	subtitle := filepath.Join(t.TempDir(), "out.vtt")
	for path, body := range map[string]string{scratch: "data", subtitle: "WEBVTT\n\n"} {
		testutil.RequireNoError(t, os.WriteFile(path, []byte(body), 0o600), "os.WriteFile")
	}
	w := &EncodeWorker{
		Objects: testFSObjectStore(mediaDir), ObjectPublisher: &putCommitPublisher{},
		MediaDir: mediaDir, MediaStoreName: "tokyo",
	}
	calls := 0
	previous := beforeEncodeObjectPut
	beforeEncodeObjectPut = func(string) { calls++ }
	t.Cleanup(func() { beforeEncodeObjectPut = previous })

	_, published, err := w.publishEncoded(context.Background(), encodePublishInput{
		relPath: "sites/tokyo/out.mp4", scratchOut: scratch, subtitleOut: subtitle, withSubtitles: true,
	})
	if err != nil || !published {
		t.Fatalf("publishEncoded() = published:%t err:%v, want published", published, err)
	}
	if calls != 1 {
		t.Errorf("beforeEncodeObjectPut calls = %d, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "sites", "tokyo", "out.vtt")); err != nil {
		t.Errorf("subtitle companion was not published: %v", err)
	}
}
