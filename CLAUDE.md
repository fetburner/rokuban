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
pnpm test      # Vitest
pnpm lint      # oxlint。既存の warning が 3 件あるので増えていないかで見る
pnpm build     # tsc -b && vite build。型エラーはここで出る
pnpm exec orval  # openapi.yaml → web/src/api/generated.ts
```

**`go test ./...` は Postgres を要求する。** `ROKUBAN_TEST_DATABASE_URL` を設定していないと DB を使うテストが落ちる（`internal/testutil` がパッケージごとに DB を作り、テストごとに TRUNCATE する）。ローカルなら `postgres://localhost:5432/postgres?sslmode=disable` で足りる。

**sqlc は式の型を推論しきれないことがある。** `program_start_at + interval '...'` のような
式に `::timestamptz` を明示しないと `int32` として生成され、`Scan` で必ず落ちる。
式を SELECT に置くときは明示キャストを付ける。

**`oapi-codegen` / `golangci-lint` は PATH に無いことがある。**

- `golangci-lint`: `$(go env GOPATH)/bin/golangci-lint`
- `oapi-codegen`: `cd internal/api && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config=oapi-codegen.yaml ../../openapi.yaml`
  - **`@version` 付きの `go run` は `go.mod` を汚さない。** ツールを `tool` ディレクティブで足すと indirect 依存が 15 個増え、ビルドツールのために Dependabot のアラート面が広がる。`sqlc` も同じくローカルバイナリ前提なので揃える

## 設計ドキュメント

**設計の権威は docs/ にある。** 実装中に設計判断を変えたくなったら実装せず issue にコメントで提起する。

### 資料マップ

**タスクに関係する doc だけ読む。** 大きい doc は索引 + 分割本文になっているので、索引で節を特定してから該当ファイルだけ開く。**形（REST のパス・パラメータ / 設定キー / マイグレーション）の権威は `openapi.yaml` / `config.example.yml` / `internal/db/migrations/` にあり、docs は判断（なぜ）だけを持つ。**

| doc | 内容 | 形 |
|---|---|---|
| [docs/overview.md](docs/overview.md) | 全体アーキテクチャ（ロール分類/nginx/認証/イメージ配布/サーバーレス/B-CAS） | 単一 |
| [docs/recording.md](docs/recording.md) | 録画エンジン（ruler/reconciler/watcher/予約モデル/ingest/B-CAS）。用語表とデータフロー図はここ | **索引** → `docs/recording/` |
| [docs/schema.md](docs/schema.md) | DB スキーマ v1（設計原則/全テーブル/ER 図） | **索引** → `docs/schema/` |
| [docs/data.md](docs/data.md) | データ層（River/NOTIFY/検索・ルール評価/EPG 射影/チューナー射影/輻輳隔離） | **索引** → `docs/data/` |
| [docs/api.md](docs/api.md) | API 設計（REST の判断/SSE/メディア配信/認証/プロキシ） | **索引** → `docs/api/` |
| [docs/storage.md](docs/storage.md) | ストレージ契約（2 階層/削除エンジン/catalog・rescue） | **索引** → `docs/storage/` |
| [docs/frontend.md](docs/frontend.md) | フロントエンド | **索引** → `docs/frontend/` |
| [docs/configuration.md](docs/configuration.md) | 設定の判断（キーの網羅は `config.example.yml`） | 単一 |
| [docs/operations.md](docs/operations.md) | 運用（監視/アラート/DB/ストレージ/k8s・ロール分割） | **索引** → `docs/operations/` |
| [docs/runbook.md](docs/runbook.md) | 手動での動作確認手順 | **索引** → `docs/runbook/` |
| [docs/invariants.md](docs/invariants.md) | 不変条件 9〜13 の経緯・失敗事例（表と war story）。ルールの根拠を辿るときだけ | 単一 |
| [docs/workflow.md](docs/workflow.md) | タスク分解・docs 保守・並行作業の規律。該当作業のときだけ | 単一 |

### タスクマップ

タスクの分解・受け入れ基準は GitHub issue 側にある。**親 issue には一覧しか置かない**ので、`gh issue view <親>` でタスク表を見て、**担当タスクのサブ issue だけ読む**。

