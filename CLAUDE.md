# Rokuban 開発ガイド

## プロジェクト概要

Rokuban（録番）は EPGStation をゼロベースで再設計するクラウドネイティブ録画サーバー。録画実行は mirakc に全面委譲し、Go 単一バイナリでオーケストレーションと I/O に徹する。

## ビルド・テスト

```bash
sqlc generate                 # SQL → Go コード生成（マイグレーションからスキーマを読む）
go generate ./internal/api/   # openapi.yaml → 生成ハンドラ/型。生成物はコミットする
go build ./...
go test ./...
golangci-lint run
```

```bash
cd web
pnpm install
pnpm test      # Vitest。M2-5 で導入（それまでフロントにテスト基盤が無かった）
pnpm lint      # oxlint。既存の warning が 3 件あるので増えていないかで見る
pnpm build     # tsc -b && vite build。型エラーはここで出る
pnpm exec orval  # openapi.yaml → web/src/api/generated.ts
```

**`go test ./...` は Postgres を要求する。** `ROKUBAN_TEST_DATABASE_URL` を設定していないと DB を使うテストが落ちる（`internal/testutil` がパッケージごとに DB を作り、テストごとに TRUNCATE する）。ローカルなら `postgres://localhost:5432/postgres?sslmode=disable` で足りる。

**`oapi-codegen` / `golangci-lint` は PATH に無いことがある。**

- `golangci-lint`: `$(go env GOPATH)/bin/golangci-lint`
- `oapi-codegen`: `cd internal/api && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config=oapi-codegen.yaml ../../openapi.yaml`
  - **`@version` 付きの `go run` は `go.mod` を汚さない。** ツールを `tool` ディレクティブで足すと indirect 依存が 15 個増え、ビルドツールのために Dependabot のアラート面が広がる。`sqlc` も同じくローカルバイナリ前提なので揃える

## 設計ドキュメント

実装の根拠はすべて GitHub issue と docs/ に確定済み。実装中に設計判断を変えたくなったら実装せず issue にコメントで提起する。

### 資料マップ

| issue | 内容 | 対応 doc |
|---|---|---|
| #1 | 全体アーキテクチャ（nginx/認証/イメージ配布/サーバーレス/B-CAS） | [docs/overview.md](docs/overview.md) |
| #2 | 録画エンジン mirakc（ingest 詳細/ruler 仕様/base-overrides） | [docs/recording.md](docs/recording.md) |
| #3 | データ層（検索・ルール評価/EPG プロジェクション/DB 輻輳隔離） | [docs/data.md](docs/data.md) |
| #4 | ストレージ契約（2 階層/削除エンジン） | [docs/storage.md](docs/storage.md) |
| #5 | フロントエンド | [docs/frontend.md](docs/frontend.md) |
| #6 | 移行計画とマイルストーン | — |
| #9 | 設定 | [docs/configuration.md](docs/configuration.md) |
| #10 | EPGStation トリアージ | — |
| #11 | 懸念トラッキング | — |
| #13 | M1 タスク分解（スキーマ v1） | [docs/schema.md](docs/schema.md) |

### 不変条件

すべての実装タスクで遵守する。

1. **api ロールは mirakc に問い合わせない**。ファイルシステムにも依存しない（go:embed は可）
2. **mirakc とのやりとりは常に API**、自身のストレージとは常にファイル I/O（S3 SDK 禁止）
3. **コミット = DB 行**。ファイルの存在はコミットではない
4. **ffmpeg/ffprobe の exec は worker / streamer パッケージのみ**
5. **レベルトリガー**: イベント（SSE/NOTIFY）はヒント。真実は定期 reconcile が再取得する
6. **TS のストリーム処理をしない**（ingest 中の読み取り専用統計のみ例外）。統計のための PSI 読み取りは PAT / PMT の `stream_type` までで、**記述子は読まない**（[docs/recording.md](docs/recording.md) §1「例外の境界」）
7. **mirakc 固有の概念を永続テーブル（rules / media_assets / 履歴）に入れない**
8. **テストのないタスク完了はない**
9. **導出値と不可逆な事実を同じ列に載せない**（下記）
10. **意味を持たない行を作らない**（下記）

> 9 と 10 は M2-4 / M2-5 で**同じ歪みを 3 回踏んでから**言語化した。番号は追加のみ（既存の 1〜8 は docs が番号で参照しているので振り直さない）。

