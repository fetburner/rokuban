// Package diskusage は 1 つのマウント（パス）の容量を statfs(2) 相当で観測する。
//
// 呼び出すのはファイルシステムを持つロール（worker）だけ（不変条件 1: api ロールは
// ファイルシステムに依存しない）。このパッケージ自身は DB も River も知らず、
// 1 パスの statfs だけに責務を絞ることで、internal/worker からの単体テストが
// 実際のディスクの数字に依存せず組み立てられるようにする（Usage を差し替え可能な
// フィールドとして worker 側に注入する。internal/worker/storage.go 参照）。
package diskusage

import (
	"fmt"
	"syscall"
)

// Usage は 1 マウントの容量観測結果（バイト単位）。
type Usage struct {
	// TotalBytes はファイルシステム全体の容量。
	TotalBytes int64
	// UsedBytes は Total から Free（本プロセスの権限に関わらず未使用の総ブロック）
	// を引いた値。root 予約領域を「使用済み」側に数える（Available ではなく
	// Free を基準にする理由）。
	UsedBytes int64
	// AvailableBytes は非特権プロセスが実際に書き込める残量
	// （statfs の Bavail。root 予約領域を含まない）。「残高」として UI に出す
	// べき数字はこちら --- Free ではなく Bavail を使うのは、一般ユーザー権限で
	// 動く worker プロセスが実際に使える容量を過大に見せないため。
	AvailableBytes int64
}

// Stat は path が乗っているファイルシステムの容量を観測する。
//
// path 自体は存在するディレクトリでなければならない（syscall.Statfs の要求。
// 中身が空でもよい）。
func Stat(path string) (Usage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	// Bsize の型は GOOS ごとに違う（darwin: uint32, linux: int64）ので明示変換が
	// 要る。Blocks/Bfree/Bavail は darwin/linux のどちらも既に uint64 なので
	// 変換不要（golangci-lint の unconvert が検出する。ビルドタグを避けるための
	// 意図的な非対称）。
	bsize := uint64(stat.Bsize)
	total := stat.Blocks * bsize
	free := stat.Bfree * bsize
	avail := stat.Bavail * bsize

	// free/avail が total を超えることは想定していないが、万一 statfs が
	// 一貫性のない値を返しても signed 変換で暴走した巨大な負数を書き込むより
	// 0 に落とす方が安全（downstream の容量判定が「残量ゼロ」を安全側に誤るだけで済む）。
	used := int64(0)
	if total >= free {
		used = int64(total - free)
	}

	return Usage{
		TotalBytes:     int64(total),
		UsedBytes:      used,
		AvailableBytes: int64(avail),
	}, nil
}
