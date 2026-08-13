> [frontend.md](../frontend.md) の一部。索引から辿る

# ライブ視聴 --- EPGStation 水準のシンプルな UI

リッチな視聴体験（ハードウェアトランスコード、低遅延、コメント連携）は KonomiTV が既に高水準で解決しており、そこに張り合わない。同じ mirakc を共有すれば KonomiTV との共存も可能（チューナーは優先度調停で録画が勝つ）。

Rokuban 自体のライブ視聴は「チャンネル一覧から選んでブラウザ再生、画質切り替え程度」で良い。

## 実装方式

- **mirakc `/api/services/{id}/stream` → ffmpeg → HLS** の薄いパイプ。セグメントはローカルスクラッチに書き、**streamer が配信する**（配信経路の詳細は [api.md](../api.md) のライブ HLS 配信）
- `streamer` ロールが担当。リアルタイムに mirakc からの帯域を張り続けるため、分散モードではエッジ寄りに配置する（エンコードジョブと違いレイテンシ・帯域制約がある）

## セッション管理

- **ライブ視聴セッションは意図的に in-memory**。落ちたらクライアント再接続で済む使い捨て状態であり、「すべての状態を Postgres に」の原則の明示的な例外（参照: [overview.md](../overview.md) の crash-only 設計原則）
- 「クライアントがいなくなったら ffmpeg を止める」idle GC が必要。セグメント要求がアプリを通ることで last-access の更新がタダで手に入る（参照: [api.md](../api.md) のライブ HLS 配信）

## フロントエンド実装

**独立したルート `/live` を持つ。** 番組表グリッドの「いま」から入る形は、グリッド自体が
`lg` 以上でしか出ない（[programs.md](programs.md)「リストを第一級に置く。グリッドは
その上に足す」）ため、モバイルからの入口を別に用意する必要が生じ結局 2 箇所になる。
`/live` はチャンネル一覧（`GET /api/sites/{site}/services`）+ プレイヤー + いま放送中の番組
（既存 EPG API の時間窓クエリ。専用 API は足していない）という 1 画面で構成する
（`pages/live.tsx`）。選択中のチャンネルは `?serviceId=` に持つ（`routes.tsx` の
`validateSearch` が不正な値に `undefined` を**明示代入**して落とす。省略では
消えない --- [recordings.md](recordings.md)「TanStack Router の `validateSearch` は
無効な値を『省略』しても消えない」）。
チャンネル一覧のリンクは `replace` にし、ザッピングでブラウザ履歴が積み上がらない
ようにする。

**「選ぶ」（`?serviceId=` を変える）と「流す」（`LivePlayer` をマウントする）を
別のタップに分ける（issue #234 M7-1）。** チャンネルを選ぶこと自体は probe も
セッション（チューナー確保 + ffmpeg 起動）も起こさない --- チャンネル一覧・
いま放送中の番組・チャンネル種別（GR/BS/CS）の表示だけで、`LivePlayer` は
「再生」ボタンを押すまでマウントしない（`pages/live.tsx` の `playingServiceId`。
`selectedServiceId` と一致するときだけ再生中とみなす）。確認ダイアログは使わない
--- 選択状態の画面そのものが値札であり、再生は 1 タップで足りる。摩擦をコストに
比例させる方針上、デスクトップ LAN でも再生 1 押しより増やさない（ダイアログを
重ねると、チューナーが有限でない環境の利用者にまで同じ摩擦を強いる）。
「チューナーが空いています」等の**肯定・保証は文言に書かない**（下界主義。
[data.md](../data.md) §6.5 と同じ規律 --- mirakc には Rokuban から見えない
消費者がいる）。

