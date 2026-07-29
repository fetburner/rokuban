## 1. 方針: mirakc への全面委譲

**「録画（リアルタイム・ハードウェア依存）はエッジの mirakc に、それ以外はサーバー側で弾力的に」**

mirakc に録画を委譲することで、Rokuban のサーバー側に残るのは録画の **コントロールプレーン** --- ルール評価、予約（desired state）生成、mirakc への宣言的同期 --- のみとなる。k8s コントローラと同型のレベルトリガーループで動作する。

重要な帰結として、**Rokuban は TS のストリーム処理（録画・demux・変換）を一切行わない**。TS を変更・解釈する処理は持たないが、**ingest 中の読み取り専用の統計採取は例外**とする（後述の ingest パイプラインを参照）。

### 例外の境界（M2 で PAT / PMT まで拡張）

ドロップ統計に PID の種別（映像 / 音声）を出すため、例外を PSI の読み取りまで広げる。PID 数値だけの統計では「映像が壊れたのか EIT が数回落ちただけか」を区別できず運用判断に使えない（issue #23）。ただし境界を条文として固定する。

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

### PSI/SI 追従

EPG 上の時刻ではなく TS 内の PSI/SI（イベント ID）を監視して実際の放送開始・終了に追従する。延長・繰り下げは `recording.rescheduled` で通知され、追従不能時も理由付きの `recording.failed`（`need-rescheduling` / `removed-from-epg` 等）が飛ぶ。

### Records API

録画物は `GET /api/recording/records/{id}/stream` で HTTP 取り出し可能。エッジとサーバー間に共有ファイルシステムが不要になる。

### SSE 再送

`/events` SSE は接続時に既存全 record の `recording.record-saved` を再送する。watcher が落ちていた間のイベントを取りこぼしても、再接続すれば現状と突き合わせて回復できる（実質 at-least-once + 状態同期）。

---

