package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/worker"
)

// storeFlagName は `server` サブコマンドのプロセスが置き場をマウントするフラグ名。
// --sites と同じく config キーにしない起動形態の軸である（resolveSiteBinding のコメント参照）。
const storeFlagName = "stores"

// resolveStoreBinding は `--stores` フラグと storage.stores から、このプロセスが
// マウントする FS 置き場の名前を返す。
//
// 解決規則は resolveSiteBinding と同じ形:
//   - フラグ未指定 → FS 置き場がちょうど 1 つならその 1 つ、0 なら nil、2 つ以上なら
//     起動エラー（暗黙に「全部」にしない）
//   - `--stores=`（明示的な空）→ 置き場を持たない（nil）
//   - `--stores a,b` → 指定された名前がすべて FS 置き場として定義されていること。
//     S3 置き場はマウントできないので、名前が存在しても拒む。同じ名前の重複は 1 つに畳む。
func resolveStoreBinding(cmd *cobra.Command, storage config.StorageConfig) ([]string, error) {
	names, err := cmd.Flags().GetStringSlice(storeFlagName)
	if err != nil {
		return nil, err
	}
	fsNames := storage.FSStoreNames()

	if !cmd.Flags().Changed(storeFlagName) {
		switch len(fsNames) {
		case 0:
			return nil, nil
		case 1:
			return fsNames, nil
		default:
			return nil, fmt.Errorf("--stores is required: storage.stores has %d fs stores and mounting all of them implicitly is not supported",
				len(fsNames))
		}
	}

	var mounted []string
	var unknown, notFS []string
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		store, ok := storage.Stores[n]
		switch {
		case !ok:
			unknown = append(unknown, n)
		case store.Kind != "fs":
			notFS = append(notFS, n)
		default:
			mounted = append(mounted, n)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--%s: unknown store(s) %s (storage.stores has: %s)",
			storeFlagName, strings.Join(unknown, ", "), strings.Join(storeNames(storage), ", "))
	}
	if len(notFS) > 0 {
		return nil, fmt.Errorf("--%s: %s is not an fs store; only fs stores can be mounted",
			storeFlagName, strings.Join(notFS, ", "))
	}
	return mounted, nil
}

