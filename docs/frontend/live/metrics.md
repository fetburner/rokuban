> [live.md](../live.md) の遅延・バッファ計器。索引から辿る

### 遅延・バッファの計器

視聴中の計器として、放送からの遅延と先読みのバッファを操作バーに出す。値が取れるかは再生経路で決まる。hls.js 経路は両方を出し、ネイティブ HLS 経路は先読みの近似だけを出して、放送からの遅延は表示自体を出さない。この値は、低遅延である証拠にはならない。

#### 計器の位置づけ

denpa の遅延・バッファ表示に相当するものを持つ。中断予測が「見始める前の判断材料」なら、こちらは、いま見ている映像が電波とどれだけ離れているかを示す視聴中の計器である。ON AIR バッジ・録画中バッジと同じく、「いま電波に乗っているものとの距離」を言う表示という位置づけは共通する。

値の取得はプレイヤーが担い、表示はプレイヤー内の「ライブ」印の横、操作バーに置く。プレイヤーはコールバックで値を親へ渡し、親が書式を整えて操作バーへ渡す。

<details><summary>根拠</summary>

- 値の取得と受け渡し: `LivePlayer`（`components/live-player.tsx`）、`onDiagnostics` コールバック prop
- 画面配線: `pages/live.tsx` の表示テスト

</details>

#### 経路ごとに出せる値

hls.js 経路（Chrome / Firefox 等）は、1 秒ごとに放送からの遅延と先読みを読む。

ネイティブ HLS 経路（Safari）は hls.js を経由しないので、遅延に相当する値を取得できない。先読みだけを、バッファの末尾から現在位置を引いた値で近似する。放送からの遅延は、表示自体を出さない。測れないものは出さず、欠損表示（`—`）で埋めることすらしない。

<details><summary>根拠</summary>

- hls.js 経路で読む値: `hls.latency`（放送から）、`hls.mainForwardBufferInfo.len`（先読み）
- ネイティブ経路の近似: `video.buffered` の末尾 - `currentTime`
- 経路ごとの表示差: `lib/live.test.ts`
- 値の契約: `latencySec` は native 経路では `null`
- 計器: `live-player.test.tsx`「計器」。`onDiagnostics` が実値を 1 秒ごとに読むこと、ネイティブの `latencySec=null` を確認する

</details>

#### 実測値と低遅延の証拠にならないこと

同一局・同一端末で、セグメント長 2 秒、30 分測った値は、hls.js 経路で放送からの遅延の中央値 5.0 秒、先読みの中央値 4.1 秒である。ネイティブ HLS 経路で測れるのはバッファの近似だけで、それも live 端の近くが安定せず、同じ条件の 30 分で 53 回の止まりが出た。

計器に出る値は、低遅延であることの証拠にならない。低遅延を目標にしない判断と、`segment_seconds` を短くしても遅延が縮まない理由は、[live-hls](../../api/media/live-hls.md) §「遅延の目標は置かない」が持つ（アプリ配信の経路の判断）。

<details><summary>根拠</summary>

- 測定の設定: `segment_seconds: 2`
- ネイティブ経路で測れる近似: `buffered`

</details>

#### 同期点が決まる前の遅延値

hls.js は、同期点が決まる前の遅延として 0 を返す。NaN は返さない。NaN を前提にすると、実ブラウザでは「放送から約0秒」という偽の測定値が出続ける。

読む側は 0 以下を欠損として弾く。表示は最初「放送から— / 先読み—」で始め、値が確定してから数値に変わる。

<details><summary>根拠</summary>

- 0 を返す実装: `node_modules/hls.js`（1.6.17）の `LatencyController.get latency()` は `this._latency || 0` を実装している。`_latency` は同期点が決まるまで `null` のまま
- 前提にしてはならない値: `NaN`。実際に返る値: `0`
- 欠損として弾く読み取り: `readHlsDiagnostics`（`components/live-player.tsx`）
- 欠損値と `NaN` の丸め: `lib/live.test.ts`
- `hls.latency=0` の欠損扱い: `live-player.test.tsx`「計器」

</details>

#### 測り直すボタンと読み上げの指定

「測り直す」ボタンは置かない。値は 1 秒ごとのポーリングで常に最新へ更新されるので、手動の再計測に操作としての意味が無い。denpa の「測り直す」は WHEP 側の再ネゴシエーションの都合であり、hls.js のポーリングにはそれに対応する操作が無い。

`aria-live` も付けない。毎秒変わる数字を支援技術に読み上げさせる理由が無いためである。

#### 破棄後のポーリング停止

hls.js の fatal エラーで破棄した後は、計器のポーリングを止める。止めるのは、意味の無くなった値を毎秒読み続けない衛生のためであって、例外対策ではない。

実 hls.js は、破棄の後に遅延や先読みを読んでも例外を投げない。内部の hls 参照を null にするだけで、遅延の内部値は直前の値のまま残るためである。

<details><summary>根拠</summary>

- fatal エラー後の停止: `live-player.test.tsx`「計器」
- 破棄の呼び出し: `hls.destroy()`、`latency` / `mainForwardBufferInfo` を読んでも例外が出ないこと
- 参照だけを外す実装: `LatencyController.destroy()`、`destroy()`、内部の `hls` 参照、`_latency`

</details>

#### 実ブラウザでの測定

実 Chrome + 実 hls.js の実測で、「放送から約 n 秒 / 先読み n 秒」が 0 以外の数値になる。bundled Chromium は H.264/AAC のデコードが進まず、遅延が更新されないので、この確認は実 Chrome で行う。

<details><summary>根拠</summary>

- 実ブラウザ: `web/e2e/live.mjs` ③

</details>
