> [live.md](../live.md) の離脱ヒントとセッション表示。索引から辿る

### 離脱ヒントとセッション表示

**チャンネル切り替え・離脱では「離脱のヒント」を送る**。`LivePlayer` はチャンネル
切り替え（`serviceId` prop の変化）を effect の cleanup で検知する。そこで probe の
in-flight `fetch` を `AbortController` で中断する。hls.js の `destroy()` /
`<video>` の `src` 解除を即座に行い、**それ以上そのサービスへのセグメント要求を
出さない**ようにする。そのうえで `POST .../live/leave` を投げる
（`sendLiveLeaveHint`）。

- **離脱ヒントは停止命令ではない。** サーバーはセッションを止めず、idle 期限を短い猶予（既定 8 秒）まで詰めるだけである（[api.md](../../api.md) §ライブ視聴の HLS「離脱は『ヒント』であって停止命令ではない」）。
- 同じチャンネルを見ている別の視聴者がいれば、その人の要求が期限を戻す。
  そのためヒントは送れなくても、送りすぎても壊れない。
- **素朴な「セッションを閉じる API」にしてはならない。**
  セッションはサービス単位で共有されるため、閉じる操作は他人の視聴も止める。
- それを避ける参照カウントは、セッション identity の導入になり、「ライブの URL にセッション ID を置かない」という決定を差し戻す。
- ヒントと猶予なら、既存のセグメント要求が「まだ誰か見ている」をレベルトリガーとして示し続ける（不変条件 5）。
  起動待ち（「読み込み中…」の区間）の視聴者も切られない。サーバーは待たせている間 last-access を更新し続ける。
- **送信は `navigator.sendBeacon`、無ければ `keepalive` つきの `fetch`。**
  ページ離脱の瞬間は通常の `fetch` がドキュメント破棄で中断されうる
- **発火点は cleanup（切り替え・停止・画面遷移）と `pagehide` /
  `visibilitychange`（hidden）。`unload` は使わない** --- モバイル Safari では
  bfcache のため発火しない。`visibilitychange` も併せて聴くのは、タブが破棄されず
  hidden のまま放置される経路では `pagehide` すら来ないため。hidden で送っても
  壊れないのは上記のとおり（音声だけ聴き続けている等でセグメント要求が続いて
  いれば期限が戻る）
- **「再読み込み」ボタンでは送らない**（離脱ではない）。probe の effect とは別の
  effect に分けてあり、`retryNonce` に依存させない ---
  `rokuban_live_leave_hints_total` が離脱以外を数えると、idle GC 回収数と対で
  読めなくなる

**実配値は次のとおり**。`live.idle_timeout` は既定 30 秒、`live.max_sessions` は既定 4 である。
猶予は 8 秒（`3 × segment_seconds + 2s`）、GC 周期は猶予の半分の 4 秒とする。
実バイナリを偽 mirakc と偽 ffmpeg につないで測った。
`rokuban_live_active_sessions{kind="live"}` が 0 に戻るまでを 1 秒間隔でポーリングし、ヒントありは 13 秒、ヒント無しは 33 秒だった。
実チューナー・実 ffmpeg では ffmpeg の停止に掛かる時間だけ伸びうる（未測定）。
手順は [runbook.md](../../runbook.md) のライブ視聴の節 ①-4。

<details><summary>根拠</summary>

- GC 周期と猶予の設定: `internal/config/config.go`、`internal/streamer/live.go` の `gcInterval`
- 実測環境と active session の観測: `rokuban server --roles streamer`、`rokuban_live_active_sessions{kind="live"}`

</details>

**チャンネル選択と視聴開始を分けるため、ザッピングのたびにセッションは積まれない。**
`?service=` を切り替えるだけでは probe もセッション開始も走らず、一覧を何度触っても掴まれるチューナーは増えない。
- 利用者が明示的に「再生」を押したチャンネルだけがセッションを掴む。
  離脱ヒントが届けば十数秒、届かなければ最終アクセスから 30 秒強残る（実測値は上記）。
- 既定の `max_sessions` 4 を明示的な再生の連打で超えると、5 回目は 503 `too many concurrent live sessions on this process` になる。
- チューナー本数がそれより少ない環境では、mirakc 側の枯渇により手前で 503 `live stream unavailable` になる。

<details><summary>根拠</summary>

- 選択だけでは probe・セッションを始めず、再生同意が必要: [selection.md](selection.md) の `playingKey` / `selectedKey`、`LivePlayer`
- 別チャンネルへ切り替えたときの選択状態と要求数: `pages/live.test.tsx`「再生中に別チャンネルへ切り替えると選択状態に戻る（同意はチャンネルごとに必要）」、`playlistFetchCallCount()`
- 実ブラウザでの要求ログ: `web/e2e/live.mjs` ⓪'

</details>

**503（`capacity`）のエラー文言には「30 秒ほど待って再読み込み」という具体的な
案内を付けている（`LiveErrorMessage`）**。待てば直ることが読めないと、ユーザーは
「壊れている」と誤解して繰り返しリロード/再訪問し、状況を悪化させる。**離脱ヒントが
効けば実際の待ちは猶予（既定 8 秒）ぶんで済むが、案内は長い方（`idle_timeout`）の
ままにする** --- ヒントの届かない経路（beacon が落ちた・別端末が掴んでいる）が
ある。短い方を書くと「待ったのに直らない」が起きる。

**テストの範囲を正確に書く。** jsdom はレイアウト・実再生のいずれも測れないため、判定は 3 層に分ける。
1. fetch をモックし、probe の成否・エラー分類ごとの表示・再読み込み・チャンネル切替時の再実行・in-flight fetch の中断を確認する。
   中断時は `AbortController.abort()` が呼ばれることも見る。
2. hls.js をフェイクに差し替え、ネイティブ HLS 非対応環境で通る hls.js 経路の呼び出し配線を見る。
   - 動的 import 後の `loadSource` / `attachMedia`、fatal エラー後の `destroy`、切替や破棄で古いインスタンスの `destroy` を確認する。
   - この層が無いと、Chrome / Firefox が実際に使う経路は単体テストで一度も検証されない。
   - 層を足す前は `supportsNativeHls` を常に true にしても cleanup を削除してもテストが通った。現在はどちらの変異も `waitFor` のタイムアウトになる。
3. URL 組み立て・エラー分類・初期チャンネル選択・時間窓・probe の中断伝播は純関数として確認する。

実ブラウザ + 実バイナリの画質切替では、一覧が遅れて届いても再取得しないこと、切替で `<video>` が作り直されず音量・ミュートが保たれること、離脱ヒントを送らないこと、`?profile=` が要求に載ることを見る。
hls.js のフェイク配線は、動的 import が別バンドルチャンクとして届くことや MSE にセグメントを入れて再生が進むことを検証しない。これらと切替後の旧 `serviceId` へのセグメント要求が 0 件になることは実ブラウザで確認する。

<details><summary>根拠</summary>

- fetch の probe・エラー・再読み込み・切替・中断: `components/live-player.test.tsx`
- hls.js 経路の配線: `components/live-player.test.tsx`、`vi.mock('hls.js', ...)`、`supportsNativeHls`
- 変異検出: `supportsNativeHls` を `true` に固定する、cleanup を削除する、の両方が `waitFor` で落ちる
- 純関数の URL・分類・初期選択・時間窓・中断伝播: `lib/live.test.ts`、`probeLivePlaylist`
- 実ブラウザ + 実バイナリの画質切替と旧 service の要求ログ: `web/e2e/live.mjs` ⑨、`web/e2e/live.mjs`、[verification.md](verification.md)「実機確認について」

</details>