`playingServiceId` と `selectedServiceId` の一致判定は**レンダー中に行う**（effect
ではない）。これは直リンク・ブックマークで来た場合だけでなく、チャンネル一覧で
他のチャンネルへ切り替えた場合も同じで、**同意はチャンネルの選択ごとに 1 回必要**
という設計の要点そのものである --- 一度再生した後に別チャンネルへザップし、また
元のチャンネルへ戻ってきても、そのチャンネルの再生は再度「再生」ボタンを押すまで
再開しない。**この判定を `useEffect` で「選択が変わったら false に戻す」形にすると、
1 コミットぶん透過的にバグる**（レビューでの指摘。実測: A 再生中に B へ切り替えると、
jsdom でも実ブラウザでも B 向けの `playlist.m3u8` への要求が 1 件飛ぶ）--- passive
effect は子（`LivePlayer`）→親（`LivePage`）の順に走るため、`selectedServiceId` が
B に変わった直後の 1 コミットだけ古い再生中フラグが残っていて `<LivePlayer
serviceId={B}>` が透過的にマウントされ probe を投げてしまい、その直後に親の
reset effect が走って unmount してももう遅い（`internal/streamer/live.go` の
セッションは `context.WithCancel(context.Background())` で回るため、クライアント側の
`AbortController.abort()` はセッション自体を止めない --- 押していないチャンネルの
チューナー + ffmpeg が idle GC まで 30〜45 秒残る）。レンダー中に判定すれば
`selectedServiceId` が変わった**その場のレンダーで**「再生中でない」が確定し、
異なる serviceId で透過的にマウントされる中間コミット自体が存在しない
（詳細は `pages/live.tsx` の `playingServiceId` 定義部のコメント）。

**直リンク・ブックマーク（`/live?serviceId=` の直開き）も選択状態で止まる。**
再生開始の同意を取る構造は、通常のチャンネル一覧からの選択と直リンクで区別しない
--- 直開きだけ自動再生にすると「タップで選んだときは同意が要るが URL 経由なら
要らない」という一貫しない規則になり、番組行の「ライブで見る」等の外部導線
（`components/program-row.tsx`）から来た場合もチューナーを暗黙に掴んでしまう。

**チャンネル切り替えのデバウンスは廃止した（issue #234 の含むもの 4）。** 以前は
ザッピングのたびに probe / セッション開始が走っていたため、「通り過ぎたチャンネル」
の分だけセッションを積まないよう 400ms のデバウンスを挟んでいた。選択自体が
probe もセッションも起こさなくなった今、デバウンスする対象（= 選択の瞬間に
コストのかかる処理）がそもそも発生しないため、デバウンスは何も守らなくなった
--- 存在理由が消えたので削除した（`pages/live.tsx` に `channelSwitchDebounceMs`
は無い）。チャンネル一覧のリンクはクリックで即座にナビゲートする。

**視聴中チャンネルの情報欄に「この局の番組表」リンクを置く**（issue #231）。
`/`（番組表）の `?serviceId=` へ、視聴中の 1 局を配列 1 要素で渡す
（[programs.md](programs.md)「番組リスト」の絞り込みと同じ形。`serviceId?: number[]`）。
このリンクは通常の push ナビゲーション（`replace` にしない） --- チャンネル一覧の
ザッピングとは違い 1 回だけの遷移なので、戻るボタンで視聴中チャンネルへ戻れる
方が自然。逆方向（番組表 → ライブ）はページ単位の導線を置かない（理由は
[programs.md](programs.md)「番組リスト」の該当箇条書き）。

**ライブへの導線はサーバーの `live.enabled` に連動させる（issue #209）。**
`live.enabled: false` のとき streamer はライブのルートを一切登録しないので、
導線を出しても行き先が無い。判断は `GET /api/capabilities` の `live` に一本化し、
フロント側の入口は `lib/capabilities.ts` だけにする（ナビの出し分けは
[shell.md](shell.md)「無効な機能の項目は出さない」）。`/live` のルート自体は
残し、**直リンク・ブックマークで来たときは「この環境ではライブ視聴が無効です」+
`live.enabled` という手がかりを出す** --- ルートを消すと SPA の 404 になるだけで、
運用者は原因（サーバー設定）に辿り着けない。無効のときはプレイリストを一度も
取りに行かない。

