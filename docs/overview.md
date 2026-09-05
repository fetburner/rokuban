# 全体アーキテクチャ

Rokuban（録番）は、クラウドネイティブに設計された録画サーバー。EPGStation の漸進的改善ではなく、ゼロベースで再設計する。

## 背景: なぜ EPGStation の改修ではないのか

EPGStation のコードを調査した結果、クラウド/k8s ホスティングを阻む構造的な問題が4つある：

1. **プロセス間通信が Node.js の `child_process` IPC**（Operator / Service / EPGUpdater の3プロセス構成）。ネットワーク越しに分離できない
2. **状態がほぼ全部インメモリ**。エンコードキューはプロセス再起動で全消失、録画スケジュールは `setTimeout` 保持
3. **共有ファイルシステム前提**。録画・サムネイル・HLS セグメントを全プロセスがローカルパスで読み書き
4. **シングルライター前提**。排他制御がプロセス内 Promise キューのみで、多重起動すると二重録画する

加えて EPGStation は公式にメンテナンスモードであり、変則編成（野球延長・イベントリレー等）での録画失敗が知られている。

## 基本方針

**「録画（リアルタイム・ハードウェア依存）はエッジの mirakc に、それ以外はサーバー側で弾力的に」**

mirakc に録画を委譲すると（詳細は [recording.md](recording.md) 参照）、サーバー側に残る仕事は性質の異なる3つに分解できる：

1. **コントロールプレーン** --- ルール評価 → 予約（desired state）生成 → mirakc への宣言的同期。k8s コントローラと同型
2. **メディアパイプライン** --- 録画完了イベント → 取り込み → エンコード → サムネイル → 公開。イベント駆動のジョブ処理
3. **ライブラリ/UI** --- 番組表検索、録画一覧、再生。読み取り中心の Web アプリ

重要な帰結として、**Rokuban は TS パケットを1バイトも触らない**。ストリーム処理は mirakc が、変換は ffmpeg がやる。バックエンドは純粋なオーケストレーションと I/O。

> **補足**: 「TS パケットを1バイトも触らない」の本意は「ストリーム処理（録画・demux・変換）をしない」ということであり、ingest 中の読み取り専用の統計採取（PID 別 continuity counter 不連続 / TEI / scrambling_control）は例外とする。詳細は [recording.md](recording.md) の ingest パイプラインを参照。

## 構成図

```
[エッジ]                       [サーバー / クラウド]
┌────────────┐    ┌─────────────────────────────────────────┐
│ mirakc      │◀──▶│ rokuban（Go、単一バイナリ）                  │
│             │    │  ├ api:        REST、UI 配信(go:embed)        │
│             │    │  ├ notifier:   NOTIFY 購読→ブラウザへ SSE 配信 │
│             │    │  ├ watcher:    mirakc SSE購読→状態反映       │
└────────────┘    │  └ streamer:   ライブ視聴 (mirakc→ffmpeg→HLS)│
   ▲               │ rokuban worker（別イメージ、0〜Nスケール）     │
   │record pull    │  └ ingest / encode / thumbnail / cleanup /  │
   │               │    ruler_pass / reconcile_pass / epg_sync   │
   └───────────────├─────────────────────────────────────────┤
                   │ PostgreSQL（唯一のステートフル基盤）           │
                   │ ファイルシステム（クラウドでは CSI で S3）       │
                   └─────────────────────────────────────────┘
```

nginx は構成図上の「箱」ではなく、推奨デプロイパターンの一部として位置づける（後述）。

## 単一バイナリ、ロールで分割

この種の OSS の現実のユーザーの大半はミニ PC や自宅サーバー1台で動かす。マイクロサービス前提の設計はその層を切り捨てる。そこで Loki / Tempo と同じ **monolithic mode / distributed mode** を最初から設計に入れる：

- `rokuban server --all`: 全ロールを1プロセスで（自宅向け、Docker Compose で Postgres と2コンテナ）
- k8s ではロールごとに Deployment を分割：api は水平スケール、worker はキュー長で 0〜N（KEDA）、watcher はシングルトン（Postgres アドバイザリロックでリーダー選出）

