> [live.md](../live.md) の放送予定と予約による中断予測。索引から辿る

### 「いま放送中」は予定であって観測ではない

「いま放送中」は既存の EPG API に時間窓を投げて出している（`lib/live.ts` の `currentProgramWindow`）。**これは予測であって観測ではない**ので、画面でもそう見せる --- 番組名を断言せず、予定であることが読み取れる形にする。

**放送波を正とした「今の番組」は持たない。** 真実は EIT[p/f] にしかなく、読むには多重化波ごとに恒久専有チューナーを払う（[../recording/delegation.md](../../recording/delegation.md)「onair tracker は採らない」）。ここで番組名は装飾で、外れてもユーザーは映っているものを見ている --- 掛け金が合わない。番組名が外れて実害が出るのは録画側で、そこは終了だけ mirakc が追従している（同「PSI/SI 追従」）。

**嘘の窓は Rokuban 側では縮まない。** `epg_programs` は mirakc の EIT[schedule] の射影である。その鮮度は mirakc の `jobs.update-schedules` の間隔で決まる（既定は 1 日 2 回なので、最大 12 時間前のスナップショット）。放送局が繰り下げを EIT[schedule] に出しても、その窓の中では拾えない。Rokuban 側の EPG 取得頻度を上げても上界は動かないので、縮めたいなら mirakc の設定を触る。

**観測を持つようになったら、予定と同じ列・同じフィールドに載せない。** 予定は毎パス作り直せる導出値、観測は「誰が・いつ観測したか」が付いて回る事実で、寿命が違う（不変条件 9 / 12）。置き場所は `(site, service_id)` を主キーにした揮発表で、`epg_programs` にも `recordings` にも列は足さない。書くのは worker で、api は DB だけを読む（不変条件 1）。mirakc 由来の `program_id` は保存しない（不変条件 7）。

### 録画予約による中断予測

mirakc の優先度調停では録画が勝つため、視聴中に同じチャンネル種別の録画予約が始まるとチューナーを取られて視聴側が中断されうる。Rokuban は予約（desired state）を持っているので、視聴開始前に「この後中断されうるか」を知らせられる。

**判定は純関数 `lib/live-interruption.ts` の `upcomingInterruptingReservation` である**。材料は `GET /api/reservations`（全サイト分。絞り込みパラメータを持たない）が返す `Reservation.channelType`（program_snapshots 由来のスナップショット）である。これを視聴対象の `Service.channelType` と直接比較する。予約は `site` が視聴対象の site と一致するものだけを見る（docs/schema.md §1 の設計原則）。

以前は `Reservation` がチャンネル種別を持たなかった。そのため、視聴対象と同じ種別のサービスに絞って EPG（`GET /api/sites/{site}/programs`）から programId を引き、 `(site, programId)` で突き合わせる第 2 クエリが要った。`Reservation.channelType` が追加されたことでこの EPG 側の join 自体が不要になった。

- **先読みの時間窓は 2 時間。** 視聴を選ぶ／始める瞬間の判断材料として出す表示なので、窓は「これから見始める 1 回の視聴」がカバーする範囲に合わせる。1 番組（30 分〜1 時間）を見ている間に次の番組の録画が競合し得ることまでは見せたいが、24 時間先の録画予約まで警告すると「今まさに見るかどうかの判断」には関係の薄い予約まで出てノイズになる
- **skip の予約は除外する（サーバーの需要計算と同じ規則）**。`effective.skip` が true の予約は reconciler が mirakc に同期しないため、チューナーを消費しない。根拠は `internal/capacity/load.go` の `demandFromRow` で、`eff.IsSkipped()` が true の行は容量の需要から除外される（docs/data.md §6.5）。API が返す `Reservation.skip` はまさにこの `effective.skip` なので、フロント側もこの値で同じ除外を適用する
- **下界主義は容量バッジと同じ規律（docs/data.md §6.5）。** 「中断されます」と断言しない。チューナーに余裕があれば中断されないが、余裕があるとも言えない。見えない消費者は次のとおり。
  - 並走 EPGStation・他のライブ視聴セッション・mirakc の `excluded_channels`
  - **チャンネル種別一致だけを見ているため、BS/CS 兼用チューナー等、別種別でも同じチューナーを取り合う構成では警告が出ない**。サーバー側の Hall 条件は `tuner_sync.types` でこの兼用を扱うが、フロントはそこまで見ていない。沈黙側に倒れているので下界主義には反しないが既知の盲点
