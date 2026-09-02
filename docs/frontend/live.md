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
`/live` はレジストリの全 site から作るチャンネル一覧（各 site の
`GET /api/sites/{site}/services`）+ プレイヤー + いま放送中の番組
（既存 EPG API の時間窓クエリ。専用 API は足していない）という 1 画面で構成する
（`pages/live.tsx`）。選択中のチャンネルは `?service=<Service.id>` に持つ（`routes.tsx` の
`validateSearch` が不正な値に `undefined` を**明示代入**して落とす。省略では
消えない --- [recordings.md](recordings.md)「TanStack Router の `validateSearch` は
無効な値を『省略』しても消えない」）。
チャンネル一覧のリンクは `replace` にし、ザッピングでブラウザ履歴が積み上がらない
ようにする。

**site も含めてチャンネルを同定する。SI の `serviceId` 単独では network をまたぐと一意でない。** Mirakurun が
`networkId * 100000 + serviceId` の合成 id を発明した理由そのもので、
`GET /api/sites/{site}/services` は GR / BS / CS を混ぜて返すため、同じ
`serviceId` を持つサービスが 2 つ返る構成がありうる。**そこで API が合成 id を
`Service.id`（`networkId * 100000 + serviceId`）として返し、画面内の同定は
site と組み合わせて行う**（選択中のハイライトと `aria-current`・再生中
チャンネルの記憶）。**`/live` の URL は `?service=<Service.id>&site=<site>` で
site も運ぶ**（`?service=` の値域も `/programs` と同じ生成スキーマで検証する）。
初期選択は「site と `Service.id` が一致すればそれ、無ければ番組を持つ先頭」だけで決まる
（`pickInitialService`。`lib/live.ts`）。
番組リスト（`components/program-row.tsx`）の「ライブで見る」リンクは
`ProgramListItem` が SI の `networkId` / `serviceId` しか持たないため、
`composeServiceId`（`lib/service-id.ts`）で合成してから渡す。

**番組表と録画の絞り込みも network を含む厳密形式を持つ。** 高松の地上波だけを
受信する実運用 mirakc では 19 サービス中の重複は 0 件だったが、この測定は GR の
範囲しか覆わない。公式割当には BS `(network_id=4, service_id=101)` と 110 度 CS
`(network_id=6, service_id=101)` の実例があり、GR / BS / CS を混ぜる一般の構成では
`serviceId` 単独を identity にできない。

「この局の番組表」も番組表ピッカーの複数選択も録画の絞り込みも、同じ
`?service=<Service.id>` の配列で運ぶ（1 局なら 1 要素）。ライブでは site も
`?site=<site>` で運ぶ。録画では site が
別軸（`?site=`）で、軸内は OR・軸間は AND。

**「選ぶ」（`?service=` を変える）と「流す」（`LivePlayer` をマウントする）を
別のタップに分ける。** チャンネルを選ぶこと自体は probe も
セッション（チューナー確保 + ffmpeg 起動）も起こさない --- チャンネル一覧・
いま放送中の番組・チャンネル種別（GR/BS/CS）の表示だけで、`LivePlayer` は
「再生」ボタンを押すまでマウントしない（`pages/live.tsx` の `playingKey`。
`selectedKey` と一致するときだけ再生中とみなす）。確認ダイアログは使わない
--- 選択状態の画面そのものが値札であり、再生は 1 タップで足りる。摩擦をコストに
比例させる方針上、デスクトップ LAN でも再生 1 押しより増やさない（ダイアログを
重ねると、チューナーが有限でない環境の利用者にまで同じ摩擦を強いる）。
「チューナーが空いています」等の**肯定・保証は文言に書かない**（下界主義。
[data.md](../data.md) §6.5 と同じ規律 --- mirakc には Rokuban から見えない
消費者がいる）。