### ロール分類の基準: ソケットを持ち続けるか

**ロールは「プロセスの形」を表し、「どの仕事をするか」は表さない。** そして「形」を決める判定はひとつだけ ---
**そのプロセスがソケットを持ち続ける必要があるか**。

| ソケット | 形 | ロール | スケール |
|---|---|---|---|
| listen し続ける | サーバー | api / notifier / streamer | 水平（api は scale-to-zero 可） |
| connect し続ける | サーバー（外向き） | watcher（mirakc の SSE） | シングルトン |
| 持たない | ジョブ | worker | キュー長で 0〜N（KEDA）または CronJob |

> **この基準は後から来た。** 初版は分類の軸を「どの仕事をするか」に取っており、`ruler` / `reconciler` / `watcher` をシングルトンロールとして並べていた。基準を明文化した結果、ruler / reconciler / record_sweep は worker のジョブになり、`/api/events` は notifier に分かれた --- **形を先に固定して判定基準を後から書いた**代償（不変条件 11。[invariants.md](invariants.md) §11）。

この基準を採る理由は、**ソケットの寿命がプロセスの寿命の下限を決めてしまう**こと。接続を張り続ける必要があれば
そのプロセスは 0 にスケールできず、逆に仕事が始まってから終わるまでで完結するなら 0 にスケールできる。
デプロイ形態（Deployment か Job か、KEDA でゼロスケールできるか）はここで決まり、これ以外の観点
（DB を触るか、mirakc を触るか、周期実行か）はデプロイ形態を変えない。

帰結:

- **キュー駆動で 0〜N の形をした仕事はすべて worker ロール。** どのキューを引くかはデプロイ時のパラメータであって
  ロールではない。ルール評価（ruler）・reconciler の同期パス・watcher の定期全量突き合わせ（record_sweep）も
  ここに含まれる。キューごとにロールを増やすと `ingester` / `encoder` /
  `thumbnailer` と際限なく増える
- **周期実行はロールの根拠にならない。** 「定期的に動く」はスケジューラの仕事で、Docker では River の
  `PeriodicJobs`、k8s では CronJob が投入する。同じジョブ本体が両方から呼ばれる（[data.md](data.md) §2）
- **SSE を扱うことはロールの根拠になる**（ソケットを持ち続けるので）。ただし SSE だからといって
  1 つのロールに集約する理由はない --- notifier（ブラウザへ送る）と watcher（mirakc から受ける）は
  機構が同じでも相手・向き・障害時の影響範囲が無関係なので、別ロールのまま置く（[api.md](api.md) §SSE）

コード上はただの modular monolith。**IPC は最初から作らない**ので、EPGStation が抱えた「分離しようにも IPC が剥がせない」問題は構造的に発生しない。

ロールの詳細と分散デプロイ時の構成については [operations.md](operations.md) を参照。

## 設計原則

### レベルトリガー

すべてのループは「現在の desired と observed の差分を消す」だけ。イベント（SSE / NOTIFY）は高速化のヒントであり、取りこぼしても定期 reconcile で収束する。

### crash-only

すべてのコンポーネントは、どこで落ちても再起動すれば収束する。インメモリの必須状態を持たない。唯一の例外はライブ視聴セッション --- 使い捨て状態なので落ちたらクライアント再接続で済む。

### 冪等

ジョブは at-least-once 実行を前提に冪等に書く。詳細は [data.md](data.md) の River（ジョブキュー）の節を参照。

## HTTP 配信層と nginx

**nginx は必須コンポーネントにしない。ただし「前段に置ける設計」を HTTP 層の要件として最初から織り込む。** `--all` で nginx なしでも全機能動作する自己完結性は維持しつつ、リバースプロキシ・フレンドリー性（`X-Forwarded-Host` 解釈（opt-in）、ルート相対パス徹底、WebSocket 不使用）と X-Accel-Redirect オプションを要件化する。

用途別の判断、リバースプロキシ要件一覧、nginx リファレンス構成は [api.md](api.md) を参照。

