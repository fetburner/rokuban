> [live.md](../live.md) の離脱ヒントとセッション表示。索引から辿る

### 離脱ヒントとセッション表示

チャンネルの切り替えや離脱では、サーバーへ「離脱のヒント」を送る。サーバーはセッションを止めず、idle 期限を短い猶予まで詰めるだけである。セッションを閉じる API にしないのは、サービス単位で共有されるセッションを閉じると、他の視聴者の視聴まで止まるためである。

#### 離脱時にプレイヤーが止めるもの

チャンネルを切り替えると、プレイヤーは進行中の probe を中断し、再生部品を即座に破棄して、そのサービスへのセグメント要求を出さないようにする。そのうえで離脱のヒントを送る。

<details><summary>根拠</summary>

- 切り替えの検知: `LivePlayer` が `serviceId` prop の変化を effect の cleanup で見る
- probe の中断: in-flight の `fetch` を `AbortController` で中断する
- 再生部品の破棄: hls.js の `destroy()` と `<video>` の `src` 解除
- ヒントの送信: `POST .../live/leave`、`sendPlaybackLeaveHint`

</details>

#### ヒントは停止命令ではない

サーバーは離脱のヒントを受けても、セッションを止めず、idle 期限を短い猶予（既定 8 秒）まで詰めるだけである。同じチャンネルを見ている別の視聴者の要求が期限を戻すので、ヒントは送れなくても送りすぎても正しく動かなくなることはない。

この扱いは [api.md](../../api.md) §ライブ視聴の HLS「離脱は『ヒント』であって停止命令ではない」に決まっている。

セッションを閉じる素朴な API にしてはならない。セッションはサービス単位で共有されるので、閉じる操作は他人の視聴も止める。それを避ける参照カウントはセッション identity の導入になり、「ライブの URL にセッション ID を置かない」という決定を差し戻す。

ヒントと猶予なら、既存のセグメント要求が「まだ誰か見ている」をレベルトリガーとして示し続ける。起動待ち（「読み込み中…」の区間）の視聴者も切られない。サーバーは待たせている間、last-access を更新し続ける。

<details><summary>根拠</summary>

- 既存のセグメント要求が見ている印として働くこと（レベルトリガー）: 不変条件 5

</details>

#### 送信手段と発火点

ヒントはページ離脱の最中にも届く手段で送る。発火点は、切り替え・停止・画面遷移の後始末と、ページの離脱・非表示のイベントである。再読み込みでは送らない。

送信はビーコン送信を使い、無ければ keepalive 付きの fetch にする。ページ離脱の最中は、通常の fetch がドキュメント破棄で中断されうるためである。

unload は使わない。モバイル Safari では bfcache のために発火しないからである。非表示のイベント（visibilitychange）も併せて聴くのは、タブが破棄されずに非表示のまま放置される経路では、離脱のイベント（pagehide）すら来ないためである。非表示で送っても正しく動かなくなることはない。音声だけを聴き続けるなどでセグメント要求が続いていれば、期限が戻るためである。

再読み込みボタンでは送らない。再読み込みは離脱ではないためである。probe の effect とは別の effect に分けてあり、再読み込みの回数には依存させない。`rokuban_live_leave_hints_total` が離脱以外を数えると、idle GC の回収数と対で読めなくなる。

<details><summary>根拠</summary>

- 送信手段: `navigator.sendBeacon`、`keepalive` つきの `fetch`
- 使わない発火点: `unload`
- 使う発火点: cleanup、`pagehide`、`visibilitychange`（hidden）
- 再読み込みで送らない: `retryNonce` に依存させない

</details>

#### 実配値と実測

既定の idle 期限は 30 秒、猶予は 8 秒、GC 周期は 4 秒である。セッションが消えるまでの実測は、ヒントありで 13 秒、ヒント無しで 33 秒だった。

設定キーは `live.idle_timeout`（既定 30 秒）と `live.max_sessions`（既定 4）である。猶予は `3 × segment_seconds + 2s`、GC 周期は猶予の半分とする。

実バイナリを偽 mirakc と偽 ffmpeg につないで測った。`rokuban_live_active_sessions{kind="live"}` が 0 に戻るまでを 1 秒間隔でポーリングした。実チューナー・実 ffmpeg では、ffmpeg の停止に掛かる時間だけ伸びうる（未測定）。手順は [runbook.md](../../runbook.md) のライブ視聴の節 ①-4 にある。

<details><summary>根拠</summary>

- GC 周期と猶予の設定: `internal/config/config.go`、`internal/streamer/live.go` の `gcInterval`
- 実測環境と active session の観測: `rokuban server --roles streamer`、`rokuban_live_active_sessions{kind="live"}`

