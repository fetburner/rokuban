> [recording.md](../recording.md) §1「方針」・§2「mirakc に任せること」の一部。索引から辿る。

## 1. 方針: mirakc への全面委譲

**「録画（リアルタイム・ハードウェア依存）はエッジの mirakc に、それ以外はサーバー側で弾力的に」**

mirakc に録画を委譲することで、Rokuban のサーバー側に残るのは録画の **コントロールプレーン** --- ルール評価、予約（desired state）生成、mirakc への宣言的同期 --- のみとなる。k8s コントローラと同型のレベルトリガーループで動作する。

重要な帰結として、**Rokuban は TS のストリーム処理（録画・demux・変換）を一切行わない**。TS を変更・解釈する処理は持たないが、**ingest 中の読み取り専用の統計採取は例外**とする（後述の ingest パイプラインを参照）。

### 例外の境界

ドロップ統計に PID の種別（映像 / 音声）を出すため、例外を PSI（PAT / PMT）の読み取りまで広げる。PID 数値だけの統計では「映像が壊れたのか EIT が数回落ちただけか」を区別できず運用判断に使えない。ただし境界を条文として固定する。

| 許す | 許さない |
|---|---|
| PAT / PMT のセクション再構成 | **記述子を一切読まない**（`component_tag` も含む） |
| ES ループの `elementary_PID` と `stream_type` の読み取り | 他テーブル（EIT / SDT / NIT / TOT）の解析 |
| 固定 PID の静的表による命名（PAT / CAT / NIT / SDT / EIT / TOT。解析不要） | ES ペイロードの解釈（字幕デコード・映像解析） |
| PID → 種別の対応を統計メタデータとして記録 | PSI を根拠に EPG プロジェクションを補正すること |
| — | TS の書き出し・変換・demux（この不変条件の本体） |

**「記述子を読まない」が歯止めの本体。** 機械的に判定できる（`Descriptors()` を呼んでいたらレビューで落ちる）し、EIT の解析も自動的に排除される（EIT の中身は記述子）。

**字幕と文字スーパーは区別しない。** ARIB では両者とも `stream_type = 0x06`（private PES）で、区別には `component_tag`（記述子タグ 0x52）が必要になる。守りたいのは映像と音声なので、`stream_type` だけで足りる分類にとどめる。この割り切りにより **ARIB 固有の知識がコードベースに一切入らない**（`stream_type` は ISO/IEC 13818-1 の標準値）。既存依存の gots が `IsVideoContent()` / `IsAudioContent()` を含めて必要なものを全て持っており、自前実装はセクションの取り回しだけになる。

**PSI 解析の失敗は ingest の失敗にしない。** 種別が不明なら PID 数値のまま表示するだけで、ドロップ統計自体は成立する。壊れた PMT で成功するはずの転送を落とさない。

PMT は録画中に更新されうる（version 更新、番組の境目での PID 再割り当て）。同一 PID の分類が途中で変わった場合は**最後に見たものを採用し、変化を検出したことを記録する**。

---

## 2. mirakc に任せること

mirakc の録画機能は分散システムのバックエンドとして非常に都合の良い性質を持つ:

### schedule API による予約

`POST /api/recording/schedules`（programId 単位）でスケジュールを登録する。スケジュールは `recording.basedir/schedules.json` に mirakc 側で永続化されるため、**Rokuban が落ちていても録画は走る**。

### チューナー調停

mirakc の優先度機構（`X-Mirakurun-Priority` 相当 + schedule options の `priority`）に一元化する。ライブ視聴・録画・（併用する場合の）KonomiTV 等が同じ mirakc に載っても、いざという時は録画が勝つ。

### PSI/SI 追従 --- 追従するのは終了だけ

**終了は TS 内の EIT[p/f] に追従する。** 既定の `filters.program-filter`（`mirakc-arib filter-program`）が録画ストリームの EIT[p/f] からイベント時刻を取る。その終端で切る（`--end-margin=2000`）。