## 認証

**Rokuban はアプリ内に認証・認可機構を一切持たない。** 日本の著作権法上、放送の録画が適法なのは私的使用（30条）の範囲内であり、Rokuban は構造的に単一世帯用アプリである。ユーザーアカウント・マルチテナント・共有リンクは将来も含めてスコープ外。認証が必要な構成ではリバースプロキシに委譲する。アプリ内に残る唯一のセキュリティ要件は DNS rebinding 対策の Host ヘッダー検証のみ。

法的根拠、帰結の詳細は [api.md](api.md) を参照。

## イメージ戦略と配布物

**公式配布物は ffmpeg を含まないコンテナイメージと素のバイナリの 2 つだけ。** ffmpeg 入りイメージはユーザーが同梱の `Dockerfile.full` で自分用にビルドする（自分のためのビルドは再配布ではないので GPL 遵守事務・特許プールの問題が生じない）。

ベースイメージは **`debian:bookworm-slim` + `ca-certificates` / `curl`**（`Dockerfile`。実行ユーザーは `nobody`）。同梱するのは `curl` 1 つだけで、これは Docker Compose の healthcheck が HTTP を叩く手段を必要とするため（`docker-compose.yml` の `curl -sf .../healthz`）。**distroless/static に置き換えると、そのまま healthcheck が壊れる。** 攻撃面を縮めたいという動機は正しいが、それは compose の healthcheck を別の手段に作り替える判断と一緒でなければ成立しない（Go 側に自己チェック用のサブコマンドを持たせる等）。イメージだけ差し替えるのは変更にならない。

この判断を支える規律として、**ffmpeg / ffprobe の exec は worker と streamer ロールに閉じ込める**。api ロールが ffmpeg を要求した瞬間、公式イメージだけで動く構成（サーバーレス api 含む）が壊れる。「どのロールが ffmpeg を要求するか」がそのまま配布物の境界になっている。

## サーバーレスデプロイとハイブリッド構成

レベルトリガー設計の帰結として、**予約・ルールの作成/編集（書き込み）すら DB-only** である。API は desired state を Postgres に書くだけで、mirakc への同期は reconciler が非同期に収束させる。したがって api ロールはサーバーレス（Cloud Run / Lambda 等）で scale-to-zero 可能であり、これは新要件ではなく既存設計が保証する性質。

SSE (`/api/events`) は notifier ロールに分離されており、api は mirakc にもファイルシステムにも長寿命接続にも依存しない純粋なリクエスト/レスポンス層である。scale-to-zero は実際に機能する。**SSE を api ロールに置いてはならない** --- 振り分け先が api 自身になるので、api を scale-to-zero にできなくなる。

推奨のハイブリッド構成:

```
[クラウド]  Postgres (Neon 等) + api (公式イメージ, scale-to-zero) + notifier (常駐) + CDN/Access
[自宅]      mirakc + watcher/worker/streamer (full) — 外向き接続で DB を見る
```

`worker` が自宅側にあるのは、**キューの置き場所の制約**であってロールの都合ではない。mirakc への到達性を
要するキュー（`reconciler` / `epg` / `watcher` / `ingest`）とアーカイブのファイルシステムを要するキュー
（`encode` / `thumbnail` / `cleanup`）は自宅側、DB しか触らない `ruler` はどちらでも動く。
ロールを増やさずキューの割り当て（`worker.queues`。デプロイ時のパラメータ）だけで置き場所が決まるのは、
ロールが「プロセスの形」だけを表しているため。

キュー × 置き場所 × site 軸の表（キュー名の site 修飾を含む）は [operations.md](operations.md) §5 を参照。

自宅サーバーが落ちていても番組表・録画一覧・予約操作ができ（DB に積まれ、復帰後 reconciler が収束）、メディア視聴だけは自宅到達が必要と割り切る。SSE は長寿命接続なのでサーバーレスには乗せず、CDN のパスルーティングで `/api/events` だけ notifier ロールへ振り分ける（詳細: [api.md](api.md)）。notifier は mirakc への到達性を必要としない（Postgres の NOTIFY を配るだけ）ため、クラウド側に常駐プロセスとして置いても自宅側に置いても成立する。