</details>

#### チャンネル選択と視聴開始の分離

チャンネルの選択と視聴の開始は別の操作なので、ザッピングのたびにセッションは積まれない。利用者が明示的に「再生」を押したチャンネルだけがセッションを取り、既定の上限を再生の連打で超えると 503 になる。

`?service=` を切り替えるだけでは probe もセッション開始も走らず、一覧を何度触っても、取られるチューナーは増えない。

再生したチャンネルのセッションは、離脱ヒントが届けば十数秒、届かなければ最終アクセスから 30 秒強残る（実測値は上の節）。既定の `max_sessions` 4 を明示的な再生の連打で超えると、5 回目は 503 `too many concurrent live sessions on this process` になる。チューナー本数がそれより少ない環境では、mirakc 側の枯渇により手前で 503 `live stream unavailable` になる。

<details><summary>根拠</summary>

- 選択だけでは probe・セッションを始めず、再生同意が必要: [selection.md](selection.md) の `playingKey` / `selectedKey`、`LivePlayer`
- 別チャンネルへ切り替えたときの選択状態と要求数: `pages/live.test.tsx`「再生中に別チャンネルへ切り替えると選択状態に戻る（同意はチャンネルごとに必要）」、`playlistFetchCallCount()`
- 実ブラウザでの要求ログ: `web/e2e/live.mjs` ⓪'

</details>

#### 503 の案内文言

容量不足（503）のエラー文言には、「30 秒ほど待って再読み込み」という具体的な案内を付ける。待ち時間は、ヒントの届かない経路があるので、短い猶予には合わせず、長い idle 期限に合わせる。

待てば直ることが読めないと、利用者が壊れていると誤解して繰り返しリロードや再訪問をし、状況を悪化させる。

ヒントが機能すれば、実際の待ちは猶予（既定 8 秒）で済む。しかし、ヒントの届かない経路（ビーコンが落ちた・別端末が取っている）では、短い方を書くと「待ったのに直らない」が起きる。

<details><summary>根拠</summary>

- 容量不足の分類: `capacity`
- 案内文言の実装: `LiveErrorMessage`
- 案内に使う長い方の期限: `idle_timeout`

</details>

#### テストの範囲

jsdom はレイアウトも実再生も測れないので、判定を fetch のモック・hls.js のフェイク・純関数の 3 層に分ける。動的 import のチャンクや MSE での再生、切替後の旧サービスへの要求 0 件は、実ブラウザで確認する。

1 層目は、fetch をモックして、probe の成否・エラー分類ごとの表示・再読み込み・チャンネル切替時の再実行・in-flight の fetch の中断を確認する。中断時は `AbortController.abort()` が呼ばれることも見る。

2 層目は、hls.js をフェイクに差し替え、ネイティブ HLS 非対応の環境で通る hls.js 経路の呼び出しを見る。動的 import 後の `loadSource` / `attachMedia`、fatal エラー後の `destroy`、切替や破棄で古いインスタンスの `destroy` を確認する。この層が無いと、Chrome / Firefox が実際に使う経路は単体テストで一度も検証されない。層を足す前は、ネイティブ判定を常に true にしても後始末を削除してもテストが通った。現在はどちらの変異も待機のタイムアウトで落ちる。

3 層目は、URL 組み立て・エラー分類・初期チャンネル選択・時間窓・probe の中断伝播を純関数として確認する。

実ブラウザ + 実バイナリの画質切替では、4 点を見る。一覧が遅れて届いても再取得しないこと、切替で `<video>` が作り直されず音量・ミュートが保たれること、離脱ヒントを送らないこと、`?profile=` が要求に載ることである。hls.js のフェイク配線は、動的 import が別バンドルチャンクとして届くことや、MSE にセグメントを入れて再生が進むことを検証しない。これらと、切替後の旧サービスへのセグメント要求が 0 件になることは、実ブラウザで確認する。

<details><summary>根拠</summary>

- fetch の probe・エラー・再読み込み・切替・中断: `components/live-player.test.tsx`
- hls.js 経路の配線: `components/live-player.test.tsx`、`vi.mock('hls.js', ...)`、`supportsNativeHls`
- 変異検出: `supportsNativeHls` を `true` に固定する、cleanup を削除する、の両方が `waitFor` で落ちる
- 純関数の URL・分類・初期選択・時間窓・中断伝播: `lib/live.test.ts`、`probeLivePlaylist`
- 切替後に旧サービスへ出ないセグメント要求: `serviceId`
- 実ブラウザ + 実バイナリの画質切替と旧 service の要求ログ: `web/e2e/live.mjs` ⑨、`web/e2e/live.mjs`、[verification.md](verification.md)「実機確認について」

</details>