**開始は追従しない。賭けている。** mirakc は EPG 予定時刻の 15 秒前（`PREP_SECS`）にチューナーを開くだけで、その時刻自体は放送波を見ていない。filter-program はそこから EIT[p/f] を見て、対象 `eid` が present なら流し、following なら待つ。**どちらでもなければ待たずに失敗する**（`might have been canceled` を出して `stop_`）。待たせる `--wait-until` は録画経路では一切渡されない。`server.program-stream-max-start-delay` は `/api/programs/{id}/stream` 専用のノブで、予約録画には効かない。

つまり繰り下げに耐えるのは「予定の 15 秒前に対象イベントがまだ following に見えている」範囲だけ。数分押している程度なら following のまま残るので実境界で録れる。p/f の並びが入れ替わるほどの繰り下げは**遅れて録るのではなく `recording.failed` になる**。延長・繰り下げの通知（`recording.rescheduled`）と理由付きの失敗（`need-rescheduling` / `removed-from-epg` 等）はここを観測するためにある。

**`recordings.started_at` を実開始として読んではいけない。** watcher が入れるのは mirakc の `record.Recording.StartTime`、つまりチューナーを開いた時刻そのままである。**構造的に「予定 − 15 秒」になる**（実測: 正常終了した録画は全て `-00:00:14.99`）。`reconciler.start_delay_grace` はこの前提に乗っている --- 予定 + 猶予を過ぎて `started_at` が無ければ mirakc 側の異常とみなす検出器であって、実開始のずれを測る道具ではない。

### onair tracker は採らない --- 観測は継続受信の副産物としてのみ採る

開始まで追従させる（および「今放送中」を放送波から得る）には `onair-program-trackers` が要る。**採らない。** チューナー本数の問題ではなく、同じ単価で厳密に優れた買い物があるため。

「今」の真実（EIT[p/f]）は放送波の中にしかなく、外部の番組表は予定しか配らない。読むには**多重化波 1 本あたりチューナー 1 本を恒久的に**払う必要があり、この単価は総本数に依らない。

`onair-program-trackers.local` はその単価（`uses.tuner` は専用で他と共有されない）を払って、表示ラベルだけをサービス単位の直列ポーリングで返す。実測: tune の TTFB は約 2.1 秒（recdvb + PX-S1UD / GR）なので 1 サービス約 7 秒。実運用の 19 サービス構成では 1 周 135 秒で、mirakc が想定する 60 秒周期に収まらない。

同じ単価の `timeshift.recorders` はチューナーを多重化波に固定し、同一波の全サービスで共有する。実イベント境界（`EventStart` / `EventUpdate` / `EventEnd`）を連続で返した上に、追っかけ再生と全録が付く。**tracker の出力は timeshift の真部分集合で遅延だけ大きい**ので、チューナーが潤沢な構成でも選ぶ理由が無い。

> **観測のために新たに恒久専有チューナーを増やさない。観測は、他の目的（timeshift / 全録）で既にその多重化波を継続受信している構成の副産物としてのみ採る。**

remote tracker も使えない。GR は network_id が地域ごとに別なので、他サイトの観測はこちらのサービスについて何も言わない。全国共通の BS/CS を配れる方向には受信設備が無い。**onair はサイトローカルな概念で、全国で 1 つの正本にはならない。**

将来 timeshift を入れるなら観測は追加コスト 0 で付いてくる。ただし mirakc は timeshift の観測を `/api/onair` に結線していないので、そこは上流に足す話になる。採らない側の帰結（予定を正本にし、それを画面で明示する）は [frontend/live.md](../frontend/live.md)「いま放送中」は予定であって観測ではない。

### Records API

録画物は `GET /api/recording/records/{id}/stream` で HTTP 取り出し可能。エッジとサーバー間に共有ファイルシステムが不要になる。

### SSE 再送

`/events` SSE は接続時に既存全 record の `recording.record-saved` を再送する。watcher が落ちていた間のイベントを取りこぼしても、再接続すれば現状と突き合わせて回復できる（実質 at-least-once + 状態同期）。

この再送と、同一 record への `record-saved` の複数回配信は、`internal/mirakc/conformance` の `TestConformance/RecordSavedResentOnConnect` / `TestConformance/RecordSavedFiresMultipleTimes` が mirakc 4.0.0-dev.0 相当に対して判定している。

---