func storeNames(storage config.StorageConfig) []string {
	names := make([]string, 0, len(storage.Stores))
	for name := range storage.Stores {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// validateStoreBinding は、解決済みのマウント集合とロール・キュー・--once の組み合わせを検査する。
//
// explicit は --stores が指定されたか（Changed）。指定しなければ既定の解決（FS 置き場が 1 つだけなら
// それ）が入るので、効かない指定の検査だけ explicit で行う。
//
// 検査は次の項目で、どれも起動後に静かに待つか、置き場を取り違えて書く形を先に止める:
//   - worker と streamer のどちらも無いプロセスでの指定は起動エラー（効かないフラグを黙って無視しない）
//   - worker が置き場で修飾するキューを引くのに、マウントが 0 件なら起動エラー（`--stores=` を含む）
//   - worker が ingest を引くのに、FS 置き場を 1 つもマウントしていなければ起動エラー
//   - worker が encode / thumbnail（seek_tiles を含む）を引くのに、storage.derived が FS 置き場で
//     マウントしていなければ起動エラー。派生物はその置き場へ書くので、S3 にするか、マウントするかを選ばせる
//   - worker が --once で、置き場で修飾した物理キューが 2 本以上になるなら起動エラー
//     （1 件消化は 1 キューを前提にする）
//
// streamer と録画再生のロールは原本の配信のために --stores を取るが、マウントが 0 件でも起動を拒まない
// （site に束縛したライブ専用の streamer が置き場を持たなくてよい）。
func validateStoreBinding(roles []string, explicit bool, storage config.StorageConfig, mounted []string, queues []string, once bool) error {
	isWorker := slices.Contains(roles, "worker")
	isStreamer := slices.Contains(roles, "streamer")
	if explicit && !isWorker && !isStreamer {
		return fmt.Errorf("--%s has no effect without the worker or streamer role (got roles [%s])",
			storeFlagName, strings.Join(roles, ", "))
	}
	if !isWorker {
		return nil
	}
	if jobs.StoreBoundQueueCount(queues) > 0 && len(mounted) == 0 {
		return fmt.Errorf("--%s: the worker pulls store-bound queues but no fs store is mounted; "+
			"pass at least one fs store name to --%s, or restrict worker.queues to queues that do not touch a store",
			storeFlagName, storeFlagName)
	}
	if (len(queues) == 0 || slices.Contains(queues, jobs.IngestQueue)) && len(mounted) == 0 {
		return fmt.Errorf("--%s: the worker pulls the ingest queue but no fs store is mounted; "+
			"ingest writes to an fs store, so pass at least one fs store name to --%s",
			storeFlagName, storeFlagName)
	}
	// encode と thumbnail（seek_tiles は thumbnail に載る）が派生物を書く。その集合は jobs が持つ。
	// 派生物の FS 置き場をマウントしていないと、その仕事は実行時に置き場が無いと失敗するので、起動で止める。
	if derived, ok := storage.Stores[storage.Derived]; ok && derived.Kind == "fs" &&
		jobs.RequiresEncodeTools(queues) && !slices.Contains(mounted, storage.Derived) {
		return fmt.Errorf("--%s: the worker pulls encode or thumbnail (which write derived media) but storage.derived %q is an fs store this process does not mount; "+
			"use an s3 store for storage.derived, or mount %q with --%s",
			storeFlagName, storage.Derived, storage.Derived, storeFlagName)
	}
	if once {
		if n := jobs.StoreBoundQueueCount(queues) * len(mounted); n >= 2 {
			return fmt.Errorf("--%s: --once pulls %d store-qualified physical queues (%d mounted stores); "+
				"one-job mode needs exactly one queue, so mount a single fs store",
				storeFlagName, n, len(mounted))
		}
	}
	return nil
}

// commandStoreFlagName は、置き場を 1 つ使う単発コマンド（rescue / catalog verify / import）の `--store`。
const commandStoreFlagName = "store"

// addCommandStoreFlag は単発コマンドに `--store` を足す。
func addCommandStoreFlag(cmd *cobra.Command) {
	cmd.Flags().String(commandStoreFlagName, "", "対象の FS 置き場の名前（FS 置き場が 1 つだけなら省略できる）")
}

// resolveCommandStore は単発コマンドが使う置き場の名前と root を返す。
// 未指定で FS 置き場が 1 つだけならその置き場、2 つ以上なら指定を求める。
func resolveCommandStore(cmd *cobra.Command, storage config.StorageConfig) (name, root string, err error) {
	name, err = cmd.Flags().GetString(commandStoreFlagName)
	if err != nil {
		return "", "", err
	}
	if name == "" {
		fsNames := storage.FSStoreNames()
		switch len(fsNames) {
		case 0:
			return "", "", fmt.Errorf("storage.stores has no fs store")
		case 1:
			name = fsNames[0]
		default:
			return "", "", fmt.Errorf("--%s is required: storage.stores has %d fs stores (%s)",
				commandStoreFlagName, len(fsNames), strings.Join(fsNames, ", "))
		}
	}
	roots, err := storage.StoreRoots([]string{name})
	if err != nil {
		return "", "", fmt.Errorf("--%s: %w", commandStoreFlagName, err)
	}
	return name, roots[name], nil
}

// resolveMountedStores は `--stores` を解決して検査し、マウントする FS 置き場の名前（`--stores` の順）と
// root を返す。置き場をマウントするのは worker と streamer だけ。api・notifier・watcher は置き場を
// 持たないので、FS 置き場が複数あっても `--stores` を要求しない（効かない `--stores` の指定は
// validateStoreBinding が拒む）。
func resolveMountedStores(cmd *cobra.Command, cfg *config.Config, roles, queues []string, once bool) ([]string, map[string]string, error) {
	var mounted []string
	if slices.Contains(roles, "worker") || slices.Contains(roles, "streamer") {
		var err error
		mounted, err = resolveStoreBinding(cmd, cfg.Storage)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := validateStoreBinding(roles, cmd.Flags().Changed(storeFlagName), cfg.Storage, mounted, queues, once); err != nil {
		return nil, nil, err
	}
	roots, err := cfg.Storage.StoreRoots(mounted)
	if err != nil {
		return nil, nil, fmt.Errorf("--%s: %w", storeFlagName, err)
	}
	return mounted, roots, nil
}

// probeIngestStorageAtStartup は、ingest を引く worker について、マウントした全ての置き場の root で
// ingest の FS 契約を起動時に 1 回確かめ、確かめたかを返す。
//
// ingest は各置き場の root の強い FS 契約（file fsync / Close / 同一 FS の atomic rename /
// 親 directory fsync）に依存するので、実際の操作列を 1 回通す。ingest を購読しない中央 worker は
// 置き場を書かないので probe の対象外。パス文字列から FS の種類は判定しない。
// 目印を確認できないときは probe を保留して false を返し、IngestWorker が最初の ingest で
// 目印を照合した後に probe する。
func probeIngestStorageAtStartup(roles, queues []string, roots map[string]string) (bool, error) {
	markersVerified := verifyStoreMarkersAtStartup(roles, roots)
	pullsIngest := slices.Contains(roles, "worker") && (len(queues) == 0 || slices.Contains(queues, jobs.IngestQueue))
	if !pullsIngest {
		return false, nil
	}
	if !markersVerified {
		slog.Warn("deferring ingest storage probe until the media store markers can be verified",
			"stores", sortedKeys(roots))
		return false, nil
	}
	for _, name := range sortedKeys(roots) {
		if err := worker.ProbeIngestStorage(roots[name]); err != nil {
			return false, fmt.Errorf("validating ingest storage of store %q: %w", name, err)
		}
	}
	return true, nil
}