`playingKey` と `selectedKey` の一致判定は**レンダー中に行う**（effect
ではない）。これは直リンク・ブックマークで来た場合だけでなく、チャンネル一覧で
他のチャンネルへ切り替えた場合も同じで、**同意はチャンネルの選択ごとに 1 回必要**
という設計の要点そのものである --- 一度再生した後に別チャンネルへザップし、また
元のチャンネルへ戻ってきても、そのチャンネルの再生は再度「再生」ボタンを押すまで
再開しない。**この判定を `useEffect` で「選択が変わったら false に戻す」形にすると、
1 コミットぶん透過的にバグる**（レビューでの指摘。実測: A 再生中に B へ切り替えると、
jsdom でも実ブラウザでも B 向けの `playlist.m3u8` への要求が 1 件飛ぶ）--- passive
effect は子（`LivePlayer`）→親（`LivePage`）の順に走るため、`selectedKey` が
B に変わった直後の 1 コミットだけ古い再生中フラグが残っていて `<LivePlayer
serviceId={B}>` が透過的にマウントされ probe を投げてしまい、その直後に親の
reset effect が走って unmount してももう遅い（`internal/streamer/live.go` の
セッションは `context.WithCancel(context.Background())` で回るため、クライアント側の
`AbortController.abort()` はセッション自体を止めない --- 押していないチャンネルの
チューナー + ffmpeg が残る。**離脱ヒントを送っても縮むだけで 0 にはならない** ---
押していないチャンネルを掴む時間は「猶予（既定 8 秒）+ GC 周期」であって、
掴まないのとは違う）。レンダー中に判定すれば
`selectedKey` が変わった**その場のレンダーで**「再生中でない」が確定し、
異なる serviceId で透過的にマウントされる中間コミット自体が存在しない
（詳細は `pages/live.tsx` の `playingKey` 定義部のコメント）。

**直リンク・ブックマーク（`/live?service=<Service.id>` の直開き）も選択状態で止まる。**
再生開始の同意を取る構造は、通常のチャンネル一覧からの選択と直リンクで区別しない
--- 直開きだけ自動再生にすると「タップで選んだときは同意が要るが URL 経由なら
要らない」という一貫しない規則になり、番組行の「ライブで見る」等の外部導線
（`components/program-row.tsx`）から来た場合もチューナーを暗黙に掴んでしまう。

**チャンネル切り替えのデバウンスは持たない。** 選択自体が probe もセッションも
起こさないので、デバウンスする対象（= 選択の瞬間にコストのかかる処理）が
そもそも発生しない --- 何も守らないものは置かない（`pages/live.tsx` に
`channelSwitchDebounceMs` は無い）。チャンネル一覧のリンクはクリックで即座に
ナビゲートする。

**視聴中チャンネルの情報欄に「この局の番組表」リンクを置く。**
`/programs`（番組表）の `?service=` へ、視聴中の 1 局を配列 1 要素で渡す
（[programs.md](programs.md)「番組リスト」の絞り込みと同じ形。`service?: number[]`）。
このリンクは通常の push ナビゲーション（`replace` にしない） --- チャンネル一覧の
ザッピングとは違い 1 回だけの遷移なので、戻るボタンで視聴中チャンネルへ戻れる
方が自然。逆方向（番組表 → ライブ）はページ単位の導線を置かない（理由は
[programs.md](programs.md)「番組リスト」の該当箇条書き）。

**ライブへの導線はサーバーの `live.enabled` に連動させる。**
`live.enabled: false` のとき streamer はライブのルートを一切登録しないので、
導線を出しても行き先が無い。判断は `GET /api/capabilities` の `live` に一本化し、
フロント側の入口は `lib/capabilities.ts` だけにする（ナビの出し分けは
[shell.md](shell.md)「無効な機能の項目は出さない」）。`/live` のルート自体は
残し、**直リンク・ブックマークで来たときは「この環境ではライブ視聴が無効です」+
`live.enabled` という手がかりを出す** --- ルートを消すと SPA の 404 になるだけで、
運用者は原因（サーバー設定）に辿り着けない。無効のときはプレイリストを一度も
取りに行かない。

**probe（`response.ok`）だけでは「無効」を検出できない。** `/api/` 配下を SPA
フォールバックに落とすと、ライブのルートが無いパスは index.html を 200 で返し、
probe は通ってしまう --- その後 hls.js / `<video>` が m3u8 として解釈できずに
再生エラーになり、「無効」ではなく「壊れている」と読める。だから `/api/` 配下は
JSON 404 にしてある（`internal/api/spa.go` の `spaOrAPINotFound`）。**能力 API
（導線を消す）とこの 404 化は両方要る。どちらか一方では足りない。**