**「無効」と断言するのは `live: false` を実際に受け取ったときだけにする。**
この画面は原因（サーバー設定）を名指しするので、`useLiveEnabled()` の真偽値では
なく `useLiveCapability()` の 4 値を見て、`pending` は読み込み中・`unknown`
（能力 API が失敗）は「利用できるかを確認できませんでした」に分ける。潰すと
**`live.enabled: true` のデプロイで能力 API が瞬断しただけでも「設定が無効」と
表示され**、issue #209 が消したかった「原因にたどり着けない」を別の顔で再演する
（レビューでの指摘。潰した実装で `pages/live.test.tsx` の 2 件が落ちることを
確認済み）。導線側（ナビ）は逆に未確定を無効に倒してよい --- 黙って消えるだけで
誤った原因を主張しないため（[shell.md](shell.md)）。

**「有効」と「今すぐ見られる」は別**である。`live: true` は config の状態だけを
表し、streamer が動いていない / チューナーが埋まっている場合は導線が出たまま
プレイリスト取得の 404 / 503 として下記のエラー分類に出る。

**プロファイル（画質）を選ぶ UI は持たない。** `live.profiles` を列挙する API が
無い（`GET /api/sites/{site}/services/{serviceId}/live/playlist.m3u8` は OpenAPI
対象外なので設定名の一覧を返す仕組みも無い）ため、選択肢を出すと「機能しない
コントロール」になる。既定プロファイル（サーバー側の `live.profiles` 先頭）に
固定し、画質切り替えは将来 `live.profiles` の一覧 API ができてから足す。

**hls.js はライブ視聴画面だけ動的 import する（`components/live-player.tsx`）。**
`pnpm build` の出力で hls.js が `assets/hls-*.js`（約 520 KB）として独立チャンクに
分かれ、他画面のバンドル（`assets/index-*.js`）には乗らないことを確認済み。

**再生経路は 3 段の梯子で選ぶ。各段は「実際に確かめた能力」で選ぶ。**

1. `<video>` が**プレイリストとセグメントの両方**を再生できる → ネイティブ HLS
   （hls.js は import すらしない）
2. hls.js が動く（`Hls.isSupported()`。MSE / ManagedMediaSource がある）→ hls.js
3. どちらも駄目だが `<video>` が m3u8 の MIME に支持を表明する → ネイティブへ
   最後の望みを託す（`claimsHlsPlaylistSupport`）。ここに来るのは MSE も
   ManagedMediaSource も無いブラウザ（iOS 17.1 未満の iPhone Safari）だけで、
   「非対応です」と断じるとネイティブなら完璧に再生できる端末を締め出す

**1 段目でセグメントの MIME（`video/mp2t`）まで問うのが要点。** プレイリストの
MIME（`application/vnd.apple.mpegurl`）に対する `canPlayType` の戻り値では
Safari と Chrome を区別できない --- Playwright の 3 エンジンで実測した値:

| `canPlayType` の引数 | WebKit 605.1.15 | Chromium 151 | Chrome 151 | Firefox 153 |
|---|---|---|---|---|
| `application/vnd.apple.mpegurl` | `maybe` | `maybe` | `maybe` | `''` |
| `application/x-mpegURL` | `maybe` | `maybe` | `maybe` | `''` |
| 上記 + `; codecs="avc1.42E01E,mp4a.40.2"` | `probably` | `probably` | `probably` | `''` |
| **`video/mp2t`** | **`maybe`** | **`''`** | **`''`** | **`''`** |

戻り値を決めているのは **codecs パラメータの有無であってエンジンの違いではない**
（HTML 仕様が「codecs を許す type について、それが無いなら `probably` を返すべき
でない」と定めているため。hls.js 公式 README のパターンが `=== 'probably'` では
なく真偽値チェックなのも同じ理由）。一方 `video/mp2t` --- streamer が実際に
セグメントに付けている Content-Type --- を demux できるのは WebKit だけで、
Chromium / Firefox はできない（hls.js が TS を fMP4 へ remux してから MSE に
載せるのはこのため）。つまりこの問いは「このブラウザは**我々が配るもの**を
そのまま再生できるか」という能力そのものへの問いであり、エンジンの同定でも
拡張子への態度でもない。判定は `web/e2e/live.mjs` の⑥が実ブラウザで固定する。

