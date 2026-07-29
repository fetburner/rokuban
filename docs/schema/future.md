## 10. 後続マイグレーションで追加するテーブル

v1 には含めず、後続のマイグレーションで足すもの。参照関係だけ先に固定しておく。

### 追加済み（M2）

| マイグレーション | 内容 |
|---|---|
| `00006_rules.sql` | `rules` + 条件の子表 6 つ + `reservation_rule_matches`。`reservations.rule_id` / `recordings.rule_id` に FK を追加。`array_is_canonical_set` 関数 |
| `00007_epg_search.sql` | `normalize_search_text` 関数と `epg_programs` への式 GIN（全角/半角の吸収。[データ層](../data.md) §5） |
| `00008_program_intents.sql` | `program_intents`。`reservations.overrides` を移設して列を削除（§3.5） |
| `00009_reservation_channel.sql` | `reservations` にチャンネル識別 4 列（§3） |
| `00010_program_overrides.sql` | `program_overrides`（`program_intents` から上書きを分離。§3.5） |
| `00011_circuit_breakers.sql` | `circuit_breakers`（発動のラッチ。§3.6） |
| `00012_drop_reservations_source.sql` | `reservations.source` を削除（#26。導出に寄せる） |
| `00013_dedup_evidence.sql` | `reservations` に重複排除の判定根拠 2 列（M2-6。§3） |
| `00014_drop_stats_pid_type.sql` | `drop_stats.pid_type`（M2-13。§7） |
| `00015_tuner_sync.sql` | `tuner_sync`（チューナー射影。M2-10。§9.5） |

### 未実装

| テーブル | マイルストーン | v1 との接続 |
|---|---|---|
| `orphan_files` | 削除エンジン実装時（M3） | 孤児候補の first_seen 記録（DB リストアで削除窓が開き直す安全弁） |

### ドロップ（M2-12 サービスロゴ）

mirakc は起動中の局ロゴ抽出をサポートしない（運用者が `mirakc-arib` 等で事前抽出したファイルを `config.yml` に静的登録し、mirakc は `/api/services/{id}/logo` で配るだけ）。Rokuban 側で再取得・ハッシュ管理・自前配信を持つ価値が薄く、見送る（issue #24 のコメント参照）。`epg_services.has_logo_data` / `logo_id` 列は mirakc の `Service` 構造体をそのまま射影しているだけで、これ自体の削除は不要。

EPG プロジェクションが v1 に入らなかった理由: 使い捨てキャッシュであり永続資産と寿命が違う（§9）。「最終形で切る」対象は、他の全タスクが依存し、後から変えると痛い**永続資産と desired/observed の骨格**。

## 11. 未決事項（実装前に issue で確定させる）

1. ~~複数ルールマッチのトレーサビリティ~~ → **確定（issue #3 のコメント）**: base 内の配列ではなく中間テーブル `reservation_rule_matches (reservation_id, rule_id)`。「このルールが今どの予約を生んでいるか」の逆引き（ルール削除の影響プレビュー）が要るため。ruler が毎パス書く導出状態なので FK は両側 ON DELETE CASCADE
2. **`record_sync.recording_id` の NOT NULL 化**: 現設計は外部産 record を NULL で表現する。外部産を track しない（行を作らない）選択肢もある
3. ~~drop_stats の PID 名~~ → **確定（M2-13。[録画エンジン](../recording.md) §1「例外の境界」）**: PAT → PMT の `stream_type` までを読み `drop_stats.pid_type` に記録する。**記述子は一切読まないので `component_tag` は使わない**（当初の想定から狭めた）。従って字幕と文字スーパーは区別しない（ARIB では両方 `stream_type = 0x06`）。守るのは映像と音声だけという割り切りで、ARIB 固有の知識がコードに入らない（`stream_type` は ISO/IEC 13818-1 の標準値）
4. ~~ルールとサイトの対応~~ → **確定（[録画エンジン](../recording.md) §3.1「サイトの扱い」）**: ルールはサイトに従属せず、サイトは条件の一次元（`rule_sites` 子テーブル、指定なし = 全サイト）。実体化はマッチした全サイトで N 予約（複数録画 → ドロップ統計で選別する運用を一級とする）。サイト名は安定識別子でリネームは運用作業
