> [recording.md](../recording.md) §7〜§9 の一部。索引から辿る。

## 7. mirakc schedule options

mirakc の予約には、開始・終了マージンを予約ごとに指定するオプションが存在しない。どちらも予約単位では動かせない固定の仕組みで決まる。追従するのは終了だけで、開始側は [delegation.md](delegation.md)「PSI/SI 追従」の賭けに乗っている。

「PSI/SI 追従だからマージンが不要」という理解は採らない。終了側は既定の program-filter が決める値、開始側は mirakc が予定の 15 秒前にチューナーを開く固定値で、予約ごとには変えられない。

予約の形は次の通り。

```json
{
  "programId": 327360102415397,
  "options": {
    "contentPath": "videos/path/to/file.m2ts",
    "priority": 1,
    "preFilters": [],
    "postFilters": [],
    "logFilter": "info"
  },
  "tags": ["program:1234"]
}
```

- `contentPath`: `recording.basedir` からの相対パス必須（絶対パス・`../`・basedir 外はバリデーションで 4xx）。`recording.records-dir` 設定時は省略可（自動生成名）
- `priority`: チューナー使用優先度（競合時の調停）
- `preFilters` / `postFilters`: config で定義した名前付きフィルタパイプライン
- `logFilter`: mirakc-arib のログをコンテンツ横の `<ファイル名>.log` に出力（EPGStation が自前生成していたドロップログの代替。表示用のパース層は必要）
- `tags`: 自由文字列。Rokuban は programId 埋め込みに使う（`program:{programId}`。詳細は [reconciler.md](reconciler.md)「tags 対応付け」参照）

<details><summary>根拠</summary>

- 終了側のマージン: 既定の program-filter が `--end-margin=2000` を持つ
- 開始側のマージン: mirakc の固定値 `PREP_SECS`（予定の 15 秒前にチューナーを開く）

</details>

---

## 8. 録画品質の実測

mirakc の追従品質は EDCB ほどの長期実績がないため、録画失敗と欠損を継続して計測する。メトリクス化する項目は次の通り。

- `recording.failed` の理由別集計
- `recording.record-broken` の記録
- 原本解析時のドロップ統計（PID 別 continuity counter 不連続 / TEI）
- scrambled カウンタ（B-CAS 障害検出）
- 開始遅延検出器（開始時刻超過 + recording.started 未観測）

品質問題が実測されたら、その時点でエンジン追加を再検討する。

---

## 9. 落とした機能・スコープ外

時刻指定予約は機能ごと落とし、mirakc の多段集約と EDCB ドライバは採らない。録画エンジンの抽象化レイヤーも作らない。イベントリレー追従は、委譲による退行が無い。

### 時刻指定予約

mirakc の予約 API は programId ベースのみで、「サービス X を 21:00〜22:00」を表現できない。用途（変則開始時刻の番組をチューナーのやりくりで録る等）は mirakc の優先度調停 + 番組追従でほぼ吸収されるため、**機能ごと落とす**。撮り逃しは再放送を待つ運用。

### イベントリレー追従

mirakc 利用時は現行 EPGStation でも効いていないため、委譲による退行はない。番組追従自体は mirakc が TS ベースで行うぶん改善方向。

### mirakc の多段集約

mirakc の多段集約は採らない。mirakc は上流チューナー（別の Mirakurun 互換サーバをチューナーとして使う種類）を定義できるが、その構成ではサイト間に余計な通信経路が挟まってストリームが劣化し、録画品質そのものを損なう。多拠点は「サイトごとに独立した mirakc + Rokuban が集約」の形に限る。

副次的な利点として、容量超過の判定（[データ層](../data.md) §6.5）が「チューナーは 1 サイトに属する」を前提にできる。集約構成を許すと、API から見えない形でサイト間の容量が共有され、判定の前提が成り立たなくなる。

<details><summary>根拠</summary>

- 採らない構成のチューナー種別: `upstream`

</details>

### EDCB ドライバ

EDCB（Linux ネイティブビルド可、番組追従の実績は最強）を録画エンジンの選択肢として検討したが、**採用しない**。Windows ネイティブ由来のソフトであること、CtrlCmd（バイナリ TCP）+ Lua HTTP という API、録画物がローカルファイルでエッジ転送エージェントが必要になることが理由である。

録画エンジンの抽象化レイヤーも作らない（YAGNI。mirakc API の呼び出しが reconciler / watcher に局所化されること自体が十分な継ぎ目）。

### 「この番組シリーズは常に...」のような永続的例外

[reservation-model.md](reservation-model.md) §4.6 参照。