**再生前に `probeLivePlaylist`（`lib/live.ts`）でプレイリストを 1 回 `fetch` する。**
`<video>` の `error` イベント・hls.js のエラーイベントはいずれも HTTP ステータスや
本文を運ばない。両方の再生経路で同じエラー表示を出すため、実際に `<video>` /
hls.js へ URL を渡す前に 1 回取得して成否を確認する。この GET 自体もセグメント
要求と同じ経路（streamer のアプリ配信）を通るので、idle GC の last-access 更新にも
自然に乗る。

**probe は HTTP 層しか見ないので、メディア層の失敗は `<video>` のイベントで拾う
（`watchNativeMedia`。ネイティブ経路のみ）。** プレイリストが 200 で返ってもセグメントが
壊れていれば再生は始まらない。ここを聴かないと**永久に止まった黒いプレイヤー**に
なる（文言も読み込み表示も再読み込みボタンも出ない）--- WebKit で実測された症状で
ある（発見の経緯は下記「経緯と失敗事例」）。聴く種類は同じ実測から決めた（プレイ
リスト 200 のまま、セグメントだけを 3 通りに壊して WebKit で観測）:

| 壊し方 | `<video>` が出すもの | `video.error` |
|---|---|---|
| セグメントが 404 | `error`（再生中なら `waiting` も） | code 3 `Media failed to decode` |
| セグメントが応答しない | `progress` → `stalled`（3.6 秒後） | null |
| プレイリストの中身が壊れている | `progress` → `stalled`（3.6 秒後） | null |

したがって **`error` だけでは足りない**（下 2 つは `error` を出さない）。一方
`stalled` / `waiting` を即座に失敗と見なすのも誤りで、正常なライブでもバッファ枯れで
出る。**`error` は即時、`stalled` / `waiting` は `nativeStallTimeoutMs`（12 秒。
`stalled` が出るのが途絶から 3 秒後、セグメント長が 2 秒なので、正常なら 3 セグメント
以上落ちないと到達しない）の猶予つき**にし、猶予中に `playing` / `canplay` /
`timeupdate` / `pause` が来たら回復と見なして捨てる。hls.js 経路には張らない
（`Hls.Events.ERROR` が同じ役目を持ち、MSE のバッファ制御で `waiting` が正常に
何度も出るため誤検知になる）。

**一時停止中は猶予を張らない。ただし「一度でも再生が始まった後」に限る。** WebKit は
`pause()` した瞬間にも `stalled` を出す（フェッチを止めるため）が配信は正常で、しかも
解除イベントは一時停止中には来ないので、放置すると**正常な配信に必ずエラー画面が出て
`<video>` が invisible になる**（実測: playing@0.0 → pause@2.3 → stalled@2.3 →
12 秒後にエラー表示）。一方、抑止条件を `paused` だけにすると**まだ再生を押していない
窓まで塞がる** --- `<video>` に `autoPlay` は無いので読み込み直後は常に
`paused === true` であり、そこで無応答が起きると届くイベントは `paused=true` の
`stalled` だけ（20 秒待っても error も waiting も来ない）で、猶予が一度も張られず
永久に黒いままになる。そこで `playing` を一度でも観測したかを持ち、
**`hasStarted && paused` のときだけ抑止する**。再開後に配信が死んだままなら
`waiting` が再送されるので張り直される（実測: pause@6.05s → play@12.05s →
waiting@12.05s）。

