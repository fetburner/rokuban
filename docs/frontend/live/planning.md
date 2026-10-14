> [live.md](../live.md) の放送予定と予約による中断予測。索引から辿る

### 「いま放送中」は予定であって観測ではない

「いま放送中」は、既存の EPG API に時間窓を投げて出している。これは予測であって観測ではないので、画面でも番組名を断言せず、予定であることが読み取れる形にする。放送波を正とした「今の番組」は持たない。

<details><summary>根拠</summary>

- 時間窓を投げる実装: `lib/live.ts` の `currentProgramWindow`

</details>

#### 放送波を正とした今の番組を持たない理由

真実は EIT[p/f] にしかなく、読むには多重化波ごとに恒久専有チューナーを払う（[../recording/delegation.md](../../recording/delegation.md)「onair tracker は採らない」）。ここで番組名は装飾で、外れてもユーザーは映っているものを見ている。掛け金が合わない。

番組名が外れて実害が出るのは録画側で、そこは終了だけ mirakc が追従している（同「PSI/SI 追従」）。

#### 外れた窓は Rokuban 側では縮まない

予定の鮮度は、mirakc の EIT[schedule] の更新間隔で決まる。Rokuban 側の EPG 取得頻度を上げても、外れの上界は動かない。縮めたいなら mirakc の設定を触る。

番組表は mirakc の EIT[schedule] の射影である。更新間隔の既定は 1 日 2 回なので、最大 12 時間前のスナップショットになる。放送局が繰り下げを EIT[schedule] に出しても、その窓の中では拾えない。

<details><summary>根拠</summary>

- mirakc の EIT[schedule] の射影: `epg_programs`
- 更新間隔の設定: `jobs.update-schedules`

</details>

#### 観測を持つようになったとき

観測を持つようになったら、予定と同じ列・同じフィールドに載せない。予定は毎パス作り直せる導出値で、観測は「誰が・いつ観測したか」が付いて回る事実であり、寿命が違うためである。

置き場所は、site とサービスを主キーにした揮発表である。番組表にも録画にも列は足さない。書くのは worker で、api は DB だけを読む。mirakc 由来の番組識別子は保存しない。

<details><summary>根拠</summary>

- 導出値と事実の寿命の違い: 不変条件 9 / 12
- 揮発表の主キー: `(site, service_id)`
- 列を足さない表: `epg_programs`、`recordings`
- api は DB だけを読む: 不変条件 1
- mirakc 由来の識別子を保存しない: `program_id`、不変条件 7

</details>

### 録画予約による中断予測

視聴中に同じチャンネル種別の録画予約が始まると、チューナーを取られて視聴が中断されうる。予約と視聴対象のチャンネル種別を直接比べ、同じ種別の予約が 2 時間以内に始まるときだけ、条件付きの文言で警告する。予約が無いことは、安全の保証としては出さない。

mirakc の優先度調停では録画が勝つ。Rokuban は予約（desired state）を持っているので、視聴開始前に「この後中断されうるか」を知らせられる。

#### 判定の材料と比べ方

判定は、予約一覧と視聴対象のチャンネル種別を直接比べる純関数で行う。予約は、site が視聴対象の site と一致するものだけを見る（docs/schema.md §1 の設計原則）。

材料は `GET /api/reservations`（全サイト分。絞り込みパラメータを持たない）が返す `Reservation.channelType` である。これは番組スナップショット由来の値で、視聴対象の `Service.channelType` と直接比較する。

以前は `Reservation` がチャンネル種別を持たなかった。そのため、視聴対象と同じ種別のサービスに絞って EPG（`GET /api/sites/{site}/programs`）から programId を引き、`(site, programId)` で突き合わせる第 2 クエリが要った。`Reservation.channelType` が追加されたことで、この EPG 側の join 自体が不要になった。

<details><summary>根拠</summary>

- 判定の純関数: `lib/live-interruption.ts` の `upcomingInterruptingReservation`
- チャンネル種別の由来: `program_snapshots`
- 予約の絞り込み: `site`

</details>

#### 先読みの時間窓

先読みの時間窓は 2 時間である。視聴を選ぶ、または始めるときの判断材料として出す表示なので、窓は「これから見始める 1 回の視聴」がカバーする範囲に合わせる。

1 番組（30 分〜1 時間）を見ている間に、次の番組の録画が競合し得ることまでは見せたい。24 時間先の録画予約まで警告すると、「今まさに見るかどうかの判断」には関係の薄い予約まで出てノイズになる。

#### skip の予約は除外する

skip の予約は除外する。サーバーの需要計算と同じ規則である。実効の skip が真の予約は、reconciler が mirakc に同期しないので、チューナーを消費しない。

サーバーの容量計算でも、実効の skip が真の行は需要から除外される（docs/data.md §6.5）。API が返す `Reservation.skip` はまさにこの実効の skip なので、フロント側もこの値で同じ除外を適用する。

<details><summary>根拠</summary>

- skip の除外: `effective.skip`
- サーバー側の除外: `internal/capacity/load.go` の `demandFromRow`、`eff.IsSkipped()`

</details>

#### 下界主義と見えない消費者

「中断されます」と断言しない。チューナーに余裕があれば中断されないが、余裕があるとも言えないためである。種別の一致だけを見るので、別種別でも同じチューナーを取り合う構成では警告が出ない。