M0（歩く骨格）・M1（録れる）・M2（任せられる）の実装は完了している。open なのは次だけ。

| | 入口 |
|---|---|
| M2 の出口基準の検証（EPGStation と 1〜2 週間並走し、予約差分がゼロ or 全件説明可能） | [#52](https://github.com/fetburner/rokuban/issues/52) |
| M3 タスク分解: 置き換えられる（エンコード・削除・移行）。サブ #63〜#75 | [#62](https://github.com/fetburner/rokuban/issues/62) |
| M4 タスク分解: 広げられる（ロール分割デプロイ・ライブ視聴・クラウド構成）。サブ #89〜#97 | [#88](https://github.com/fetburner/rokuban/issues/88) |
| M5 タスク分解: 名乗れる（イの計器盤 — デザイン言語と頻度階層）。サブ #224〜#228 | [#220](https://github.com/fetburner/rokuban/issues/220) |
| M6 タスク分解: 辿れる（画面間の導線とオブジェクトの着地先）。サブ #229〜#233 | [#221](https://github.com/fetburner/rokuban/issues/221) |
| M7 タスク分解: 見積もれる（資源の値札と残高）。サブ #234〜#239 | [#222](https://github.com/fetburner/rokuban/issues/222) |
| M8 タスク分解: 見返せる（ホームとライブラリ）。サブ #240〜#242 + 判定基準の決定後に起票 3 件 | [#223](https://github.com/fetburner/rokuban/issues/223) |

- **`reservations` と shadow-diff（#52 の出口基準を測る道具そのもの）を触るタスクは、#52 の並走中は着手しない。** 測定の連続性が切れる。並走が始まっているかは #52 を見て判断する（始まっていなければこの制約は効かない。「並走中は着手しない」と書いている issue #98 / #101 / #129 も同じ基準で判断する）
- **streamer のスケールとライブ視聴の資源同定は [docs/operations.md](docs/operations.md) §5「streamer のスケール」と [docs/api.md](docs/api.md) §ライブ視聴の HLS に決まっている。** sticky は使わない / ライブの URL にセッション ID を置かない / 既定 replicas=1 は可逆にする、の 3 点。触るタスク（#91 / #94）は実装前にこの 2 節を読む

### タスク分解と issue

**タスク分解を頼まれたら [docs/workflow.md](docs/workflow.md) §タスク分解と issue に従う。** 要点: マイルストーンごとにエピック 1 本 + 自己完結したサブ issue（`.github/ISSUE_TEMPLATE/` の `epic` / `sub-issue` テンプレート準拠）。

### ドキュメントと issue の保守

詳細は [docs/workflow.md](docs/workflow.md) §ドキュメントと issue の保守。全タスクに効く要点だけここに置く:

- **実装と同時に docs を更新する。** 実装が docs を追い越したらその PR で直す（別タスクにしない）。古い記述は無いより悪い
- **終わったタスクはその場で close し、close したものへのポインタを索引に残さない**
- **issue 番号・タスク番号は経緯の置き場（invariants.md・各ファイル末尾の「経緯と失敗事例」節・タスクマップ・失敗の証拠）にだけ書く。** 現行仕様の本文に出自として書かない（「M3-1 で変えた」等）。番号を読まないと本文が理解できない状態にしない
- **過去の失敗の記録は消さない。** 消すのは「現在の実装はこうである」と読めて事実でなくなった記述だけ

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
11. **形を固定する前に、その形を決める判定基準を書く**（下記）
12. **表は行の寿命で割る**（下記）
13. **永続表に列を足すとき、書くループが脊椎の書き手でなければ衛星表にする**（下記）

> 番号は追加のみ（既存の 1〜8 は docs が番号で参照しているので振り直さない）。9〜13 の**言語化した経緯・失敗事例（表と war story）は [docs/invariants.md](docs/invariants.md) に退避した。** ここには各条件の「チェック」だけ置く。ルールの根拠を辿るときや、間違えそうになったときだけ docs を開く。

#### 9. 導出値と不可逆な事実を同じ列に載せない

毎パス再計算される値と、二度と再取得できない事実を 1 つの列に同居させると、**導出側が事実を上書きする**（`program_intents.action` / `reservations.source` / `reservations.state` の 3 例。経緯は invariants.md）。混同は列だけでなく identity・式・適用の瞬間にも起きる。

チェック:
- 新しい列を足すとき「この値は毎パス作り直せるか」を問う。作り直せるなら**列にせず導出する**。作り直せない事実と混ぜてはならない。
- 新しいエンドポイントを足すとき「**宛先のキーは誰が作るか**」を問う。導出器が作るキーは API の宛先に置かない（`base.skip` を覆す意図を書けなくなる）。
- `WHERE a.x_id = b.id` を書くとき「**この id は誰が作り、いつ変わるか**」を問う。導出器が作るなら放送イベント `(site, network_id, service_id, event_id)` や `(site, program_id)` のように**外から与えられたキー**で引く。導出器が作るキーを保存する列は、読者を移設し終えたら残さない（実例 6 件は invariants.md）。
- 導出を列に焼くなら**両方向のテストを書く**。導出の判定をメモリに持って後で適用するなら、適用の側で判定条件を再評価する（読み書きの割り込み窓）。

#### 10. 意味を持たない行を作らない

**行の存在そのものを主張として使う。** 「何も主張していない行」を作ると、それを掃除する規則が必要になり、その規則が判断材料を別表に求めて壊れる。

- `program_overrides`: 空の上書き = 行が無い（`{}` の行を作らない）
- `circuit_breakers`: 行の存在 = 発動中（「停止していない」を表す行は無い）。再開は DELETE

「あってはいけない組み合わせ」は **CHECK で禁止するより表現不可能にする**方が強い。CHECK は覚えておくべき規則だが、表現不可能なら忘れようがない。

#### 11. 形を固定する前に、その形を決める判定基準を書く

**分類（ロール）や形（スキーマ・API）を先に固定して判定基準を後から書くと、基準が来た時点でやり直しになる**（M2 で 3 回。代償は [docs/invariants.md](docs/invariants.md)）。

- **「最終形で切る」の対象は永続資産（`recordings` / `media_assets` / `drop_stats` / `rules`）と外から見える資源同定（API のパスと識別子）に限る。** 導出テーブル（`reservations` / `*_sync` / 射影）の列は、**それを書くコードと同じ PR で決める**（churn のコストが非対称）。
- **将来への先払いは高い方（API の資源同定・キャッシュキー）から。** 安い方（DB 列）だけ先に払っても利益は出ない。

チェック: 新しいテーブル・列・ロール・エンドポイントを足すとき「**これを書く / 使うコードは今あるか**」を問う。無ければ形を決めず、書き手と同じ PR に回す。

#### 12. 表は行の寿命で割る

**1 表 = 1 つの書き手 = 1 つの寿命。** 不変条件 9 は**列**の粒度なので、行に寿命が混ざっているケースを網に掛けられない（`reservations` に導出出力・番組スナップショット・不可逆な観測の 3 寿命が同居していた。経緯は invariants.md）。分割の軸を「予約という概念」に取ったので解消が 2 回に分かれた —— 軸を寿命に取れば 1 回で済む。

チェック: 新しい列を足すとき「**この値はこの行と同時に生まれて同時に死ぬか**」を問う。違えば `(site, program_id)` を主キーにした別表にする。1 つの表に書き手が 2 人いるのも同じ兆候。

#### 13. 永続表に列を足すとき、書くループが脊椎の書き手でなければ衛星表にする

不変条件 12 の寿命チェックは**永続表に対して盲目**になる（表自体が永続なので列も永続で自明に真が返る。`recordings` が書き手 5 人になっても 12 では止まらなかった。経緯は invariants.md）。**recordings 本体は「試行の帰結の観測」だけを持つ脊椎。別のループが書く状態は `recording_id` を FK に持つ衛星表に置く**（`media_assets` が実例）。

チェック: 永続表に列を足すとき「**この列を書くループは脊椎の書き手か**」を問う（`recordings` なら試行を観測する watcher / reconciler。既にその表に書いている＝根拠にならない）。脊椎の書き手でないなら `recording_id` を持つ衛星表にする。

境界（絶対視すると壊す。詳細は [docs/invariants.md](docs/invariants.md)）: `deleted_at` / `superseded_at` は部分一意索引の述語が参照するので本体に置く。番組スナップショット列群は watcher が一度だけ書くので脊椎に属する。

### コーディング規約

- Go 標準プロジェクトレイアウト（`cmd/` + `internal/`）
- ログは `log/slog`
- エラーは握り潰さず `fmt.Errorf("...: %w", err)` で文脈付き wrap
- 各タスクは 1 PR 粒度。着手前に**担当タスクのサブ issue**（本文とコメント）と、そこが参照している doc の節だけを読む。親 issue や他タスクのサブ issue は読まなくてよい（上記「タスクマップ」）
- **doc コメント**: エクスポートされた関数・型・メソッド・定数には [Go Doc Comments](https://go.dev/doc/comment) 規約に従った doc コメントを書く。`// FuncName は〜` の形式で主語を識別子名にする。非公開でも他パッケージから呼ばれうる重要な関数には書く
- **測っていない挙動を断言しない。** コメント・docs で実行時の挙動を書くなら、テスト名か測定値を
  併記する。書けないなら「未検証」と書く。**古い記述より悪いのは、一度も真でなかった記述**。
  実例: 「`<video>` の error イベントに落ちる」（誰も error を聴いていない。#92）/「動的ビルダ
  なら常に具体的なプランになる」（prepared statement 経由で 6 回目に generic plan、0.7ms →
  290ms。#136）/「cleanup を掴む経路を塞ぐために独立させた」（塞いでいない。#185）

### テスト規律

**「テストのないタスク完了はない」（不変条件 8）は「通るテストを書く」ではない。**

- **書いたテストは意図的に実装を壊して落ちることを確認する。** 通るだけのテストは何も保証しない。実際に、生成 SQL を文字列一致で見るだけのテストがタイムゾーンのバグを通してしまった
- **落ち方を確認する。** ビルドエラーで落ちたのはアサーションの検証になっていない（未使用変数で壊すとこれが起きる）。壊し方はコンパイルが通る形にする
- **壊す場所を、実際に壊れる経路の上に置く。** 1 段下の関数を壊して落ちても、実物が通る経路を
  通っていなければ検証になっていない。`newBoundBacklogCollector` の戻り値を `nil` と比較する
  テストは通り続けたが、それを `prometheus.Collector` に渡す 1 段上で型付き nil になり、
  **CI 3 ジョブ緑のまま実バイナリが起動時 panic した**（#183）
- **実装の定数と比較するテストは何も主張していない。** `opts.Queue != cleanupQueue` は定数を
  変えても通る。期待値はリテラルで書く（#185）
- **CI が緑でも実バイナリ・実ブラウザを起動して確かめる。** テストが通っても起動しない／実
  クライアントから使えない類は、これでしか出ない。実例: 上記の起動時 panic（#183）/ プレイ
  リストのセグメント URI が basename で実 HLS クライアントから 404（#91）/ 実 Chrome が HLS の
  MIME に `'maybe'` を返すため全 Chrome ユーザーが再生できない（#92）
- **非同期の空虚な成功に注意する。** 「何も表示されない」系のテストは、クエリが解決する前にアサーションが走って通ることがある。読み込み完了を待つ手段を入れてから、壊して落ちることを確認する
- 分岐を直したら**両方向**で確認する（片側だけ見ると反転しても気付かない）
- **jsdom が測れないもの（レイアウト・スクロール位置・要素の可視性）は、ユニットテストが
  全部通っても何の保証にもならない。** この領域の機能は**実装より先に判定手段を作る** ——
  実ブラウザで機械的に合否が出る形にしてから着手する（`web/e2e/`）。番組リストの遡行では、
  これを怠って「`pnpm test` が通った」を根拠に 3 回リリースし、3 回とも実機で壊れていた。
  壊れ方は毎回違ったが、いずれも jsdom では原理的に検出できないものだった（詳細は
  `web/e2e/README.md`）。足した判定が**直す前の実装で実際に落ちること**も確認する

### 並行作業（複数エージェント）

複数エージェントで並行させるときは [docs/workflow.md](docs/workflow.md) §並行作業を読む。要点: 契約を先に書いてコミットしてから並列に投げる / `openapi.yaml` を触るのは 1 本に限定 / マージ後は生成物を再生成してフルテスト（マージが通ったことは何の保証にもならない）。
