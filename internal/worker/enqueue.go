package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/store"
)

func enqueueJob(ctx context.Context, enqueuer jobq.Enqueuer, args jobq.Args) error {
	if enqueuer == nil {
		return errors.New("worker: job enqueuer is nil")
	}
	_, err := enqueuer.Enqueue(ctx, args)
	return err
}

// errNoActiveOriginal は、読み取りジョブを積む置き場を決められない（active な原本のコピーが無い）ことを表す。
// 次の reconcile が引き直す。
var errNoActiveOriginal = errors.New("no active original copy to choose a store from")

// originalStoresOf は、読み取りジョブを積む録画の置き場を 1 回の問い合わせでまとめて引く。
func originalStoresOf(ctx context.Context, locator store.OriginalLocator, recordingIDs []int64) (map[int64]string, error) {
	if locator == nil {
		return nil, errors.New("worker: original locator is nil")
	}
	stores, err := locator.OriginalStores(ctx, recordingIDs)
	if err != nil {
		return nil, fmt.Errorf("locating originals: %w", err)
	}
	return stores, nil
}
