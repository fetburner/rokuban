> [live.md](../live.md) の遅延・バッファ計器。索引から辿る

### 遅延・バッファの計器

denpa の遅延・バッファ表示に相当するものを持つ。中断予測が
「見始める前の判断材料」なら、こちらは「いま見ている映像が電波とどれだけ
離れているか」という**視聴中の**計器で、ON AIR バッジ・録画中バッジと同じ
「いま電波に乗っているものとの距離」を言う表示という位置づけは共通する。
値の取得は `LivePlayer`（`components/live-player.tsx`）が担うが、表示自体は
プレイヤー内の「ライブ」印の横、操作バーに置く。`LivePlayer` は
`onDiagnostics` コールバック prop で値を親へ渡し、親が書式を整えて操作バーへ渡す。

**経路によって出せるものが違う。**

- hls.js 経路（Chrome / Firefox 等）: 1 秒ごとに `hls.latency`（放送から）と
  `hls.mainForwardBufferInfo.len`（先読み）を読む
- ネイティブ HLS 経路（Safari）: hls.js を経由しないため `latency` に相当する
  値を取得できない。「先読み」だけを `video.buffered` の末尾 - `currentTime`
  で近似し、「放送から」は**表示自体を出さない**（測れないものを出さない ---
  欠損表示（`—`）で埋めることすらしない）

**この計器が実測で出した値**（同一局・同一端末、`segment_seconds: 2`、30 分）は、
hls.js 経路で `hls.latency` の中央値 5.0 秒・先読みの中央値 4.1 秒である。ネイティブ HLS
経路で測れるのは `buffered` の近似だけで、それも live 端近傍が安定せず、同じ条件の 30 分で
53 回の止まりが出た。**計器に出る値は「低遅延である」ことの証拠にはならない。**
低遅延を目標にしない判断と、`segment_seconds` を短くしても遅延が縮まない理由は
[live-hls](../../api/media/live-hls.md) §「遅延の目標は置かない」が持つ（アプリ配信の経路の判断）。

**`hls.latency` は同期点が決まる前も `NaN` ではなく `0` を返す**。
`node_modules/hls.js`（1.6.17）の `LatencyController.get latency()` は
`this._latency || 0` を実装している。`_latency` は同期点が決まるまで `null`
のままである。つまり `NaN` を前提にすると、実ブラウザでは「放送から約0秒」という
偽の測定値が出続ける。読む側
（`readHlsDiagnostics`。`components/live-player.tsx`）は `0` 以下を欠損として
弾く。表示は最初「放送から— / 先読み—」で始め、値が確定してから数値に変わる。

**「測り直す」ボタンは無い**。値は 1 秒ごとのポーリングで常に最新へ更新
されるため、手動の再計測に操作としての意味がない（denpa の「測り直す」は
WHEP 側の再ネゴシエーションの都合であり、hls.js のポーリングにはそれに
対応する操作が無い）。**`aria-live` も付けない** --- 毎秒変わる数字を
支援技術に読み上げさせる理由が無い。

**hls.js の fatal エラーで `hls.destroy()` した後は計器のポーリングを止める**。
実 hls.js は `destroy()` 後に `latency` / `mainForwardBufferInfo` を読んでも
例外は投げない。`LatencyController.destroy()` は内部の `hls` 参照を `null`
にするだけで、`_latency` は直前値のまま残るためである。それでも止めるのは、意味の
無くなった値を毎秒読み続けない衛生のためであって例外対策ではない。

**実測（実 Chrome + 実 hls.js）**: 「放送から約 n 秒 / 先読み n 秒」が 0 以外の数値になる。bundled Chromium は H.264/AAC のデコードが進まず `hls.latency` が更新されないため、この確認は実 Chrome で行う。

<details><summary>根拠</summary>

- 純関数: `lib/live.test.ts`（欠損値・`NaN` の丸め・経路ごとの表示差）
- 値の契約: `latencySec` は native 経路では `null`
- 計器: `live-player.test.tsx`「計器」。`onDiagnostics` が実値を 1 秒ごとに読むこと、ネイティブの `latencySec=null`、`hls.latency=0` の欠損扱い、fatal エラー後の停止を確認する
- 画面配線: `pages/live.tsx` の表示テスト
- 実ブラウザ: `web/e2e/live.mjs` ③

</details>