判定手段: `live-player.test.tsx` の「ネイティブ経路のメディア失敗」6 件（`error` で
出る / 猶予経過で出る / 猶予中の `playing` で出さない / 一時停止中の `stalled` で
出さない / 猶予中の `pause` で出さない / **再生前**の `stalled` では出す /
再開後に復帰していなければ再び出す）と「hls.js 経路では stalled を拾わない」1 件、
`web/e2e/live.mjs` ⑦（実 WebKit。404 と無応答の両方でエラー表示 + 再読み込みが
出ること。⑦は `play()` を呼ばないので、上の「再生前の窓を塞がない」ことも同時に
見ている）。**一時停止の抑止そのものをブラウザで機械判定する手段は無い** ---
e2e は一時停止を一度も作らないので、そこは jsdom のテストと手動測定が根拠である。**覆えているのは probe 通過後のメディア層だけで、HTTP 層（streamer 不在 /
503 / プレイリスト 404）は従来どおり probe 側が押さえている。**

**エラーは 3 種に分類する（`classifyLiveLoadError`）。**

- `unreachable`（`fetch` 自体が reject）: streamer に到達できない。ハイブリッド
  構成では自宅側が落ちているだけの正常状態でありうる（[overview.md](../overview.md)
  §サーバーレスデプロイ）ため、destructive な赤ではなく中立の文言にする
- `capacity`（503）: 同時セッション上限 / チューナー枯渇 / シャットダウン中の
  いずれか。本文（プレーンテキスト）はいずれも「今は無理なので後で試す」という
  同じ対応を要求するので UI 側の分岐は 1 つにまとめ、本文はそのまま見せる
  （[shell.md](shell.md)「エラーの本文も UI まで運ぶ」と同じ規律）
- `other`: 想定外のステータス。本文をそのまま見せる

いずれも再読み込みボタンで `probeLivePlaylist` からやり直せる。

**チャンネル切り替えは idle GC に任せる。明示的にセッションを閉じる API が無い**
（配られているのは `GET .../live/playlist.m3u8` と `GET .../live/segments/{name}`
のみ）ため、実質これ以外の選択肢がない（サーバー側の即時解放 API は切り出し済みの
別 issue。下記「経緯と失敗事例」）。`LivePlayer` はチャンネル切り替え（`serviceId`
prop の変化）を effect の cleanup で検知し、probe の in-flight `fetch` を
`AbortController` で中断、hls.js の `destroy()` / `<video>` の `src` 解除を
即座に行って**それ以上そのサービスへのセグメント要求を出さない**ところまでは
保証する。

**実配値は `live.idle_timeout` 既定 30 秒 / `live.max_sessions` 既定 4 / GC 周期
`idle_timeout / 2` = 15 秒（`internal/config/config.go` / `internal/streamer/live.go`）。**
セッションは最終アクセスから**30〜45 秒**生き残る。クライアント側がセグメント
要求を止めても、この間サーバー側のチューナーは掴まれたまま。

**選択と視聴開始を分離した（issue #234 M7-1）ことで、この節が本来問題にしていた
「ザッピングのたびにセッションが積まれる」という事態自体が起きなくなった。**
`?serviceId=` を切り替えるだけでは probe もセッション開始も走らないため、
チャンネル一覧を何度触っても掴まれるチューナーは 0 のまま増えない --- ただし
これは `playingServiceId` と `selectedServiceId` の一致判定をレンダー中に行う
実装でのみ真になる（上記「フロントエンド実装」の同じ節参照）。`useEffect` で
「選択が変わったら false に戻す」形にすると、切替直後の 1 コミットだけ押していない
チャンネルへ透過的に probe が飛ぶ（レビューで発見。実測は同節）。ザッピング
緩和のためのデバウンス（400ms。`channelSwitchDebounceMs`）はこの理由により削除
した（`pages/live.tsx` に存在しない。issue #234 の含むもの 4）。掴まれるのは
利用者が明示的に「再生」を押したチャンネルだけであり、その本数分だけ
**最終アクセスから 30〜45 秒**残る（既定 `max_sessions` の 4 を明示的な再生の
連打で超えると、5 回目が 503 `too many concurrent live sessions on this
process`、チューナー本数がそれより少ない環境ではさらに手前で mirakc 側の枯渇に
より 503 `live stream unavailable` になる。この経路自体は変わっていない）。
判定手段: `pages/live.test.tsx`「再生中に別チャンネルへ切り替えると選択状態に戻る
（同意はチャンネルごとに必要）」（`playlistFetchCallCount()` で件数を見る）と
`web/e2e/live.mjs` ⓪'（実ブラウザでの要求ログ観測）。

