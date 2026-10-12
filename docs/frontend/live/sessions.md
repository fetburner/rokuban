> [live.md](../live.md) の離脱ヒントとセッション表示。索引から辿る

**チャンネル切り替え・離脱では「離脱のヒント」を送る**。`LivePlayer` はチャンネル
切り替え（`serviceId` prop の変化）を effect の cleanup で検知する。そこで probe の
in-flight `fetch` を `AbortController` で中断する。hls.js の `destroy()` /
`<video>` の `src` 解除を即座に行い、**それ以上そのサービスへのセグメント要求を
出さない**ようにする。そのうえで `POST .../live/leave` を投げる
（`sendLiveLeaveHint`）。

- **これは停止命令ではない**。サーバー側はセッションを止めず、idle 期限を短い猶予
  （既定 8 秒）まで詰めるだけである。同じチャンネルを見ている別の視聴者がいれば、
  その人の要求が期限を戻す（理由と形は [api.md](../../api.md) §ライブ視聴の HLS
  「離脱は『ヒント』であって停止命令ではない」）。したがって**送れなくても
  送りすぎても壊れない**。**素朴な「セッションを閉じる API」にしてはならない**
  --- セッションはサービス単位で共有されるので、他人の視聴を止められる形になる。
  それを避けようとすると参照カウント = セッション identity の導入になり、
  「ライブの URL にセッション ID を置かない」という決定を差し戻すことになる。
  ヒント + 猶予にすれば、既存の観測（セグメント要求）が「まだ誰か見ている」を
  勝手に主張し続けてくれる（レベルトリガー、不変条件 5）。**起動待ち（プレイヤーが「読み込み中…」を出している
  区間）の視聴者も切られない** --- サーバー側は待たせている間 last-access を
  更新し続ける（同節「待っている客も客」）
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

**実配値は次のとおり**。`live.idle_timeout` は既定 30 秒、`live.max_sessions` は
既定 4、猶予は 8 秒（`3 × segment_seconds + 2s`）である。GC 周期は猶予の半分 = 4 秒
（`internal/config/config.go` / `internal/streamer/live.go` の `gcInterval`）。**実測は実バイナリ
`rokuban server --roles streamer` + 偽 mirakc + 偽 ffmpeg で行った**。
`rokuban_live_active_sessions{kind="live"}`
が 0 に戻るまでを 1 秒間隔でポーリングした結果は、**ヒントあり 13 秒 / ヒント無し 33 秒**である。
実チューナー・実 ffmpeg では ffmpeg の停止に掛かる時間だけ伸びうる（未測定）。
手順は [runbook.md](../../runbook.md) のライブ視聴の節 ①-4。

**選択と視聴開始を分離したことで、「ザッピングのたびにセッションが積まれる」と
いう事態自体が起きなくなった**。
`?service=` を切り替えるだけでは probe もセッション開始も走らないため、
チャンネル一覧を何度触っても掴まれるチューナーは 0 のまま増えない。前提は
上記「フロントエンド実装」の `playingKey`/`selectedKey` の判定である。
掴まれるのは利用者が明示的に「再生」を押したチャンネルだけであり、その本数分
だけ**離脱ヒントが届けば十数秒、届かなければ最終アクセスから 30 秒強**残る
（実測値は下記）。既定 `max_sessions` の 4 を明示的な再生の連打で超えると、
5 回目が 503 `too many concurrent live sessions on this process` になる。
チューナー本数がそれより少ない環境ではさらに手前で mirakc 側の枯渇により
503 `live stream unavailable` になる。
判定手段は `pages/live.test.tsx`「再生中に別チャンネルへ切り替えると選択状態に戻る
（同意はチャンネルごとに必要）」である（`playlistFetchCallCount()` で件数を見る）。
`web/e2e/live.mjs` ⓪'（実ブラウザでの要求ログ観測）も見る。

**503（`capacity`）のエラー文言には「30 秒ほど待って再読み込み」という具体的な
案内を付けている（`LiveErrorMessage`）**。待てば直ることが読めないと、ユーザーは
「壊れている」と誤解して繰り返しリロード/再訪問し、状況を悪化させる。**離脱ヒントが
効けば実際の待ちは猶予（既定 8 秒）ぶんで済むが、案内は長い方（`idle_timeout`）の
ままにする** --- ヒントの届かない経路（beacon が落ちた・別端末が掴んでいる）が
ある。短い方を書くと「待ったのに直らない」が起きる。

**テストの範囲を正確に書く。** jsdom はレイアウト・実再生のいずれも測れないため、
`components/live-player.test.tsx` は 3 層に分けてある:

1. `fetch` をモックして probe の成否を見る。あわせてエラー分類ごとの表示・
   再読み込み・チャンネル切り替え時の `fetch` 再実行・in-flight `fetch` の中断
   （`AbortController.abort()` が呼ばれること）も見る
2. `vi.mock('hls.js', ...)` で hls.js 自体をフェイクに差し替え、**hls.js 経路
   （ネイティブ HLS 非対応。Chrome / Firefox 相当）の呼び出しの配線**を見る。
   内訳は、動的 import 後に `loadSource` / `attachMedia` が呼ばれる、fatal エラーで
   `destroy` が呼ばれる、切り替え・破棄で古いインスタンスが `destroy` される、である。
   この層が無いと Chrome / Firefox が実際に通る経路が単体テストで
   一度も検証されない（実際に、この層を足すまでは `supportsNativeHls` を常に
   `true` に固定しても cleanup を丸ごと削除しても既存テストが全部通っていた）。
   `supportsNativeHls` を `true` に固定する・cleanup を削除する、のいずれも
   この層のテストが `waitFor` のタイムアウトとして落ちることを確認済み
3. 純関数（URL 組み立て・エラー分類・初期チャンネル選択・時間窓・
   `probeLivePlaylist` の中断伝播）は `lib/live.test.ts` に切り出してテストする

`web/e2e/live.mjs` の ⑨（実ブラウザ + 実バイナリ）は画質切替を担う。見るのは
4 点である。一覧が遅れて届いてもプレイリストを取り直さないこと。切替で
`<video>` が作り直されないこと（音量・ミュートが保たれること）。切替が
離脱ヒントを送らないこと。`?profile=` が実際に要求に載ること。

ただし 2 は**フェイクの配線が正しく呼ばれること**の検査であり、hls.js の
「動的 import が本当に別バンドルチャンクとして届く」ことや「実際に MSE へ
セグメントを投入して再生が進む」ことは検証していない。この 2 点は
`web/e2e/live.mjs` が実ブラウザ・実 hls.js で担う。あわせて「チャンネル切り替え後に
旧 `serviceId` へのセグメント要求が実際に 0 件になる」こと（= 保証の実効性そのもの）
も担う（後述「実機確認について」）。
