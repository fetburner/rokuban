> [live.md](../live.md) のチューナー状態。索引から辿る

### チューナー状態の行

チャンネル一覧（`components/tuner-status.tsx`。`pages/live.tsx` のチャンネル一覧の脇）に「チューナー n 本（故障 m）」の行を出す。判断の理由は docs/operations/monitoring.md §監視「チューナー故障はライブ画面の 1 行で見せる」にまとめてある。要点は 2 つ --- 故障（`tuner_sync.is_fault`）を見る手段が UI 以外に無いこと、ライブ画面はチューナーを実際に掴む操作の直前に開く画面であること。

- **`GET /api/sites/{site}/tuners` は `tuner_sync` の行をそのまま返す。** 導出はしない。「いまどの局を掴んでいるか」「ライブ視聴が何本か」はこの射影に無い。したがってこのエンドポイントもそれを持たない --- watcher の観測対象を広げる別の判断になる
- **site ごとに 1 行出す。** `GET /api/sites` の各 site へ問い合わせ、各 site の `tuner_sync` を利用可能本数・故障本数・最古 `observedAt` まで独立に集計する。他の site の値と混ぜず、未取得または空の site は何も主張しない。複数 site のときだけ行頭に site 名を表示し、1 site のときは従来どおり site 名を表示しない。
- **n は射影の全行数ではなく `isAvailable && !isFault` の本数にする**（`internal/capacity` の `countable` と揃える。docs/data/capacity.md §6.5）。射影の生本数のままだと、設定で無効化した本数まで「使える本数」に見えてしまい、この行が消したかった「警告が無い = 大丈夫」の誤読が n 自体で復活する。故障の本数は n に含めない別枠の警告として添える
- **故障 0 本のときは「（故障 m）」を出さない。** 故障は `destructive` の淡い地 + 文字（docs/frontend/design.md「色は信号のみ」）。利用可であることに色は使わない --- 緑を持たない
- **射影が 0 行のサイトは何も主張しない。** 応答が空配列なら行自体を描かない（docs/data/capacity.md §6.5 と同じ規律）
- **鮮度は `observedAt`（射影内で最も古いもの）で見る**。 専用のしきい値は作らず、`isObservationStale`（`lib/storage-forecast.ts`）の既定しきい値（`observationStaleAfterMs`。1 時間）をそのまま再利用する。`tuner_sync` は worker の定期全量同期でしか値が変わらない使い捨てプロジェクションである。ストレージ観測（`GET /api/storage`）と性質が同じなので、同じしきい値を使う。独自の数字は発明しない
- **クライアント側の取り直しは `lib/events.ts` の専用グループ（`tuners`）に登録してある。** `storage` と同じ「トピックを持たず専用の周期だけで収束させる」形である。周期も storage と同じ値（`storageRefreshIntervalMs`）を流用する。クエリキーは URL ではなく手書きにする。`/api/sites/{site}/tuners` のままだと `epg` グループの接頭辞（`/api/sites/`）にも一致するため。周期タイマーは周期の値ごとに別の `setInterval` に分かれているので、周期の違う 2 グループに同じキーが入ると、両方が発火する時刻（5 分と 10 分なら 10 分ごと）に同じクエリが 2 回取り直される。接頭辞はグループ定義と同じ定数を参照させ、片方だけ改名して取り直しが止まる drift を防ぐ
- **mirakc は `isFault` を実装しておらず、常に false を返す**（`isAvailable` も常に true。根拠は docs/data/capacity.md §6.5）。つまり「（故障 m）」は現行の mirakc では出ない。それでも読む形にしてあるのは、Mirakurun 互換 API のフィールドであり mirakc が将来実装したら効くため。**この表示の実用上の値は鮮度表示のほうにある** --- そちらは射影ループの停止を実際に捉える

判定は故障の有無・設定・site ごとの分離・鮮度を両方向で確認する。
- 故障ありで destructive の淡い地 + 文字が見えること・無ければ出ないことを確かめる。3 本中 1 本故障という非対称な内訳にしてあるのは、`isFault` の判定を反転させる変異でも表示件数が対称に入れ替わらず実際に落ちるようにするためである。
- 設定で無効化した行は件数から除き、複数 site の本数・故障・鮮度は別行に保つ。
- `observedAt` が古いときは「観測が止まっています」を出し、新しいときは出さない。
- チューナーイベントは専用周期で取り直し、SSE トピックを持たない。5 分と 10 分のタイマーが同時に発火する時刻でも重複取得しない。

<details><summary>根拠</summary>

- 故障表示・無効化・site 別表示・鮮度表示の両方向: `pages/live.test.tsx`「チューナー状態」
- 専用周期・SSE トピック無し・周期の重複防止: `lib/events.ts` の `tuners` グループ、`events.test.tsx`

</details>