**503（`capacity`）のエラー文言には「30 秒ほど待って再読み込み」という具体的な
案内を付けている（`LiveErrorMessage`）。** 待てば直ることが読めないと、ユーザーは
「壊れている」と誤解して繰り返しリロード/再訪問し、状況を悪化させる。

**テストの範囲を正確に書く。** jsdom はレイアウト・実再生のいずれも測れないため、
`components/live-player.test.tsx` は 3 層に分けてある:

1. `fetch` をモックして probe の成否とエラー分類ごとの表示・再読み込み・
   チャンネル切り替え時の `fetch` 再実行・in-flight `fetch` の中断
   （`AbortController.abort()` が呼ばれること）を見る
2. `vi.mock('hls.js', ...)` で hls.js 自体をフェイクに差し替え、**hls.js 経路
   （ネイティブ HLS 非対応。Chrome / Firefox 相当）の呼び出しの配線**
   （動的 import 後に `loadSource` / `attachMedia` が呼ばれる、fatal エラーで
   `destroy` が呼ばれる、切り替え・破棄で古いインスタンスが `destroy` される）
   を見る。この層が無いと Chrome / Firefox が実際に通る経路が単体テストで
   一度も検証されない（実際にそうなっていた。下記「経緯と失敗事例」）。
   `supportsNativeHls` を `true` に固定する・cleanup を削除する、のいずれも
   この層のテストが `waitFor` のタイムアウトとして落ちることを確認済み
3. 純関数（URL 組み立て・エラー分類・初期チャンネル選択・時間窓・
   `probeLivePlaylist` の中断伝播）は `lib/live.test.ts` に切り出してテストする

ただし 2 は**フェイクの配線が正しく呼ばれること**の検査であり、hls.js の
「動的 import が本当に別バンドルチャンクとして届く」ことや「実際に MSE へ
セグメントを投入して再生が進む」ことは検証していない。この 2 点と、
「チャンネル切り替え後に旧 `serviceId` へのセグメント要求が実際に 0 件になる」
（= 保証の実効性そのもの）は `web/e2e/live.mjs` が実ブラウザ・実 hls.js で担う
（後述「実機確認について」）。

### 実機確認について

**実機確認の手段・判定項目・回帰の記録は [runbook.md](../runbook.md)（ライブ視聴の
節）と `web/e2e/`（`web/e2e/README.md`）を見る。** ここには設計に跳ね返る 2 点だけ
残す。

- **再生経路の 3 段の梯子（上記）は、実ブラウザ（WebKit / Chromium / Chrome /
  Firefox）で実測した `canPlayType` の値の表で固定した。** 判定を変えるときは
  同じ実測をやり直す（`lib/live.test.ts` は実測値の表を入力にしている）
- **iOS 実機（iPhone Safari）は誰も確認していない。** 判定は macOS の 4 エンジン
  で実測して固定したが、iOS の `canPlayType('video/mp2t')` が macOS の WebKit と
  同じ `'maybe'` を返す保証は無い。違っていた場合、iOS は hls.js 経路へ落ちる
  （iOS 17.1 以降は ManagedMediaSource で再生できる。それ未満は上記 3 段目の
  最後の砦で `<video>` に直接渡る）。この判定が正しいかは
  [runbook.md](../runbook.md)（ライブ視聴の節）の実機確認でしか確定しない

## 経緯と失敗事例

- UI の決定と「チューナー数が少ない環境で何が見えるかを含めて判断する」という
  問いは issue #92（M4-4）。配っている API（playlist / segments の GET のみ）は
  M4-3 で決まった形