サーバーレスの置き場所の選定、ハイブリッド構成の運用詳細は [operations.md](operations.md) を参照。

## mirakc への依存の受容

### 決定: 受容する

エンジン抽象化も EDCB ドライバも作らない。mirakc は単一メンテナ OSS であり、スキーマ自体を mirakc の形（programId / RecordingOptions / tags）に合わせたため「継ぎ目は reconciler/watcher に局所化」という保険は言うほど厚くない。それでも受容できるのは、賭け金が見た目より小さいためである。

### 爆風半径は有界（意図せず手に入っている担保）

mirakc の形をしているのは**短命な導出状態だけ**：

| 分類 | 内容 |
|---|---|
| **mirakc 非依存（永続・本当の資産）** | ルール / 録画履歴 / media_assets / ドロップ統計 / tombstone / overrides --- ライブラリの全部 |
| **mirakc の形（短命・導出）** | reservations の base（ルールから毎回再計算）、schedule_sync（observed、エンジンから再同期） |
| **外部識別子（API の資源同定という長寿命の契約）** | `programId`。`(site, programId)` が API のパスとクライアントのクエリキー階層に入り、`docs/schema/principles.md` §1-5 の資源同定そのものになった。爆風半径としては上の「短命・導出」より一段重い —— クライアントのブックマーク・キャッシュキーという、DB のマイグレーションより差し替えコストが高い場所に染み出している |

**この行の自己評価**: `programId` は mirakc 固有の連番 ID ではなく、NID（ネットワーク識別）/SID（サービス識別）/EID（イベント識別）という ARIB（地上波・BS/CS 放送の標準規格）由来の値から合成される（Mirakurun 互換の合成規則）。エンジンを載せ替えても放送波そのものの NID/SID/EID は変わらないため、新エンジンの上に同じ合成規則（または新エンジンが提供する同値の識別子）を被せれば `programId` を再合成できる。つまりこの依存は「mirakc というプロセスの内部 ID に縛られている」のではなく「ARIB という止まらない外部標準に縛られている」ため、実際の載せ替えコストは表の見た目（API 契約への浸透）ほど高くない。

エンジンを載せ替える場合でも、書き直すのは reconciler / watcher / ingest の取得部と予約まわりのスキーマだけで、**ライブラリと履歴は無傷で持ち越せる**。desired/observed 分離とレベルトリガーの帰結。

### 保険（すべて安価）

1. **移行判断のトリガーは設置済み**: recording.failed 理由別集計 / record-broken / ドロップ統計 / 開始遅延検出器 / scrambled カウンタ。「品質問題が実測されたら再検討」の観測基盤は既存決定で完了している。詳細は [recording.md](recording.md) の録画品質実測の節を参照
2. **規律: mirakc 固有の概念を永続テーブルに漏らさない**。rules / media_assets / 履歴に mirakc 固有フィールドを持ち込まないことを明文化し、爆風半径の有界性を維持する
3. **pin & fork が現実的な最終手段**: mirakc は Apache-2.0。放送 TS フォーマットとチューナードライバは変化が極めて遅いドメインで、上流が止まっても「動いているバージョンに固定して数年運用」が成立する

「マージンなし・PSI/SI 追従にユーザー側の回避ノブがない」点は残るが、これは mirakc の設計思想への同意そのものであり、ノブを足すことは思想と矛盾する。品質メトリクスが問題を示したときに初めて再検討する。

## 関連ドキュメント

- [recording.md](recording.md) --- 録画エンジン: mirakc への委譲と予約同期
- [data.md](data.md) --- データ層: PostgreSQL 一本化（ブローカーレス設計）
- [storage.md](storage.md) --- メディアストレージ: ファイルシステム契約
- [api.md](api.md) --- API 設計
- [frontend.md](frontend.md) --- フロントエンド
- [configuration.md](configuration.md) --- 設定
- [operations.md](operations.md) --- 運用・デプロイ
