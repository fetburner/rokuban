> [live.md](../live.md) のチャンネル選択と URL。索引から辿る

### サイトとサービスの同定・選択・リンク

**ライブ配信・セッション・離脱ヒント、URL、音声切替、チューナー状態の行は
既存のまま保つ。** 画質・音声を切り替えてもライブセッションを作り直さず、画質の
`?profile=` と音声の `?audio=`、チューナー状態の表示も維持する。今回変えるのは
ライブの操作面と予定番組の見せ方であり、配信の資源同定やチューナー表示の意味を
変える理由は無い。

選択中のチャンネルは `?service=<Service.id>` に持つ。不正な値は `undefined` を**明示代入**して落とす。省略では
消えない（[recordings.md](../recordings.md)「TanStack Router の `validateSearch` は
無効な値を『省略』しても消えない」）。
チャンネル一覧のリンクは `replace` にし、ザッピングでブラウザ履歴が積み上がらない
ようにする。

desktop ではプレイヤーとチャンネル一覧を隣り合わせにし、映像の高さを局名・番組名・
時刻の 3 行が初期 viewport に入る上限に合わせる。上限に当たった分の横幅はチャンネル
一覧に回し、一覧を多段にする。高さだけを制限して動画を横に引き伸ばさない。
`lg` 以上ではプレイヤー列を sticky にして、長い一覧のスクロール中も映像と番組情報を
残す。sticky の `top` は居座りバナーと `PageHeader` の実測高を足し、ページのスクロール
箱は 1 つに保つ。phone の縦積みは変えない。1180px 前後の配置は既存のラフを保ち、
高さ上限が効く広い画面だけでプレイヤー幅を抑える。

**site も含めてチャンネルを同定する。** SI の `serviceId` 単独では network をまたぐと一意でない。
Mirakurun は `networkId * 100000 + serviceId` の合成 id を使う。GR / BS / CS を混ぜる一覧では同じ `serviceId` が重なることがあるため、API は合成値を `Service.id` として返す。

- 選択中ハイライト・`aria-current`・再生中チャンネルの記憶は site と `Service.id` の組で行う。
  `/live` の URL は `?service=<Service.id>&site=<site>` とする。`service` の値域は `/programs` と同じ生成スキーマで検証する。
- 初期選択は、site と `Service.id` が一致すればそれを使い、無ければ番組を持つ先頭にする。
- 番組リストの放送中行には、予約ボタンの左に 44px の `Play` アイコンボタンを置く。行に対する動作なので、展開領域のテキストリンクにはしない。
- 対応する録画中録画が無いときは「ライブで見る」で `/live?service=<Service.id>&site=<site>` へ進む。
  録画があるときだけ「追っかけ再生」で `/recordings/{recordingId}#chase` へ進む。

**番組表と録画の絞り込みも network を含む厳密形式を持つ。** 高松の地上波だけを
受信する実運用 mirakc では 19 サービス中の重複は 0 件だったが、この測定は GR の
範囲しか覆わない。公式割当には BS `(network_id=4, service_id=101)` と 110 度 CS
`(network_id=6, service_id=101)` の実例がある。そのため GR / BS / CS を混ぜる
一般の構成では、`serviceId` 単独を identity にできない。

「この局の番組表」も番組表ピッカーの複数選択も録画の絞り込みも、同じ
`?service=<Service.id>` の配列で運ぶ（1 局なら 1 要素）。ライブでは site も
`?site=<site>` で運ぶ。録画では site が
別軸（`?site=`）で、軸内は OR・軸間は AND。

**「選ぶ」（`?service=` を変える）と「流す」（`LivePlayer` をマウントする）を別のタップに分ける。**
選択画面から再生できるが、二段階の同意は維持する。
`?service=` の変更だけでは probe もセッション（チューナー確保 + ffmpeg 起動）も起こさず、チャンネル一覧・現在番組・チャンネル種別の表示だけを変える。

- `LivePlayer` は「再生」を押すまでマウントしない。確認ダイアログは使わず、選択状態の画面そのものを値札として、再生は 1 タップにする。
- 「チューナーが空いています」などの肯定・保証は書かない。mirakc には Rokuban から見えない消費者がいるため、空きの有無を断言できない。

操作の摩擦は明示的な再生のコストに合わせ、確認ダイアログでタップを重ねない。チューナーが有限でない環境にまで余計な操作を求めない。

`playingKey` と `selectedKey` の一致判定はレンダー中に行う（effect ではない）。
直リンクで来た場合も、チャンネル一覧で切り替えた場合も、選択ごとに再生同意を求める。
- 一度再生した後に B へ切り替え、A に戻っても、自動再開はしない。再度「再生」を押す。
- `useEffect` で「選択が変わったら false に戻す」と、1 コミットだけ古い再生中フラグが残る。
  子の `LivePlayer` が親の `LivePage` より先に effect を実行し、B の `<LivePlayer serviceId={B}>` が 1 件の playlist.m3u8 を要求する。jsdom と実ブラウザの両方で確認済みである。
- `LivePlayer` を先に mount するとサーバー側のセッションは残る。`AbortController.abort()` だけではセッションを止めない。
- 離脱ヒントも期限を縮めるだけで 0 にはしない。押していないチャンネルを掴む時間は猶予（既定 8 秒）+ GC 周期となる。
- レンダー中に一致を判定すれば、`selectedKey` が変わった時点で再生中でないと確定し、別 serviceId の中間 mount を作らない。