- `watchNativeMedia` はレビュー #190 の 3 回目の指摘で入った。当初は誰も
  `<video>` のメディアイベントを聴いておらず、プレイリスト 200 のままセグメント
  が壊れると「永久に止まった黒いプレイヤー」になることが WebKit の実測で確認
  された
- 同じレビュー（#190）まで hls.js 経路の配線テスト（`vi.mock` の層）が無く、
  `supportsNativeHls` を常に `true` に固定しても・cleanup を丸ごと削除しても
  既存テストが全部通っていた --- Chrome / Firefox が実際に通る経路が単体テスト
  で一度も検証されていなかった
- ユニットテストの盲点の実例: `expect(supportsNativeHls(() =>
  'probably')).toBe(true)` は通っていたが、**`'probably'` を返す実ブラウザは
  存在しない**ので、実在しない入力についての主張であり何も守っていなかった。
  現在は `lib/live.test.ts` が実測値の表（エンジンごとの戻り値）を入力にしている
- 実機確認（`web/e2e/live.mjs`）が発見した本番相当の回帰 2 件
  （`canPlayType` の `'maybe'` をネイティブ対応と誤判定 → Chrome 全ユーザーが
  沈黙して再生不能 / その修正がどの実ブラウザでも false → Safari まで hls.js
  経路に落ちる）の詳細は [runbook.md](../runbook.md)（ライブ視聴の節）と
  `web/e2e/README.md` に記録がある。2 件目の実害は macOS では 523 KB の不要な
  ダウンロードと docs の虚偽記載（「Safari はネイティブを使う」と書きながら
  そうなっていない）に留まるが、**iPhone Safari は危険**だった ---
  `window.MediaSource` を持たない iOS 17.1 未満では `Hls.isSupported()` が
  false になり、ネイティブなら完璧に再生できる端末に「このブラウザはライブ視聴
  （HLS）に対応していません」が出る
- サーバー側の即時セッション解放 API は
  [issue #191](https://github.com/fetburner/rokuban/issues/191) へ切り出し済み
- **`live.enabled: false` でも導線が出ていた**（issue #209）。probe は
  `response.ok` を見るが、ライブのルートが無いパスは SPA フォールバックの
  **HTML を 200 で返していた**ので probe が通り、hls.js / `<video>` が m3u8 と
  して解釈できずに再生エラーになっていた --- 「無効」ではなく「壊れている」と
  読める壊れ方。直したのは 2 箇所で、**どちらか一方だけでは足りなかった**:
  能力 API（導線を消す）と `/api/` の 404 化（probe が HTML を成功扱いしない）
- **e2e（`live.mjs`）はライブが有効なサーバーを前提にする**ので、
  `/api/capabilities` も `page.route` で差し替えている（プレイリスト/セグメントと
  同じ扱い）。差し替えを `{live: false}` にすると①〜⑦のすべてが NG になることを
  実測した（macOS / ffmpeg あり / Chromium + WebKit。`live.enabled: false` の
  サーバーに対して実行。ffmpeg が無い環境では①〜⑤は NG ではなく「未測定」に
  落ちるので、この観測は ffmpeg のある環境での話）--- 判定がこの導線に依存して
  いることの裏取りでもある
- **同じ確認で `live.mjs` ④ が #208 以降ずっと測れていなかったことが判明した。**
  セグメント URL に載るのは mirakc 合成 id（`networkId * 100000 + serviceId`）
  なのに、SI の `serviceId` で照合していたため `network_id` が 0 でない環境
  （実 EPG も runbook の投入例も該当）では待機が必ずタイムアウトしていた。
  合成 id を `GET /api/sites/{site}/services` から解決する形に直した
- 実 mirakc / 実チューナーでの idle GC・録画優先度調停・実 ISDB-T トランス
  コードの確認手順は [runbook.md](../runbook.md)（ライブ視聴の節）にあるが、
  M4-4 の時点では ISDB-T チューナーも mirakc も無い開発環境のため実行されて
  いない
