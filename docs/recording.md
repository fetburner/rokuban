# 録画エンジン: mirakc への委譲と予約同期（索引）

録画の実行は [mirakc](https://github.com/mirakc/mirakc) に全面的に委譲する。Rokuban はルールエンジンと予約の宣言的同期に徹する。

**本文は `docs/recording/` に分割してある。節番号は分割前のまま**なので、コードコメントの「recording.md §3.2」等はこの表で該当ファイルを引ける。

| 節 | 内容 | ファイル |
|---|---|---|
| §1 | 方針: mirakc への全面委譲 / **例外の境界**（PSI 読み取りをどこまで許すか） | [recording/delegation.md](recording/delegation.md) |
| §2 | mirakc に任せること（schedule API / チューナー調停 / PSI 追従 / Records API / SSE 再送） | [recording/delegation.md](recording/delegation.md) |
| §3 §3.1 | **ruler**（ルール評価 → 予約生成）: 全量評価 + 差分書き込み / 複数ルール解決 / 重複排除 / サイトの扱い | [recording/ruler.md](recording/ruler.md) |
| §3.2 | **reconciler**（宣言的同期）: ファイル名テンプレート / 予約オプションの差分反映 / 再作成のガード / **大量削除サーキットブレーカー** / 番組終了後の GC | [recording/reconciler.md](recording/reconciler.md) |
| §3.3 | **watcher**（SSE 購読・状態反映）: 3 段構えの信頼性設計 / record 処理の冪等性 / 品質メタデータ / 開始遅延検出器 | [recording/watcher.md](recording/watcher.md) |
| §4 | **予約モデル: base / overrides 分離** —— 設計根拠（EPGStation v2.10.0 の問題）/ `program_intents` と `program_overrides` / jsonb を許す条件 / detached のライフサイクル / manual 予約との統一 / 録画開始後の編集 | [recording/reservation-model.md](recording/reservation-model.md) |
| §5 §6 | **ingest パイプライン**（転送方式 / インライン TS ドロップスキャン / リトライ設計 3 層 / worker）と **B-CAS 復号の責務境界** | [recording/ingest.md](recording/ingest.md) |
| §7 §8 §9 | mirakc schedule options / 録画品質の実測 / 落とした機能・スコープ外 | [recording/reference.md](recording/reference.md) |

読む順の目安:

- **ruler を触る** → §3.1 と §4（予約モデル）
- **reconciler を触る** → §3.2 と §7（schedule options）
- **watcher を触る** → §3.3
- **ingest / tsstat を触る** → §1 の「例外の境界」と §5・§6
- **予約 API を触る** → §4

> 関連ドキュメント: [overview.md](overview.md)（全体アーキテクチャ）/ [data.md](data.md)（データ層）/ [schema.md](schema.md)（スキーマ）/ [storage.md](storage.md)（メディアストレージ）/ [operations.md](operations.md)（運用）