下界主義は、容量バッジと同じ規律である（docs/data.md §6.5）。

見えない消費者は、並走 EPGStation・他のライブ視聴セッション・mirakc の `excluded_channels` である。

別種別でも同じチューナーを取り合う構成には、BS/CS 兼用チューナー等がある。サーバー側の Hall 条件は兼用を扱うが、フロントはそこまで見ていない。

警告を出さない側の誤りなので下界主義には反しないが、既知の盲点である。

#### 文言と、安全を保証しないこと

文言は「HH:MM から録画予約があります。チューナーが不足すると視聴は中断されます」という条件付きにする。「録画予約はありません = 安全に見られます」は出さない。沈黙は保証ではない。

肯定的な文言は一切持たない。容量不足のバッジや番組重複の警告と同じ「余計な枠を出さない」流儀である。

<details><summary>根拠</summary>

- 同じ流儀の表示: `CapacityShortfallBadge`、`ProgramOverlapWarning`

</details>

#### 鮮度と表示場所

鮮度は SSE の予約のトピックに相乗りし、専用トピックは作らない（容量バッジと同じ判断）。表示は、選択状態と視聴中の画面の両方に、同じ警告部品で出す。

予約のクエリグループは既に `/api/reservations` を接頭辞に持つので、予約が変わればこの表示も自動で invalidate される。予約一覧は `/api/reservations` から読む。

警告部品は、チャンネル名・種別バッジ・番組表への導線と同じ情報欄に置く。再生中かどうかの分岐の外なので、1 箇所の実装で選択中と視聴中の両方の受け入れ条件を満たしている。

<details><summary>根拠</summary>

- 予約のトピックに相乗りする: SSE `reservations`、`lib/events.ts` の `queryGroups`
- 警告部品と置き場所: `LiveInterruptionWarning`（`components/live-interruption-warning.tsx`）、`pages/live.tsx`、`isPlaying` の分岐の外
- 変更時の invalidate: SSE の `reservations` トピック

</details>

#### tick をまたいでも警告を保つ

30 秒の tick や番組境界の更新をまたいでも、警告は保つ。判定に使うのは、現在時刻で変化しない予約一覧と視聴対象のチャンネル種別だからである。

EPG の時間窓クエリで判定すると、現在時刻が tick ごとに変わってクエリキーが割れ、react-query が新しいキャッシュエントリとして扱う。取得完了までの間、表示中の警告が一時的に消えていた（実測: jsdom で 30,038 ms 後、実 Chromium で 28,258 ms 後）。予約との直接比較に変えて、この経路をなくした。

<details><summary>根拠</summary>

- tick の周期: `pages/live.tsx` の `scheduleClockTickMs`
- キーが割れる原因（現在時刻の値）: `nowMs`

</details>

#### 判定の確認手段

判定は純関数・画面配線・警告表示の 3 層で確認する。予約が条件に一致するときは警告を出し、skip・別チャンネル種別・別 site では出さない。

選択中と視聴中の両方に警告を表示する。予約が null のときは何も描画せず、30 秒の tick を実時間でまたいでも表示が消えないことを確認する。文言の regex 一致だけでは、指定語を含まない肯定的な文への変異を検出できない。空状態の DOM 自体を確認する。

<details><summary>根拠</summary>

- 純関数: `lib/live-interruption.test.ts`
- 画面の両方向と tick: `pages/live.test.tsx`「録画予約による中断予測」
- null のときの空表示: `components/live-interruption-warning.test.tsx`、`reservation`、`toBeEmptyDOMElement()`

</details>

#### 未検証・要確認の 2 点

既知の不正確さが 2 つある。視聴中のチャンネル自体の録画予約でも警告が出るが、mirakc が相乗りするかは未検証である。別種別でも同じチューナーを取り合う構成では、警告が出ない。

どちらも blocking ではないが、既知の不正確さとして書いておく。

1 つ目は、視聴中のチャンネル自体の録画予約でも警告が出ることである。docs/data.md §6.5 の需要モデルは「同一物理チャンネルなら 1 本のチューナーに相乗りできる」ので、録画同士は同一チャンネルで競合しない。しかし mirakc がライブのストリーム要求と録画を同一チャンネルで相乗りさせるかどうかは、本リポジトリ内に記述が無く**未検証**である。ライブの配信部品は、「同じサービスを複数クライアントが見ても共有する」までしか言っていない。相乗りするなら、この機能が最も頻繁に発火するケース（見ているチャンネルの次の番組がルールで予約されている）が偽陽性になる。文言自体は「不足すると中断されます」という条件付きなので嘘にはならないが、判断材料としての精度は下がる。

2 つ目は、別チャンネル種別でも同じチューナーを取り合う構成（BS/CS 兼用チューナー等）で警告が出ないことである。判定は `channelType` の一致だけで引いており、`tuner_sync.types`（サーバー側の Hall 条件はこちらで兼用を扱う。docs/data/capacity.md）までは見ていない。仕様どおり種別一致で実装しており、警告を出さない側の誤りなので下界主義には反しないが、既知の盲点である。上の下界主義の項の、見えない消費者の一覧に挙げてある。

<details><summary>根拠</summary>

- 同一サービスの共有範囲: `internal/streamer/live.go` の `LiveStreamer`
- 容量の下界と未観測の負荷: [docs/data.md](../../data.md) §6.5

</details>