**「無効」と断言するのは `live: false` を実際に受け取ったときだけにする。**
この画面は原因（サーバー設定）を名指しするので、`useLiveEnabled()` の真偽値では
なく `useLiveCapability()` の 4 値を見て、`pending` は読み込み中・`unknown`
（能力 API が失敗）は「利用できるかを確認できませんでした」に分ける。潰すと
**`live.enabled: true` のデプロイで能力 API が瞬断しただけでも「設定が無効」と
表示され**、この画面が消したかった「原因にたどり着けない」を別の顔で再演する
（潰した実装で `pages/live.test.tsx` の 2 件が落ちることを確認済み）。導線側
（ナビ）は逆に未確定を無効に倒してよい --- 黙って消えるだけで誤った原因を
主張しないため（[shell.md](shell.md)）。

**「有効」と「今すぐ見られる」は別**である。`live: true` は config の状態だけを
表し、streamer が動いていない / チューナーが埋まっている場合は導線が出たまま
プレイリスト取得の 404 / 503 として下記のエラー分類に出る。

**プロファイル（画質）を選ぶ UI は持たない。** `live.profiles` を列挙する API が
無い（`GET /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/playlist.m3u8`
は OpenAPI
対象外なので設定名の一覧を返す仕組みも無い）ため、選択肢を出すと「機能しない
コントロール」になる。既定プロファイル（サーバー側の `live.profiles` 先頭）に
固定し、画質切り替えは将来 `live.profiles` の一覧 API ができてから足す。

**ライブのページキー操作は M（ミュート）と F（フルスクリーン）だけにする。**
録画向けの速度変更とシークは出さない。入力欄・選択欄・リンク・ボタン・編集可能領域と
`<video>` にフォーカスがあるときは処理せず、ネイティブ controls と二重に効かせない。
ネイティブ HLS の `playbackRate` が実 Safari で有効かは未検証なので、速度変更を
ライブへ広げる根拠にはしない。

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

**この表をテストの入力にする。実在しない戻り値で主張しない。**
`expect(supportsNativeHls(() => 'probably')).toBe(true)` は通るが、
**`'probably'` を返す実ブラウザは存在しない**ので、実在しない入力についての
主張であり何も守っていない（`lib/live.test.ts` はこの実測値の表を入力にしている。
判定を変えるときは同じ実測をやり直す）。

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
ある。聴く種類は同じ実測から決めた（プレイリスト 200 のまま、セグメントだけを
3 通りに壊して WebKit で観測）:

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

**チャンネル切り替え・離脱では「離脱のヒント」を送る。** `LivePlayer` はチャンネル
切り替え（`serviceId` prop の変化）を effect の cleanup で検知し、probe の
in-flight `fetch` を `AbortController` で中断、hls.js の `destroy()` /
`<video>` の `src` 解除を即座に行って**それ以上そのサービスへのセグメント要求を
出さない**ようにしたうえで、`POST .../live/leave` を投げる
（`sendLiveLeaveHint`）。

- **これは停止命令ではない。** サーバー側はセッションを止めず idle 期限を短い猶予
  （既定 8 秒）まで詰めるだけで、同じチャンネルを見ている別の視聴者がいれば
  その人の要求が期限を戻す（理由と形は [api.md](../api.md) §ライブ視聴の HLS
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

**実配値は `live.idle_timeout` 既定 30 秒 / `live.max_sessions` 既定 4 / 猶予
8 秒（`3 × segment_seconds + 2s`）/ GC 周期は猶予の半分 = 4 秒
（`internal/config/config.go` / `internal/streamer/live.go`）。**実測（実バイナリ
`rokuban server --roles streamer` + 偽 mirakc + 偽 ffmpeg。`rokuban_live_active_sessions`
が 0 に戻るまでを 1 秒間隔でポーリング）: **ヒントあり 13 秒 / ヒント無し 33 秒**。
実チューナー・実 ffmpeg では ffmpeg の停止に掛かる時間だけ伸びうる（未測定）。
手順は [runbook.md](../runbook.md) のライブ視聴の節 ①-4。

