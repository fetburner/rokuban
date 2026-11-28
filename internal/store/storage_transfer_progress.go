package store

// TransferProgressLeaseInput はロックした転送進捗行から lease の引き継ぎを決める値を表す。
type TransferProgressLeaseInput struct {
	// WriterGeneration は転送進捗行の現在の書き手世代である。
	WriterGeneration int64
	// HandedOff は前の slice が進捗を保存して lease を切ったことを表す。
	HandedOff bool
	// Offset は既に処理した転送元のバイト数であり、転送または照合の位置を表す。
	Offset int64
	// HashState は保存済みの SHA-256 状態である。
	HashState []byte
	// UploadStore は再開できる送信の宛先の置き場名を指す。DB の upload_store が NULL なら nil になる。
	UploadStore *string
	// UploadState は保存済みの送信状態である。
	UploadState []byte
	// VerifyingSource は原本の照合が途中であることを表す。
	VerifyingSource bool
}

// PlanTransferProgressLease はロック済みの転送進捗行から lease の引き継ぎを決める。
// leaseLive は方言の実装が DB の時計で計算する。
func PlanTransferProgressLease(input TransferProgressLeaseInput, leaseLive bool) (TransferProgress, bool) {
	if !input.HandedOff && leaseLive {
		return TransferProgress{}, false
	}

	progress := TransferProgress{
		PrevWriterGeneration: input.WriterGeneration,
		WriterGeneration:     input.WriterGeneration + 1,
	}
	if input.HandedOff {
		progress = TransferProgress{
			PrevWriterGeneration: input.WriterGeneration,
			WriterGeneration:     input.WriterGeneration,
			Resumed:              true,
			Offset:               input.Offset,
			HashState:            input.HashState,
		}
		if input.UploadStore != nil {
			progress.UploadStore = *input.UploadStore
			progress.UploadState = input.UploadState
		}
	}
	progress.VerifyingSource = input.VerifyingSource
	return progress, true
}
