> [live.md](../live.md) のブラウザ再生能力とエラー処理。索引から辿る

### ブラウザ再生能力、probe、エラー分類

**hls.js はライブ視聴画面だけ動的 import する（`components/live-player.tsx`）**。`pnpm build` の出力で hls.js が `assets/hls-*.js`（約 520 KB）として独立チャンクに分かれることを確認済み。他画面のバンドル（`assets/index-*.js`）には乗らない。

**再生経路は 3 段の梯子で選ぶ。各段は「実際に確かめた能力」で選ぶ。**

1. `<video>` が**プレイリストとセグメントの両方**を再生できる → ネイティブ HLS（hls.js は import すらしない）
2. hls.js が動く（`Hls.isSupported()`。MSE / ManagedMediaSource がある）→ hls.js
3. どちらも駄目だが `<video>` が m3u8 の MIME に支持を表明する → ネイティブへ最後の望みを託す（`claimsHlsPlaylistSupport`）。ここに来るのは MSE も ManagedMediaSource も無いブラウザ（iOS 17.1 未満の iPhone Safari）だけで、「非対応です」と断じるとネイティブなら完璧に再生できる端末を締め出す

**1 段目でセグメントの MIME（`video/mp2t`）まで問うのが要点**。プレイリストの MIME（`application/vnd.apple.mpegurl`）に対する `canPlayType` の戻り値では Safari と Chrome を区別できない。Playwright の 3 エンジンで実測した値:

| `canPlayType` の引数 | WebKit 605.1.15 | Chromium 151 | Chrome 151 | Firefox 153 |
|---|---|---|---|---|
| `application/vnd.apple.mpegurl` | `maybe` | `maybe` | `maybe` | `''` |
| `application/x-mpegURL` | `maybe` | `maybe` | `maybe` | `''` |
| 上記 + `; codecs="avc1.42E01E,mp4a.40.2"` | `probably` | `probably` | `probably` | `''` |
| **`video/mp2t`** | **`maybe`** | **`''`** | **`''`** | **`''`** |

戻り値を決めるのは **codecs パラメータの有無で、エンジンの違いではない**。HTML 仕様では、codecs を許す type に codecs が無い場合 `probably` を返すべきではない。hls.js 公式 README も `=== 'probably'` ではなく真偽値で判定する。

`video/mp2t` は streamer が実際にセグメントへ付ける Content-Type である。これを demux できるのは WebKit だけで、Chromium / Firefox はできない。hls.js が TS を fMP4 へ remux して MSE に載せる理由もここにある。

つまり判定するのは、ブラウザが配信物をそのまま再生できるかである。エンジンや拡張子で推測しない。判定は `web/e2e/live.mjs` の⑥が実ブラウザで固定する。

**この表をテストの入力にする**。**実在しない戻り値で主張しない**。`expect(supportsNativeHls(() => 'probably')).toBe(true)` は通る。しかし **`'probably'` を返す実ブラウザは存在しない**ので、これは実在しない入力についての主張であり、何も守っていない。`lib/live.test.ts` はこの実測値の表を入力にしている。判定を変えるときは同じ実測をやり直す。

**再生前に `probeLivePlaylist`（`lib/live.ts`）でプレイリストを 1 回 `fetch` する。** `<video>` の `error` イベント・hls.js のエラーイベントはいずれも HTTP ステータスや本文を運ばない。両方の再生経路で同じエラー表示を出すため、実際に `<video>` / hls.js へ URL を渡す前に 1 回取得して成否を確認する。この GET 自体もセグメント要求と同じ経路（streamer のアプリ配信）を通るので、idle GC の last-access 更新にも自然に乗る。

**probe は HTTP 層しか見ないので、メディア層の失敗は `<video>` のイベントで拾う（`watchNativeMedia`。ネイティブ経路のみ）。** プレイリストが 200 で返ってもセグメントが壊れていれば再生は始まらない。ここを聴かないと**永久に止まった黒いプレイヤー**になる（文言も読み込み表示も再読み込みボタンも出ない）--- WebKit で実測された症状である。聴く種類は同じ実測から決めた（プレイリスト 200 のまま、セグメントだけを 3 通りに壊して WebKit で観測）:

| 壊し方 | `<video>` が出すもの | `video.error` |
|---|---|---|
| セグメントが 404 | `error`（再生中なら `waiting` も） | code 3 `Media failed to decode` |
| セグメントが応答しない | `progress` → `stalled`（3.6 秒後） | null |
| プレイリストの中身が壊れている | `progress` → `stalled`（3.6 秒後） | null |