**選択と視聴開始を分離したことで、「ザッピングのたびにセッションが積まれる」と
いう事態自体が起きなくなった。**
`?service=` を切り替えるだけでは probe もセッション開始も走らないため、
チャンネル一覧を何度触っても掴まれるチューナーは 0 のまま増えない。前提は
上記「フロントエンド実装」の `playingKey`/`selectedKey` の判定である。
掴まれるのは利用者が明示的に「再生」を押したチャンネルだけであり、その本数分
だけ**離脱ヒントが届けば十数秒、届かなければ最終アクセスから 30 秒強**残る
（実測値は下記）。既定 `max_sessions` の 4 を明示的な再生の連打で超えると、
5 回目が 503 `too many concurrent live sessions on this process` になる。
チューナー本数がそれより少ない環境ではさらに手前で mirakc 側の枯渇により
503 `live stream unavailable` になる。
判定手段: `pages/live.test.tsx`「再生中に別チャンネルへ切り替えると選択状態に戻る
（同意はチャンネルごとに必要）」（`playlistFetchCallCount()` で件数を見る）と
`web/e2e/live.mjs` ⓪'（実ブラウザでの要求ログ観測）。

**503（`capacity`）のエラー文言には「30 秒ほど待って再読み込み」という具体的な
案内を付けている（`LiveErrorMessage`）。** 待てば直ることが読めないと、ユーザーは
「壊れている」と誤解して繰り返しリロード/再訪問し、状況を悪化させる。**離脱ヒントが
効けば実際の待ちは猶予（既定 8 秒）ぶんで済むが、案内は長い方（`idle_timeout`）の
ままにする** --- ヒントの届かない経路（beacon が落ちた・別端末が掴んでいる）が
あるので、短い方を書くと「待ったのに直らない」が起きる。

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
   一度も検証されない（実際に、この層を足すまでは `supportsNativeHls` を常に
   `true` に固定しても cleanup を丸ごと削除しても既存テストが全部通っていた）。
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

### 録画予約による中断予測

mirakc の優先度調停では録画が勝つため、視聴中に同じチャンネル種別の録画予約が
始まるとチューナーを取られて視聴側が中断されうる。Rokuban は予約（desired state）
を持っているので、視聴開始前に「この後中断されうるか」を知らせられる ---
EPGStation・KonomiTV には構造的にできない表示。

**判定は純関数 `lib/live-interruption.ts` の `upcomingInterruptingReservation`。**
`GET /api/reservations`（全サイト分。絞り込みパラメータを持たない）が返す
`Reservation.channelType`（program_snapshots 由来のスナップショット）を、視聴対象の
`Service.channelType` と直接比較する。予約は `site` が視聴対象の site と一致する
ものだけを見る（docs/schema.md §1 の設計原則）。

以前は `Reservation` がチャンネル種別を持たなかったため、視聴対象と同じ種別の
サービスに絞って EPG（`GET /api/sites/{site}/programs`）から programId を引き、
`(site, programId)` で突き合わせる第 2 クエリが要った。`Reservation.channelType`
が追加されたことでこの EPG 側の join 自体が不要になった。

- **先読みの時間窓は 2 時間。** 視聴を選ぶ／始める瞬間の判断材料として出す表示
  なので、窓は「これから見始める 1 回の視聴」がカバーする範囲に合わせる。1 番組
  （30 分〜1 時間）を見ている間に次の番組の録画が競合し得ることまでは見せたいが、
  24 時間先の録画予約まで警告すると「今まさに見るかどうかの判断」には関係の薄い
  予約まで出てノイズになる
- **skip の予約は除外する（サーバーの需要計算と同じ規則）。** `effective.skip` が
  true の予約は reconciler が mirakc に同期しないためチューナーを消費しない
  （`internal/capacity/load.go` の `demandFromRow` --- `eff.IsSkipped()` が true の
  行は容量の需要から除外される。docs/data.md §6.5）。API が返す `Reservation.skip`
  はまさにこの `effective.skip` なので、フロント側もこの値で同じ除外を行う
- **下界主義は容量バッジと同じ規律（docs/data.md §6.5）。** 「中断されます」と
  断言しない --- チューナーに余裕があれば中断されないが、余裕があるとも言えない
  （見えない消費者。並走 EPGStation・他のライブ視聴セッション・mirakc の
  `excluded_channels`。**加えてチャンネル種別一致だけを見ているため、BS/CS
  兼用チューナー等、別種別でも同じチューナーを取り合う構成では警告が出ない
  --- サーバー側の Hall 条件は `tuner_sync.types` でこの兼用を扱うが、フロントは
  そこまで見ていない。沈黙側に倒れているので下界主義には反しないが既知の盲点**）。
  文言は「HH:MM から録画予約があります。チューナーが
  不足すると視聴は中断されます」という条件付きにする。**「録画予約はありません =
  安全に見られます」は出さない** --- 肯定的な文言を一切持たない。沈黙は保証では
  ない（`CapacityShortfallBadge` / `ProgramOverlapWarning` と同じ「余計な枠を
  出さない」流儀）
