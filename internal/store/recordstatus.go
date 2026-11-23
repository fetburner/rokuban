package store

import "log/slog"

// NormalizeRecordingStatus は mirakc の RecordInfo.Status（値域を制限する型を持たない素の
// string）を recordings.status の閉じた値域に正規化する（issue #130）。
//
// mirakc の RecordingStatus は 4 バリアントの網羅的 enum で、ワイヤに出る値は
// recording/finished/canceled/failed の 4 つで閉じていることをソースで確認済み
// （issue #130）。だが RecordInfo.Status 自体は素の string なので、mirakc が
// 将来 5 つ目の値を追加すると Rokuban はコンパイル時に気付けない。未知の値を
// そのまま CreateRecording / UpdateRecordingStatus に渡すと recordings_status_check
// 違反で ProcessRecord のトランザクション全体がロールバックし、record_sync にも
// 観測が残らないまま同じ record を毎パス再試行し続ける
// （実際に canceled だけがこの壊れ方をしていたのが #130 本体）。
//
// 未知の値は 'failed' に丸める。'canceled' を 'failed' に丸めると「録画一覧が
// 嘘をつく」という #130 が避けた問題と混同しないこと --- あちらは**分かっている
// 2つの異なる事実**（取消と失敗）を同じ値に潰すから嘘になる。ここは逆に
// **何が起きたか分からない**という状態そのものが事実であり、その生の値は
// record_sync.status（CHECK 無し。docs/schema/record-sync.md「mirakc の
// recordingStatus そのまま」）に手を加えず残る。recordings.status 側は「正常に
// 完了しなかった」という粗い事実だけを表現する。5 値目を CHECK / openapi.yaml の
// enum に足さない選択（issue #130 の「決めること」）に伴う代償として、この粗さは
// 許容する（TestRecordObservationsContract/record status values are normalized）。
//
// 未知の値を観測したら必ず slog.Error でログに残す。次に mirakc が値を足したら、
// このログを見た人間が本関数と recordings_status_check / openapi.yaml の enum を
// 更新すること。switch の case は recordings_status_check が許す値集合と一致させる。
func NormalizeRecordingStatus(recordID, raw string) string {
	switch raw {
	case RecordingStatusRecording, RecordingStatusFinished, RecordingStatusCanceled, RecordingStatusFailed:
		return raw
	default:
		slog.Error("unknown mirakc recording status; storing as failed", "record_id", recordID, "raw_status", raw)
		return RecordingStatusFailed
	}
}