**直リンク・ブックマーク（`/live?service=<Service.id>` の直開き）も選択状態で止まる**。
再生開始の同意を取る構造は、通常のチャンネル一覧からの選択と直リンクで区別しない
--- 直開きだけ自動再生にすると「タップで選んだときは同意が要るが URL 経由なら
要らない」という一貫しない規則になる。その規則では、番組行の「ライブ」ボタン等の
外部導線（`components/program-row.tsx`）から来た場合もチューナーを暗黙に掴んでしまう。

**チャンネル切り替えのデバウンスは持たない**。選択自体が probe もセッションも
起こさないので、デバウンスする対象（= 選択の瞬間にコストのかかる処理）が
そもそも発生しない --- 何も守らないものは置かない。チャンネル一覧のリンクはクリックで即座に
ナビゲートする。

**視聴中チャンネルの情報欄に「この局の番組表」リンクを置く。**
`/programs`（番組表）の `?service=` へ、視聴中の 1 局を配列 1 要素で渡す
（[番組表](../programs.md)「番組リスト」の絞り込みと同じ形。`service?: number[]`）。
このリンクは通常の push ナビゲーション（`replace` にしない） --- チャンネル一覧の
ザッピングとは違い 1 回だけの遷移なので、戻るボタンで視聴中チャンネルへ戻れる
方が自然。逆方向（番組表 → ライブ）はページ単位の導線を置かない（理由は
[番組表](../programs.md)「番組リスト」の該当箇条書き）。

**ライブへの導線はサーバーの `live.enabled` に連動させる。**
`live.enabled: false` のとき streamer はライブのルートを一切登録しないので、
導線を出しても行き先が無い。判断は `GET /api/capabilities` の `live` に一本化し、
フロント側の入口を一つにする（ナビの出し分けは
[shell.md](../shell.md)「無効な機能の項目は出さない」）。`/live` のルート自体は
残す。**直リンク・ブックマークで来たときは「この環境ではライブ視聴が無効です」+
`live.enabled` という手がかりを出す** --- ルートを消すと SPA の 404 になるだけで、
運用者は原因（サーバー設定）に辿り着けない。無効のときはプレイリストを一度も
取りに行かない。

**probe（`response.ok`）だけでは「無効」を検出できない**。`/api/` 配下を SPA
フォールバックに落とすと、ライブのルートが無いパスは index.html を 200 で返す。
そのため probe は通ってしまう --- その後 hls.js / `<video>` が m3u8 として
解釈できずに再生エラーになる。その結果「無効」ではなく「壊れている」と読める。だから `/api/` 配下は
JSON 404 にしてある。**能力 API
（導線を消す）とこの 404 化は両方要る。どちらか一方では足りない。**

**「無効」と断言するのは `live: false` を実際に受け取ったときだけにする**。
この画面は原因（サーバー設定）を名指しするので、能力 API の状態を次のように分ける。

| 能力 API の状態 | 表示 |
|---|---|
| `false` | 「この環境ではライブ視聴が無効です」+ `live.enabled` の手がかり |
| `pending` | 読み込み中 |
| `unknown`（取得失敗） | 「利用できるかを確認できませんでした」 |
| `true` | ライブ視聴を有効として扱う |

導線側は未確定を無効に倒してよい。画面と違って原因を断言せず、黙って消えるだけだからである。状態を一つに潰すと
**`live.enabled: true` のデプロイで能力 API が瞬断しただけでも「設定が無効」と
表示され**、この画面が消したかった「原因にたどり着けない」を別の顔で再演する。
潰した実装では画面のテスト 2 件が落ちることを確認済みである（[shell.md](../shell.md)）。

**「有効」と「今すぐ見られる」は別**である。`live: true` は config の状態だけを
表し、streamer が動いていない / チューナーが埋まっている場合は導線が出たまま
プレイリスト取得の 404 / 503 として[playback.md](playback.md)のエラー分類に出る。

<details><summary>根拠</summary>

- 不正な service 値を明示的に落とす: `routes.tsx` の `validateSearch`、`undefined` を明示代入
- 外部導線のリンクと識別子: `components/program-row.tsx`、`ProgramListItem`、`composeServiceId`、`lib/service-id.ts`
- チャンネル変更に debounce を置かない: `pages/live.tsx`、`channelSwitchDebounceMs`
- 合成 id と site を使う初期選択: `GET /api/sites/{site}/services`、`networkId`、`serviceId`、`Service.id`、`lib/live.ts`
- 初期選択関数と引数: `pickInitialService(services, requestedId, requestedSite)`
- 再生同意は選択ごとに必要: `pages/live.tsx` の `playingKey` / `selectedKey`、`selection.md` の選択判定
- 先行 mount で残るサーバーセッション: `internal/streamer/session.go`、`context.WithCancel(context.Background())`、`AbortController.abort()`
- 能力の取得元と導線: `lib/capabilities.ts`、`GET /api/capabilities`、`live.enabled`
- 未登録ルートの JSON 404: `internal/api/spa.go`、`spaOrAPINotFound`
- capability 状態の分岐: `useLiveEnabled()`、`useLiveCapability()`、`pending`、`unknown`
- 状態を混同したときの回帰: `pages/live.test.tsx` の 2 件
- 誤った playlist 要求を防ぐ: `playlist.m3u8`

</details>