- **鮮度は SSE の `reservations` トピックに相乗りする。専用トピックは作らない**
  （容量バッジと同じ判断。`lib/events.ts` の `queryGroups` の reservations が既に
  `/api/reservations` を接頭辞に持つため、予約が変わればこの表示も自動で
  invalidate される）
- **値札（選択状態）と視聴中の画面の両方に同じ
  `LiveInterruptionWarning`（`components/live-interruption-warning.tsx`）を出す。**
  `pages/live.tsx` ではチャンネル名・種別バッジ・番組表への導線と同じ情報欄
  （`isPlaying` の分岐の外）に置くことで、1 箇所の実装で両方の受け入れ条件を
  満たしている
- **「いま」を更新する tick（`nowPlayingRefetchMs`。30 秒）を跨いでも警告は
  消えない。** 判定に使う値（`reservations` と視聴対象の `channelType`）は tick
  で変わらないクエリ（`GET /api/reservations` は SSE の `reservations` トピックで
  invalidate されるだけ）にしか依存しない。以前の EPG 第 2 クエリ経由の判定は
  `nowMs` を含む時間窓をクエリキーに持っていたため、tick のたびにキーが割れて
  react-query が新しいキャッシュエントリとして扱い、取得完了までの間**表示中の
  警告が一時的に消えていた**（実測: jsdom で 30038ms 後・実 Chromium で 28258ms
  後に消失。レビューでの指摘）。直接比較に変えたことでこの経路自体が無くなった

判定手段: `lib/live-interruption.test.ts`（一致するとき返す / skip・別チャンネル
種別・別サイトでは返さない、の両方向）、`pages/live.test.tsx`
「録画予約による中断予測」（選択状態・視聴中画面の両方に出る / skip・別チャンネル
種別では出ない、の end-to-end wiring。30 秒の tick を実時間で跨いでも警告が
消え続けないこともポーリングで確認）、
`components/live-interruption-warning.test.tsx`（`reservation` が null のとき
**描画そのものが無いこと**を `toBeEmptyDOMElement()` で見る --- 文言の regex
一致だけでは、指定した語を含まない別の肯定文言への変異を検出できない。
レビューでの指摘）。

**未検証・要確認の 2 点**（blocking ではないが、既知の不正確さとして書いておく）:

- **視聴中のチャンネル自体の録画予約でも警告が出る。** docs/data.md §6.5 の需要
  モデルは「同一物理チャンネルなら 1 本のチューナーに相乗りできる」ため、録画同士
  は同一チャンネルで競合しない。しかし mirakc がライブのストリーム要求と録画を
  同一チャンネルで相乗りさせるかどうかは本リポジトリ内に記述が無く、**未検証**
  （`internal/streamer/live.go` は「同じサービスを複数クライアントが見ても共有
  する」までしか言っていない）。相乗りするなら、この機能が最も頻繁に発火する
  ケース（見ているチャンネルの次の番組がルールで予約されている）が偽陽性になる
  --- 文言自体は「不足すると中断されます」という条件付きなので嘘にはならないが、
  値札としての精度は下がる
- **別チャンネル種別でも同じチューナーを取り合う構成（BS/CS 兼用チューナー等）
  では沈黙する。** 判定は `channelType` の一致だけで引いており、
  `tuner_sync.types`（サーバー側の Hall 条件はこちらで兼用を扱う。
  docs/data/capacity.md）までは見ていない。仕様どおり種別一致で実装しており、
  沈黙側に倒れているので下界主義には反しないが、既知の盲点として上記
  「下界主義」の項の見えない消費者の一覧に挙げてある

### チューナー状態の行

チャンネル一覧（`components/tuner-status.tsx`。`pages/live.tsx` のチャンネル一覧の
脇）に「チューナー n 本（故障 m）」の行を出す。判断の理由は
docs/operations/monitoring.md §監視「チューナー故障はライブ画面の 1 行で見せる」に
まとめてある。要点は 2 つ --- 故障（`tuner_sync.is_fault`）を見る手段が UI 以外に
無いこと、ライブ画面はチューナーを実際に掴む操作の直前に開く画面であること。

