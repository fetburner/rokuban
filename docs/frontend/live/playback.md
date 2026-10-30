> [live.md](../live.md) のブラウザ再生能力とエラー処理。索引から辿る

# ブラウザ再生能力とエラー処理

ライブ視聴と録画 HLS は hls.js で再生する。ライブを再生する前にプレイリストを probe して HTTP 層の失敗を分類し、再生中は hls.js の致命的エラーと再生位置の停滞を扱う。録画 HLS の選択、世代切替、位置復元は [録画再生](../recordings/playback.md) に記す。

## hls.js の読み込みと非対応表示

hls.js は再生画面だけが動的に import する。他の画面の初期バンドルには含めない。ビルドで独立チャンクになることを確認する。

再生可能かどうかは `Hls.isSupported()` で決める。真なら hls.js を作り、偽なら再生方式に応じた非対応メッセージを表示する。`canPlayType()` の戻り値で経路を変えない。ブラウザが HLS の MIME に `maybe` を返すことは、アプリが配る TS を再生できる根拠にならない。

`Hls.isSupported()` が偽のときは `<video>` に m3u8 URL を渡さず、利用者が理由を読める案内を出す。

<details><summary>根拠</summary>

- 動的 import と方式別の非対応表示: `components/live-player.tsx`
- `canPlayType()` が `maybe` でも hls.js を使う: `components/live-player.test.tsx`

</details>

## ライブ再生前の probe

ライブ playlist の GET を 1 回行い、到達可能性と HTTP status を確かめてから hls.js に URL を渡す。hls.js のエラーイベントは HTTP status や応答本文を持たないため、probe の応答で失敗を分類する。

probe はセグメント要求と同じアプリ配信を通るので、idle GC の last-access 更新にも乗る。成功時は同じ応答本文から、全プロファイルを束ねた master かを調べる。

<details><summary>根拠</summary>

- probe 関数: `probeLivePlaylist`（`lib/live.ts`）
- probe の後に再生 URL を渡す: `LivePlayer`
- master 判定: `bundlesProfiles`

</details>

## HTTP エラーの分類

ネットワーク到達不能は中立の文言、503 は再試行を案内する容量エラー、その他の status は本文を含めて表示する。録画 HLS も同じ分類関数を使い、録画向けの見出しと案内文を出す。再読み込みは probe からやり直す。

503 はライブではセッション上限・チューナー枯渇・シャットダウン中などを表す。録画 HLS では変換待ちの上限到達などを表す。原因の違いは応答本文で伝え、配信方式ごとの例外分類は置かない。

<details><summary>根拠</summary>

- 共通分類: `classifyLiveLoadError`（`lib/live.ts`）
- 方式ごとの見出し・案内文: `playbackStrategy`（`lib/playback-mode.ts`）
- 応答本文を UI まで運ぶ: `LiveErrorMessage`

</details>

## 再生中の失敗と停滞

hls.js の致命的エラーは再生を止め、方式に応じたエラー表示を出す。表示後も裏でセグメント要求が続かないよう、インスタンスを破棄する。

ライブ映像の停滞は `currentTime` を 1 秒ごとに観測し、12 秒進まないときに画質の自動降格を試す。`stalled` / `waiting` は MSE で正常時にも発生するため、失敗判定には使わない。録画 HLS ではライブ向けの自動降格をしない。詳細は [画質選択と自動降格](quality.md) を参照する。

<details><summary>根拠</summary>

- hls.js の致命的エラー: `Hls.Events.ERROR`、`data.fatal`
- 停滞の観測: `watchLiveDiagnostics`、`observeStall`
- 12 秒の境界、pause と hidden の抑止: `live-player.test.tsx`

</details>