したがって **`error` だけでは足りない**。セグメントの無応答や壊れたプレイリストでは `error` が出ない。一方、`stalled` / `waiting` を即失敗とするのも誤りで、正常なライブでもバッファ枯れで出る。

**`error` は即時、`stalled` / `waiting` は `liveStallTimeoutMs`（12 秒）の猶予つきにする**。猶予の根拠は、`stalled` が出るのが途絶から 3 秒後、セグメント長が 2 秒なので、正常なら 3 セグメント以上落ちないと到達しないことである。

猶予中に `playing` / `canplay` / `timeupdate` / `pause` が来たら回復として捨てる。満了したらエラー表示の前に画質の自動降格を試す（[quality.md](quality.md)「停滞したら画質を 1 段下げる」）。

hls.js 経路にはこの watcher を張らない。`Hls.Events.ERROR` が同じ役割を持ち、MSE では `waiting` が正常時にも出るため誤検知になる。

**一時停止中は猶予を張らない。ただし再生が一度始まった後に限る。** WebKit は `pause()` した瞬間にも `stalled` を出す（フェッチを止めるため）が、配信は正常である。解除イベントは一時停止中には来ないので、放置すると正常な配信に必ずエラー画面が出て `<video>` が invisible になる。実測は playing@0.0 → pause@2.3 → stalled@2.3 → 12 秒後にエラー表示だった。

`paused` だけで抑止すると、再生前の監視まで止まる。`<video>` に `autoPlay` は無く、読み込み直後は `paused === true` である。無応答時は `stalled` だけが届き、20 秒待っても `error` / `waiting` は来ないため、永久に黒いままになる。

そのため `playing` を一度でも観測したかを持ち、`hasStarted && paused` のときだけ抑止する。再開後も配信が止まっていれば `waiting` が再送される。実測は pause@6.05s → play@12.05s → waiting@12.05s である。

判定手段は `live-player.test.tsx` の「ネイティブ経路のメディア失敗」である。内訳は `error` で出る / 猶予経過で出る / 猶予中の `playing` で出さない / 一時停止中の `stalled` で出さない / 猶予中の `pause` で出さない、である。さらに再生前の `stalled` では出す / 再開後に復帰していなければ再び出す、も見る。

実 WebKit では 404 と無応答の両方でエラー表示と再読み込みが出る。e2e は `play()` を呼ばないため、再生前の窓を塞がないことも同時に見ている。

**一時停止の抑止そのものは実ブラウザで機械判定できていない。** e2e は一時停止を作らないため、そこは jsdom のテストと手動測定が根拠である。probe 通過後のメディア層を対象にし、HTTP 層（streamer 不在 / 503 / playlist 404）は probe 側が確認する。

<details><summary>根拠</summary>

- `error` と猶予経過でエラーを出す: `live-player.test.tsx`「ネイティブ経路のメディア失敗」
- 猶予中の `playing` / `pause` と、一時停止中の `stalled` では出さない: `live-player.test.tsx`「ネイティブ経路のメディア失敗」
- 再生前の `stalled` では出し、再開後に復帰していなければ再び出す: `live-player.test.tsx`「ネイティブ経路のメディア失敗」
- hls.js 経路では `stalled` を拾わない: `live-player.test.tsx`
- 実 WebKit で 404 と無応答の両方にエラー表示と再読み込みが出る。`play()` を呼ばないので再生前の窓を塞がないことも見る: `web/e2e/live.mjs` ⑦

</details>

**エラーは 3 種に分類する（`classifyLiveLoadError`）。**

- `unreachable`（`fetch` 自体が reject）: streamer に到達できない。ハイブリッド構成では自宅側が落ちているだけの正常状態でありうる（[overview.md](../../overview.md) §サーバーレスデプロイ）ため、destructive な赤ではなく中立の文言にする
- `capacity`（503）: 同時セッション上限 / チューナー枯渇 / シャットダウン中のいずれか。本文（プレーンテキスト）はいずれも「今は無理なので後で試す」という同じ対応を要求するので UI 側の分岐は 1 つにまとめ、本文はそのまま見せる（[shell.md](../shell.md)「エラーの本文も UI まで運ぶ」と同じ規律）
- `other`: 想定外のステータス。本文をそのまま見せる

いずれも再読み込みボタンで `probeLivePlaylist` からやり直せる。