- **`GET /api/sites/{site}/tuners` は `tuner_sync` の行をそのまま返す。** 導出はしない。
  「いまどの局を掴んでいるか」「ライブ視聴が何本か」はこの射影に無い。したがって
  このエンドポイントもそれを持たない --- watcher の観測対象を広げる別の判断になる
- **site ごとに 1 行出す。** `GET /api/sites` の各 site へ問い合わせ、各 site の
  `tuner_sync` を利用可能本数・故障本数・最古 `observedAt` まで独立に集計する。他の
  site の値と混ぜず、未取得または空の site は何も主張しない。複数 site のときだけ
  行頭に site 名を表示し、1 site のときは従来どおり site 名を表示しない。
- **n は射影の全行数ではなく `isAvailable && !isFault` の本数にする**
  （`internal/capacity` の `countable` と揃える。docs/data/capacity.md §6.5）。
  射影の生本数のままだと、設定で無効化した本数まで「使える本数」に見えてしまい、
  この行が消したかった「警告が無い = 大丈夫」の誤読が n 自体で復活する。
  故障の本数は n に含めない別枠の警告として添える
- **故障 0 本のときは「（故障 m）」を出さない。** 故障は `destructive` の淡い地 +
  文字（docs/frontend/design.md「色は信号のみ」）。利用可であることに色は使わない
  --- 緑を持たない
- **射影が 0 行のサイトは何も主張しない。** 応答が空配列なら行自体を描かない
  （docs/data/capacity.md §6.5 と同じ規律）
- **鮮度は `observedAt`（射影内で最も古いもの）で見る。** 専用のしきい値は作らず、
  `isObservationStale`（`lib/storage-forecast.ts`）の既定しきい値
  （`observationStaleAfterMs`。1 時間）をそのまま再利用する。`tuner_sync` は
  worker の定期全量同期でしか値が変わらない使い捨てプロジェクションである。
  ストレージ観測（`GET /api/storage`）と性質が同じなので、同じしきい値を使う。
  独自の数字は発明しない
- **クライアント側の取り直しは `lib/events.ts` の専用グループ（`tuners`）に
  登録してある。** `storage` と同じ「トピックを持たず専用の周期だけで収束させる」
  形である。周期も storage と同じ値（`storageRefreshIntervalMs`）を流用する。
  クエリキーは URL ではなく手書きにする。`/api/sites/{site}/tuners` のままだと
  `epg` グループの接頭辞（`/api/sites/`）にも一致するため。周期タイマーは周期の
  値ごとに別の `setInterval` に分かれているので、周期の違う 2 グループに同じキーが
  入ると、両方が発火する時刻（5 分と 10 分なら 10 分ごと）に同じクエリが 2 回
  取り直される。接頭辞はグループ定義と同じ定数を参照させ、片方だけ改名して
  取り直しが止まる drift を防ぐ
- **mirakc は `isFault` を実装しておらず、常に false を返す**（`isAvailable` も
  常に true。根拠は docs/data/capacity.md §6.5）。つまり「（故障 m）」は現行の
  mirakc では出ない。それでも読む形にしてあるのは、Mirakurun 互換 API の
  フィールドであり mirakc が将来実装したら効くため。**この表示の実用上の値は
  鮮度表示のほうにある** --- そちらは射影ループの停止を実際に捉える

判定手段: `pages/live.test.tsx`「チューナー状態」。故障ありで destructive の淡い地 +
文字が見えること・無ければ出ないことを両方向で確認する。3 本中 1 本故障という
非対称な内訳にしてあるのは、`isFault` の判定を反転させる変異でも表示件数が対称に
入れ替わらず実際に落ちるようにするためである。設定で無効化した行が n から除かれる
ことと、複数 site の本数・故障・鮮度が別行で混ざらないことも確認する。`observedAt` が古いときに「観測が
止まっています」が出ることと、新しいときは出ないことも両方向で確認する。
`lib/events.ts` の `tuners` グループは `events.test.tsx` が確認する。専用の周期で
取り直すこと、SSE トピックを持たないこと、5 分と 10 分のタイマーが同時に発火する
時刻でも余分に取り直さないことの 3 つである。

### 遅延・バッファの計器

