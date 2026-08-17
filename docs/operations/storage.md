> [operations.md](../operations.md) §4「ストレージ運用」の一部。索引から辿る。

## 4. ストレージ運用

### 録画バッファのサイジング

録画バッファ（mirakc `recording.basedir`、エッジのローカルディスク）のサイジング指針:

- **容量の支配項は同時録画数ではなく「ingest が詰まったときの滞留分」**。回線断・クラウド側障害時は未 ingest の record が溜まり続ける。推奨値は**「N 日分の全録画を保持できる容量」**（地デジ約 7 GB/時で見積り）
- **速度要件は絶対帯域ではなくレイテンシ**。書き込みは 1 録画あたり約 2 MB/s（地デジ 17 Mbps）で、同時 8 本でも 16 MB/s に過ぎない。怖いのは他 I/O との競合によるレイテンシスパイクで、ingest pull のサイト単位 1〜2 本キャップはこのための決定でもある

録画バッファの容量アラート（[監視メトリクス](monitoring.md) の「未 ingest record 総量」と[アラート設計](alerts.md) のエッジディスク残量アラート）とセットで運用する。

#### N 日は容量だけでは決まらない: `epg.retention_grace` との交点

**ディスクを N 日分積んでも、encode 意図が生き残る滞留の上限は `epg.retention_grace`（既定 24h）である**。録画の encode policy（`keepOriginal` / `encodeProfiles`）は ingest が原本をコミットする瞬間に `program_snapshots` 経由で予約から解決して凍結される（[ストレージ](../storage.md) §6）。その `program_snapshots` は**放送終了 + `epg.retention_grace` で GC される**。GC は DB の時計だけで動き、エッジに到達できるかどうかを見ない。

> **滞留を N 日まで許すつもりでリングバッファをサイジングするなら、`epg.retention_grace >= N` にする。**

GC がその番組のスナップショットを刈った後に ingest が走ると、その録画は既定ポリシー（`keep_original='always'` / `encode_profiles=[]`）で凍結される。原本は残るのでデータは失われないが、**エンコードは投入されず、`recordings.source` は `manual` に・`rule_id` は NULL に落ちる**。復帰は障害復旧時に一括で起きるので、まとまった件数が同時にこうなりうる。事後回復は `POST /api/recordings/{id}/encode-profiles`（追加のみ）**だけ**で、`encode_reconcile` の定期パスは拾わない（desired が空なので候補にならない。[ストレージ](../storage.md) §6）。

**ただし GC 自身が止まる障害では話が違う**。worker 全停止や DB 到達不能のように `ruler_pass` ごと止まる障害では断のあいだ GC も進まない。復帰時は sweep + ingest と `ruler_pass` の競争になり、ingest が先に走れば意図は守られる（どちらが先かはジョブの実行順に依存する。未検証）。決定論的に上の帰結になるのは「**GC は動き続けたが、その録画の ingest だけが猶予を跨いで遅れた**」場合。

**`epg.retention_grace` を上げるのは無料ではない**。同じキーが EPG 射影のローリングウィンドウ（`PruneEpgPrograms`）も駆動する。N 日にすると `epg_programs` に全サービスの放送済み番組が N 日ぶん残る。終了済み番組に対して永続表 `never_scheduled_events` へ欠測行が作られる窓も同じだけ広がる（増加量は未検証）。詳細と、**GC 側を滞留と連動させない理由**、確認しているテストは [ストレージ](../storage.md) §6「凍結が依存する寿命と、エッジの滞留の交点」にある。

見張るメトリクスは**「その record が `finished` としてクラウドに観測されていたか」**で分かれる（「リンクが生きているか」ではない）。**どちらも閾値は「悪い状態が `epg.retention_grace` を超えて続いたか」**（[監視メトリクス](monitoring.md) の一覧）:

| 滞留の型 | 見るメトリクス |
|---|---|
| `finished` として観測済みなのに ingest が詰まる（worker 停止・ストレージ障害・キュー詰まり。**断の直前に `finished` として観測済み**の record もこちら） | `rokuban_uningested_records{site}` / `rokuban_uningested_record_bytes{site}` の増加 |
| エッジ↔クラウドの回線断で、**断の最中に始まった録画と、断が始まった時点でまだ録画中だった録画** | `rokuban_sweep_last_pass_timestamp_seconds` / `rokuban_epg_sync_last_success_timestamp_seconds` の停滞 |

後者では未 ingest メトリクスは増えない。`GetUningestedRecordBacklog`（`internal/db/queries/metrics.sql`）の述語は `record_sync.status = 'finished'` である。その行は watcher が mirakc を観測して初めて作られる・更新される。したがって**断の最中に始まった録画（行そのものが無い）だけでなく、断が始まった時点で `status='recording'` だった録画も gauge には現れない**。`finished` への更新には復帰後の再観測が要るため、断のあいだ gauge は平らなまま。**滞留量のアラートだけでは回線断を検知できない。**

> 「アンカーがある（案 1 で留め置ける）」と「gauge に出る」は**別の条件**。案 1 は述語を自分で選べるので `status='recording'` の観測でも足りる（[ストレージ](../storage.md) §6）が、gauge の述語は `finished` 固定で選べない。

### アーカイブの速度要件

アーカイブ（Rokuban のメディアストレージ。ローカル FS / NAS / CSI の S3）は低速で良い:

- **平均スループット >= 1 日の録画総量 / 24 時間**。瞬間的な変動は録画バッファが吸収するので、リアルタイム性は一切要求されない。エンコードの読み出しもバッチなので遅くて良い
- 唯一レイテンシが人間に見えるのは**再生時のシーク**（S3 + FUSE の range read）。原本削除ポリシーと組み合わせた「視聴は H.265 派生物、原本は消すか S3 の奥」という運用が前提なら実用上問題にならない見込み

### disaster recovery（catalog + rescue）

`rokuban rescue` は **DB を失った後にだけ使う**。**live DB を catalog の内容で上書きするので健全性の確認には使わない**（確認は `rokuban catalog verify`。[DB 運用](database.md) のバックアップ節）。ストレージを走査し、

- `catalog/` に**完成世代**（manifest まで書き終えた世代ディレクトリ。[storage.md](../storage.md) §8）があれば照合してフルメタデータ（番組情報・ドロップ統計・保持ポリシー）ごと復元。最新世代が不完全なら 1 世代前へ落ち、飛ばした世代を理由付きで出力する
- catalog にないファイルはディレクトリ規約・ファイル名から推定できる範囲で「素の asset」として登録（UI から見えて再生できる状態に戻す）

実装は `rokuban import epgstation` の in-place 登録機構と同型なので共有する。

既存不変条件の再確認:

- **「放送データのコピーが常に 1 つ以上」は DB 喪失時も維持される**。エッジ record の削除は ingest の DB コミット後に起きる。コミット直後に DB を失ってもファイルはアーカイブに存在し、孤児回収の安全弁が守り、rescue が再登録する
- cleanup は mirakc の basedir に絶対に触らない（エッジ側削除は ingest の検証済み削除のみ）