#### 9. 導出値と不可逆な事実を同じ列に載せない

毎パス再計算される値と、二度と再取得できない事実を 1 つの列に同居させると、**導出側が事実を上書きする**。踏んだ実例:

| 列 | 混ざっていた 2 つの事実 | 症状 |
|---|---|---|
| `program_intents.action` | ①録る/録るな ②パラメータを上書きしたか | `action NOT NULL` のため「上書きだけ」を表現できず、掃除の判定が別表の `rule_id` に依存して誤答した（#18） |
| `reservations.source` | ①ユーザーが手動予約したか ②いまルールが base を供給しているか | ruler が manual → rule に**不可逆に**書き換え、永続資産の `recordings.source` に恒久的に漏れた（#26） |
| `reservations.state` | ①番組終了後に schedule が観測されなかった ②ルールが base を供給しているか | 導出値（`active`/`detached`）を同期フィルタに使い、**手動予約が黙って録画されなくなった**（M2-4） |

チェック: 新しい列を足すとき「この値は毎パス作り直せるか」を問う。作り直せるなら**列にせず導出する**。作り直せない事実と混ぜてはならない。

#### 10. 意味を持たない行を作らない

**行の存在そのものを主張として使う。** 「何も主張していない行」を作ると、それを掃除する規則が必要になり、その規則が判断材料を別表に求めて壊れる。

- `program_overrides`: 空の上書き = 行が無い（`{}` の行を作らない）
- `circuit_breakers`: 行の存在 = 発動中（「停止していない」を表す行は無い）。再開は DELETE

「あってはいけない組み合わせ」は **CHECK で禁止するより表現不可能にする**方が強い。CHECK は覚えておくべき規則だが、表現不可能なら忘れようがない。

### コーディング規約

- Go 標準プロジェクトレイアウト（`cmd/` + `internal/`）
- ログは `log/slog`
- エラーは握り潰さず `fmt.Errorf("...: %w", err)` で文脈付き wrap
- 各タスクは 1 PR 粒度。着手前に対応 issue の本文とコメントを必ず読む
- **doc コメント**: エクスポートされた関数・型・メソッド・定数には [Go Doc Comments](https://go.dev/doc/comment) 規約に従った doc コメントを書く。`// FuncName は〜` の形式で主語を識別子名にする。非公開でも他パッケージから呼ばれうる重要な関数には書く

### テスト規律

**「テストのないタスク完了はない」（不変条件 8）は「通るテストを書く」ではない。**

- **書いたテストは意図的に実装を壊して落ちることを確認する。** 通るだけのテストは何も保証しない。実際に、生成 SQL を文字列一致で見るだけのテストがタイムゾーンのバグを通してしまった
- **落ち方を確認する。** ビルドエラーで落ちたのはアサーションの検証になっていない（未使用変数で壊すとこれが起きる）。壊し方はコンパイルが通る形にする
- **非同期の空虚な成功に注意する。** 「何も表示されない」系のテストは、クエリが解決する前にアサーションが走って通ることがある。読み込み完了を待つ手段を入れてから、壊して落ちることを確認する
- 分岐を直したら**両方向**で確認する（片側だけ見ると反転しても気付かない）

### 並行作業（複数エージェント）

**契約を先に書いてコミットしてから並列に投げる。** M2-5 ではテーブル・クエリ・`internal/breaker`・メトリクスを先に固定したので 3 本が競合なしに走った。

- **`openapi.yaml` はボトルネック。** 触るのは 1 本に限定する（description だけの狭い例外は可）
- **クエリファイルは新規に切る。** 既存の `reservations.sql` 等を複数が触ると取り合いになる
- **git が競合と見なさない意味的な競合がある。** M2-8 の `sqlc.embed(r)` が #26 で削除した列を含んでいて、行としては衝突しないのでマージは成功し、その後ビルドが落ちた。テストフィクスチャの生 SQL も同様
- **マージ後に必ず生成物を再生成してフルテストを回す**（`sqlc generate` / `go generate ./internal/api/` / `go test ./...`）。マージが通ったことは何の保証にもならない
- エージェントの報告は検証してから信じる。実際に「docs に古い記述がある」という報告が grep すると存在しなかったし、指示範囲外の妥当な修正（分離が生んだ新経路の取りこぼし）を見つけてきたこともある