denpa の遅延・バッファ表示に相当するものを持つ。中断予測が
「見始める前の判断材料」なら、こちらは「いま見ている映像が電波とどれだけ
離れているか」という**視聴中の**計器で、ON AIR バッジ・録画中バッジと同じ
「いま電波に乗っているものとの距離」を言う表示という位置づけは共通する。
値の取得は `LivePlayer`（`components/live-player.tsx`）が担うが、表示自体は
ON AIR バッジと同じ情報欄（`pages/live.tsx`）に置く --- `LivePlayer` は
`onDiagnostics` コールバック prop で値を親へ渡すだけで、自分では描画しない。

**経路によって出せるものが違う。**

- hls.js 経路（Chrome / Firefox 等）: 1 秒ごとに `hls.latency`（放送から）と
  `hls.mainForwardBufferInfo.len`（先読み）を読む
- ネイティブ HLS 経路（Safari）: hls.js を経由しないため `latency` に相当する
  値を取得できない。「先読み」だけを `video.buffered` の末尾 - `currentTime`
  で近似し、「放送から」は**表示自体を出さない**（測れないものを出さない ---
  欠損表示（`—`）で埋めることすらしない）

**`hls.latency` は同期点が決まる前も `NaN` ではなく `0` を返す。**
`node_modules/hls.js`（1.6.17）の `LatencyController.get latency()` は
`this._latency || 0` を実装しており、`_latency` は同期点が決まるまで `null`
のまま --- つまり `NaN` を前提にすると実ブラウザでは「放送から約0秒」という
偽の測定値が出続ける。読む側
（`readHlsDiagnostics`。`components/live-player.tsx`）は `0` 以下を欠損として
弾く。表示は最初「放送から— / 先読み—」で始め、値が確定してから数値に変わる。

**「測り直す」ボタンは無い。** 値は 1 秒ごとのポーリングで常に最新へ更新
されるため、手動の再計測に操作としての意味がない（denpa の「測り直す」は
WHEP 側の再ネゴシエーションの都合であり、hls.js のポーリングにはそれに
対応する操作が無い）。**`aria-live` も付けない** --- 毎秒変わる数字を
支援技術に読み上げさせる理由が無い。

**hls.js の fatal エラーで `hls.destroy()` した後は計器のポーリングを止める。**
実 hls.js は `destroy()` 後に `latency` / `mainForwardBufferInfo` を読んでも
例外は投げない（`LatencyController.destroy()` は内部の `hls` 参照を `null`
にするだけで `_latency` は直前値のまま残る）。それでも止めるのは、意味の
無くなった値を毎秒読み続けない衛生のためであって例外対策ではない。

判定手段: `lib/live.test.ts`（欠損値・`NaN` の丸め・経路ごとの表示差の純関数
テスト）、`live-player.test.tsx`「計器」（`onDiagnostics` が 1 秒ごとに実際に
読み直した値で呼ばれること・ネイティブ経路では `latencySec` が常に `null` で
あること・`hls.latency` が `0` のままでも欠損として扱うこと・fatal エラー後に
ポーリングを止めることをフェイクの hls.js で検証）、`pages/live.tsx` 側の
表示配線テスト、`web/e2e/live.mjs` ③（実 Chrome + 実 hls.js で「放送から約
n 秒 / 先読み n 秒」が実際に 0 でない数値になることを確認 --- bundled
Chromium は H.264/AAC の実デコードが進まず `hls.latency` が更新されないため、
ここでしか測れない）。

### 実機確認について

**実機確認の手段・判定項目・回帰の記録は [runbook.md](../runbook.md)（ライブ視聴の
節）と `web/e2e/`（`web/e2e/README.md`）を見る。** ここには設計に跳ね返る 1 点だけ
残す。

- **iOS 実機（iPhone Safari）は誰も確認していない。** 判定は macOS の 4 エンジン
  で実測して固定したが、iOS の `canPlayType('video/mp2t')` が macOS の WebKit と
  同じ `'maybe'` を返す保証は無い。違っていた場合、iOS は hls.js 経路へ落ちる
  （iOS 17.1 以降は ManagedMediaSource で再生できる。それ未満は上記 3 段目の
  最後の砦で `<video>` に直接渡る）。この判定が正しいかは
  [runbook.md](../runbook.md)（ライブ視聴の節）の実機確認でしか確定しない