- 文言は「HH:MM から録画予約があります。チューナーが不足すると視聴は中断されます」という条件付きにする。
- **「録画予約はありません = 安全に見られます」は出さない。** 肯定的な文言を一切持たない。沈黙は保証ではない（`CapacityShortfallBadge` / `ProgramOverlapWarning` と同じ「余計な枠を出さない」流儀）。
- **鮮度は SSE の `reservations` トピックに相乗りする。専用トピックは作らない**（容量バッジと同じ判断。`lib/events.ts` の `queryGroups` の reservations が既に `/api/reservations` を接頭辞に持つため、予約が変わればこの表示も自動で invalidate される）
- **値札（選択状態）と視聴中の画面の両方に同じ `LiveInterruptionWarning`（`components/live-interruption-warning.tsx`）を出す。**`pages/live.tsx` ではチャンネル名・種別バッジ・番組表への導線と同じ情報欄（`isPlaying` の分岐の外）に置くことで、1 箇所の実装で両方の受け入れ条件を満たしている
- **30 秒の tick や番組境界の更新を跨いでも警告を保つ。** 判定に使うのは現在時刻で変化しない予約一覧と視聴対象の channelType である。
- 予約一覧は /api/reservations から読み、変更時は SSE の reservations トピックで invalidate する。
- EPG の時間窓クエリで判定すると、nowMs が tick ごとに変わり query key が割れ、react-query が新しいキャッシュエントリとして扱う。取得完了までの間、表示中の警告が一時的に消えていた（実測: jsdom で 30,038 ms 後、実 Chromium で 28,258 ms 後）。予約との直接比較に変えてこの経路をなくした。

判定は純関数、画面配線、警告表示の 3 層で行う。予約が条件に一致するときは警告を出し、skip・別チャンネル種別・別 site では出さない。

選択中と視聴中の両方に警告を表示する。予約が null のときは何も描画せず、30 秒の tick を実時間で跨いでも表示が消えないことを確認する。

文言の regex 一致だけでは、指定語を含まない肯定的な文への変異を検出できない。空状態の DOM 自体を確認する。

**未検証・要確認の 2 点**（blocking ではないが、既知の不正確さとして書いておく）:

- **視聴中のチャンネル自体の録画予約でも警告が出る。** docs/data.md §6.5 の需要モデルは「同一物理チャンネルなら 1 本のチューナーに相乗りできる」ため、録画同士は同一チャンネルで競合しない。しかし mirakc がライブのストリーム要求と録画を同一チャンネルで相乗りさせるかどうかは、本リポジトリ内に記述が無く**未検証**である。`internal/streamer/live.go` の `LiveStreamer` は「同じサービスを複数クライアントが見ても共有する」までしか言っていない。相乗りするなら、この機能が最も頻繁に発火するケース（見ているチャンネルの次の番組がルールで予約されている）が偽陽性になる。文言自体は「不足すると中断されます」という条件付きなので嘘にはならないが、値札としての精度は下がる
- **別チャンネル種別でも同じチューナーを取り合う構成（BS/CS 兼用チューナー等）では沈黙する。** 判定は `channelType` の一致だけで引いており、`tuner_sync.types`（サーバー側の Hall 条件はこちらで兼用を扱う。docs/data/capacity.md）までは見ていない。仕様どおり種別一致で実装しており、沈黙側に倒れているので下界主義には反しないが、既知の盲点として上記「下界主義」の項の見えない消費者の一覧に挙げてある

<details><summary>根拠</summary>

- 容量の下界と未観測の負荷: [docs/data.md](../../data.md) §6.5、`CapacityShortfallBadge`、`ProgramOverlapWarning`、`excluded_channels`、`tuner_sync.types`
- skip の需要除外: `internal/capacity/load.go` の `demandFromRow`、`eff.IsSkipped()`、`Reservation.skip`
- 予約一覧の更新: `GET /api/reservations`、SSE `reservations`、`lib/events.ts` の `queryGroups`、`nowMs`
- 時計と表示場所: `pages/live.tsx` の `scheduleClockTickMs`、`LiveInterruptionWarning`、`components/live-interruption-warning.tsx`
- 純関数: `lib/live-interruption.test.ts`
- 画面の両方向と tick: `pages/live.test.tsx`「録画予約による中断予測」
- null のときの空表示: `components/live-interruption-warning.test.tsx`、`reservation`、`toBeEmptyDOMElement()`
- 同一サービスの共有範囲: `internal/streamer/live.go` の `LiveStreamer`

</details>
