> [live.md](../live.md) のチューナー状態。索引から辿る

### チューナー状態の行

チャンネル一覧の脇に、site ごとに「チューナー n 本（故障 m）」の行を出す。n は故障を含めない使える本数で、観測が古いときは止まっていると出す。故障を見る手段が UI 以外に無く、ライブ画面はチューナーを取る操作の直前に開く画面だからである。

判断の理由は docs/operations/monitoring.md §監視「チューナー故障はライブ画面の 1 行で見せる」にまとめてある。

<details><summary>根拠</summary>

- 表示の場所: `components/tuner-status.tsx`、`pages/live.tsx` のチャンネル一覧
- 故障を見る手段が UI 以外に無いこと: `tuner_sync.is_fault`

</details>

#### チューナー一覧 API が返すもの

チューナー一覧の API は、チューナー射影の行をそのまま返す。導出はしない。「いまどの局を取っているか」と「ライブ視聴が何本か」は、この射影に無い。したがってこの API もそれを持たない。持たせるなら、watcher の観測対象を広げる別の判断になる。

対象の API は `GET /api/sites/{site}/tuners` である。

<details><summary>根拠</summary>

- そのまま返す射影: `tuner_sync`

</details>

#### site ごとの行

行は site ごとに 1 つ出す。サイトの一覧 API の各 site へ問い合わせ、各 site のチューナー射影を、利用可能本数・故障本数・最古の観測時刻まで独立に集計する。他の site の値と混ぜず、未取得または空の site は何も主張しない。複数 site のときだけ行頭に site 名を表示し、1 site のときは従来どおり site 名を表示しない。

射影が 0 行の site は何も主張しない。応答が空配列なら行自体を描かない（docs/data/capacity.md §6.5 と同じ規律）。

<details><summary>根拠</summary>

- サイトの一覧 API: `GET /api/sites`
- 集計する値: `tuner_sync`、`observedAt`
- site ごとの集計の単位: `site`

</details>

#### 本数の数え方と故障の見せ方

n は、利用可能で故障していない本数にする。故障の本数は別枠の警告として付け、0 本のときは出さない。利用可であることには、色を使わない。

射影の全行数を n にする案は採らない。設定で無効化した本数まで「使える本数」に見えてしまい、この行が消したかった「警告が無い = 大丈夫」の誤読が n 自体で復活するためである。数え方は容量計算と揃える（docs/data/capacity.md §6.5）。

故障は destructive の淡い地 + 文字で見せる（docs/frontend/design.md「色は信号のみ」）。緑は持たない。

<details><summary>根拠</summary>

- n の数え方: `isAvailable && !isFault`、`internal/capacity` の `countable`
- 故障の色: `destructive`

</details>

#### 鮮度の見せ方

鮮度は、射影内で最も古い観測時刻で見る。専用のしきい値は作らず、ストレージ観測の既定のしきい値をそのまま再利用する。独自の数字は発明しない。

チューナー射影は、worker の定期全量同期でしか値が変わらない使い捨てプロジェクションである。ストレージ観測（`GET /api/storage`）と性質が同じなので、同じしきい値を使う。

<details><summary>根拠</summary>

- 最も古い観測時刻: `observedAt`
- 既定のしきい値の再利用: `isObservationStale`（`lib/storage-forecast.ts`）、`observationStaleAfterMs`
- 定期全量同期でしか変わらない射影: `tuner_sync`

</details>

#### 取り直しの仕組み

クライアント側の取り直しは、専用の周期だけで収束させ、SSE のトピックは持たない。これはストレージと同じ形である。クエリキーは手書きにして、周期の違うグループに同じキーが入って二重に取り直されるのを防ぐ。

周期の値も、ストレージと同じものを流用する。

URL そのままのクエリキーは採らず、手書きにする。`/api/sites/{site}/tuners` のままだと、epg グループの接頭辞（`/api/sites/`）にも一致するためである。周期タイマーは周期の値ごとに別の `setInterval` に分かれている。周期の違う 2 グループに同じキーが入ると、両方が発火する時刻（5 分と 10 分なら 10 分ごと）に、同じクエリが 2 回取り直される。接頭辞はグループ定義と同じ定数を参照させ、片方だけ改名して取り直しが止まるずれを防ぐ。

<details><summary>根拠</summary>

- 専用のグループ: `lib/events.ts` の `tuners`、`storage`
- 周期の流用: `storageRefreshIntervalMs`
- 周期ごとのタイマー: `setInterval`
- 接頭辞が一致するグループ: `epg`

</details>

#### mirakc の故障フィールド

mirakc は故障のフィールドを実装しておらず、常に故障なしを返す。利用可能のフィールドも常に真である。そのため「（故障 m）」は、現行の mirakc では出ない。それでも読む形にしてあるのは、Mirakurun 互換 API のフィールドであり、mirakc が将来実装したら機能するためである。

**この表示の実用上の値は、鮮度表示のほうにある。** そちらは射影ループの停止を実際に捉える。

<details><summary>根拠</summary>

- 常に返す値（根拠は docs/data/capacity.md §6.5）: `isFault` は常に false、`isAvailable` は常に true

</details>

#### 判定

判定は、故障の有無・設定・site ごとの分離・鮮度を両方向で確認する。故障は 3 本中 1 本という非対称な内訳にして、判定を反転させる変異で実際に落ちるようにする。

故障ありでは、destructive の淡い地 + 文字が見えること、故障が無ければ出ないことを確かめる。内訳が非対称なのは、故障の判定を反転させる変異でも表示件数が対称に入れ替わらず、実際に落ちるようにするためである。設定で無効化した行は件数から除き、複数 site の本数・故障・鮮度は別行に保つ。

観測が古いときは「観測が止まっています」を出し、新しいときは出さない。チューナーイベントは専用周期で取り直し、SSE トピックを持たない。5 分と 10 分のタイマーが同時に発火する時刻でも、重複取得しない。

<details><summary>根拠</summary>

- 故障表示・無効化・site 別表示・鮮度表示の両方向: `pages/live.test.tsx`「チューナー状態」
- 専用周期・SSE トピック無し・周期の重複防止: `lib/events.ts` の `tuners` グループ、`events.test.tsx`
- 反転させる変異の対象: `isFault`
- 故障の色: `destructive`

</details>
