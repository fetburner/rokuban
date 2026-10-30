> [recordings.md](../recordings.md) のブラウザ再生。索引から辿る。

# 録画のブラウザ再生

詳細画面の再生枠にはプレイヤーを 1 つだけ置く。ごみ箱・失敗・キャンセルの録画にはプレイヤーを出さない。録画 HLS が利用できる場合は API の master URL を使い、利用できない場合は encoded ファイルへフォールバックする。どちらも無ければ再生操作を出さない。

## 再生元の選び方

| 録画の状態 | 選ぶ再生元 |
|---|---|
| 録画中で `hlsPlayback` がある | 世代付き録画 HLS |
| 完了後の取り込み中で `hlsPlayback` がある | 世代付き録画 HLS |
| 完了済みで cut でない encoded がある | encoded ファイル |
| 上記以外で `hlsPlayback` がある | 世代付き録画 HLS |
| HLS が空で encoded がある | encoded ファイル |
| HLS と encoded がどちらも無い | 再生なし |

`hlsPlayback` が空の応答では HLS 要求を作らない。encoded があればファイル再生を選び、無ければ再生できない状態を表示する。失敗・キャンセル・ごみ箱では、他の再生元が残っていてもプレイヤーを表示しない。

## 世代付き録画 HLS

録画 HLS の master URL は `/api/media/stores/{store}/recordings/{id}/hls/v{gen}/{profile}/master.m3u8` の形で、録画 API が `hlsPlayback` に返す。録画中は ingest temp、commit 後は原本を、同じ世代の URL で読む。詳細は [API の録画 HLS](../../api/media/playback.md) を参照する。

世代が期限切れになると playlist 要求は 410 を返す。プレイヤーは録画詳細を再取得し、新しい世代の URL で止まる直前の位置から再開する。世代切り替え中に `hlsPlayback` が空になっても、再生中に最後に得た URL を保持する。新しい世代が届いたら同じ再生元を更新し、録画状態の通知だけでプレイヤーを作り直さない。

## 再生位置と操作

再生位置は `/api/recordings/{id}/playback-position` に録画時間軸のミリ秒で保存する。HLS と encoded は同じ値を共有し、profile ごとには分けない。cut 版は keep 区間を使って原本時間軸との対応を取る。

録画中は時間軸の右端を録画済み時間に置き、HLS playlist の seekable 末尾は実際のシーク上限として使う。録画 HLS は世代の URL を維持して seek し、範囲外では playlist の範囲内へ丸める。再生開始ボタンを押す前に HLS 配信の要求を送らない。

画質・音声・字幕・再生速度・PiP は共有操作バーから操作する。録画 HLS の速度は端末ごとの好みに保存し、ライブ視聴には適用しない。

## 検証

再生元の選択と `hlsPlayback` が空のときのフォールバックは `recording-playback-source.test.ts`、世代切り替え時の位置保持は `recording-detail.test.tsx` と `web/e2e/recording-playback-timeline.mjs` で固定する。録画 HLS の固定長 segment と変換再起動境界は `web/e2e/recording-segmented-vod.mjs` で測る。
