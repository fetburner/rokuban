package store

// DeleteReconcileStores は、削除 reconcile が 1 つの置き場に束ねて使う永続化の契約である。
type DeleteReconcileStores struct {
	MediaDelete MediaDelete
	// MediaPendingReclaim は、原本の置き場と派生物の置き場が同じ FS のとき、その FS にある pending 派生物を回収する。S3 の独立パスは DerivedObjectDeleteStores の契約を使う。
	MediaPendingReclaim PendingCopyReclaim
	StorageTransfer     StorageTransfer
	RecordingPurge      RecordingPurge
	// DerivedObjectDeleteStores は、派生物の置き場を掃く独立した削除パスの契約である。
	// nil なら、この置き場の worker は S3 の独立パスを持たない（置き場の種類で決まる正常な状態）。
	DerivedObjectDeleteStores *DerivedObjectDeleteStores
}
