> [docs/api.md](../api.md)（索引）の分割本文。メディア配信（録画バイト配信・サムネイル・ライブ HLS・SPA アセット）の仕様はここが唯一の権威（openapi.yaml には載せない）。

## メディア配信（Range 対応・X-Accel-Redirect オプション）

録画再生・ライブ視聴の HLS プレイリスト/セグメント・サムネイル画像は HTTP GET で配信する。

### 録画済みファイルのストリーミング

Go の `http.ServeContent` は `*os.File` 相手なら sendfile が効き、Range 対応も標準。家庭サーバーの同時視聴数本でギガビット LAN を飽和させるのに問題はなく、**性能を理由とする nginx 導入は不要**。

#### 実装

```
GET  /api/recordings/{id}/file              →  video/MP2T（原本、Range 対応）
HEAD /api/recordings/{id}/file              →  ヘッダーのみ
GET  /api/recordings/{id}/file?profile=h264 →  video/mp4 等（encoded、Range 対応）
HEAD /api/recordings/{id}/file?profile=h264 →  ヘッダーのみ
GET  /api/recordings/{id}/thumbnail         →  image/jpeg
HEAD /api/recordings/{id}/thumbnail         →  ヘッダーのみ
```

**ブラウザ VOD は MP4 progressive + Range とする（HLS ではない）。** 家庭 LAN の
オンデマンド再生では単一ファイル + `http.ServeContent` の Range が十分で、
セグメント化・プレイリスト・hls.js のコストに見合わない。ライブ視聴の HLS は
別経路（下記「ライブ視聴の HLS」）のまま。

**HEAD も登録する。** VLC やブラウザはシーク前に HEAD で `Content-Length` と
`Accept-Ranges` を取るため、405 を返すとシーク再生に失敗しうる。
`http.ServeContent` は HEAD ならヘッダーだけを書くので実装は共通。

**OpenAPI には載せない。** SSE と同じ理由で、生成クライアントは JSON を前提にする
（`customInstance` が `response.json()` を呼ぶ）ためバイナリ配信では誤った
クライアントが生成される。UI は URL を `<video>` の src や `<img>` の src、
保存リンクに直接使い、生成フックを経由しない。守るべきスキーマがないので
生成物から得るものもない。利用可能なプロファイル（名前 + サイズ）は
`Recording.encodedAssets`（一覧 API）で返す。

**`internal/streamer` の所有物として実装する。** api ロールはファイルシステムに
依存しない（不変条件 1）ため、バイト転送はロールとして分ける。monolith では
`api.RouterConfig.Mounter` 経由で同一リスナーに相乗りするが、コードの境界は
最初から引いてある。`--roles streamer` を指定したときだけ登録される。

**`/file` は `profile` クエリが無いときは原本（`kind = 'original'`）、あるときは
`kind = 'encoded'` かつそのプロファイル名。`/thumbnail` はサムネイル
（`kind = 'thumbnail'`）。** ブラウザ UI は encoded を優先し、原本 TS は VLC 等
向けのダウンロードリンクに残す。原本が `until_encoded` で消えた後も派生物だけで
再生できる（アセット解決は kind ごとに独立）。

**`rel_path` は配信側でも独立に検証する。** `internal/mediapath.Resolve` を
ingest と共有し、メディアディレクトリの外を指す `rel_path` は 404 にする。
書き込み時に検証済みでも、DB に不正な行が入った場合に任意ファイルを
読み出させないため片側だけでは足りない。

**配らないもの:** ごみ箱に入った録画（`recordings.deleted_at IS NOT NULL`）、
削除済みアセット（`media_assets.state <> 'active'`）、未 ingest の録画
（指定 kind の `media_assets` 行なし）、存在しないプロファイル。いずれも 404。
コミット（DB 行）はあるのにファイルが無い不整合も 404 にしつつ WARN で記録する
（孤児回収や外部からの削除）。

**`Cache-Control: private, max-age=0, must-revalidate`。** 原本は一度書いたら
変わらないが、ごみ箱からの復元で同じ URL の中身が入れ替わりうるので
`immutable` は付けない。`Last-Modified` による条件付きリクエストは効く。

**`media_assets.size_bytes` と実ファイルのサイズが違えば WARN で記録する。**
size_bytes は ingest / encode 時に照合した値なので、
違うならコミット後に改変・切り詰めが起きている。配信自体は続ける
（ユーザーは録画を見たい）。

**再生位置はサーバーに持たない。** ブラウザの localStorage に録画 ID（+ プロファイル）
をキーにして保存する。視聴履歴テーブルは作らない。

#### X-Accel-Redirect（`storage.accel_location`）

意味があるのは **X-Accel-Redirect パターン**（認可判定はアプリ、バイト転送は nginx）。
設定フラグ + レスポンスヘッダー 1 個で対応でき、アプリ側コストがほぼゼロなので
オプションとして実装する（Mastodon / GitLab の X-Sendfile 対応と同じ位置づけ）。
性能を理由に必要なわけではなく、既に nginx が前段に居る構成で転送を任せられる、
という位置づけ。

```yaml
storage:
  media_dir: /mnt/media
  accel_location: /_media/     # 空なら Go が直接配る
```

有効時は本文を返さず `X-Accel-Redirect: /_media/<相対パス>` を返す。nginx 側は
`internal` な location で `media_dir` を alias する。

- **パス検証を通した後にヘッダーを返す。** 検証前に返すと、細工された `rel_path` で
  nginx に任意ファイルを配らせられる
- **値は URI として解釈されるのでパス要素を URL エスケープする。** 番組名由来の
  ファイル名には空白・括弧・日本語が入る
- Range の扱いは nginx 側に移る（`Accept-Ranges` も nginx が付ける）

### ライブ視聴の HLS --- アプリ配信を維持

ライブセッションはインメモリの使い捨て状態（全体アーキテクチャの crash-only 例外）で、「クライアントがいなくなったら ffmpeg を止める」idle GC が要る。セグメント要求がアプリを通れば last-access の更新がタダで手に入るが、nginx が scratch から直接配るとアプリはクライアントの生存を見失う。`auth_request` やログ監視で回収はできるが、セグメントは数 MB で転送負荷が軽く、複雑さに見合わない。**streamer ロールのアプリ配信のまま**とする。

**`live.enabled: false` ならこれらのルートは登録されず、404（JSON）になる。**
SPA フォールバックには落とさない（[rest.md](rest.md)「機能の有効/無効は能力 API で
観測する」。落とすと「無い」が HTML の 200 になり、probe するクライアントが成功と
誤認する。issue #209）。導線そのものを出さない判断は `GET /api/capabilities` 側。

#### 資源同定: セッション ID を持たない

プレイリストとセグメントの URL は **`/api/sites/{site}/services/{serviceId}/live...`**
の形にし、**セッション ID を URL にもクッキーにも置かない**。ライブセッションは
サービスに対して 1 つで、同じサービスを見ている視聴者はそれを共有する。

- **チューナーが共有される。**別の部屋で同じチャンネルを見ても ffmpeg 1 本・
  チューナー 1 本で済む。チューナーは録画と取り合う唯一の共有資源なので、これが
  一番効く
- **スケールアウトの鍵が既に資源同定の中にある。**`(site, serviceId)` は前段の
  consistent hash の鍵にそのまま使えるので、streamer のレプリカを増やしても
  URL・クライアント・API は変わらない（[operations.md](../operations.md) §5
  「streamer のスケール」。URL を固定深さにする制約もそこに書いてある）
- **セッションが消えても URL が死なない。**Pod 死・ハッシュの担当移動・idle GC の
  後でも、同じ URL への再要求が新しいセッションを起こす。「セッション ID を握った
  クライアントが 404 で詰む」経路が存在しない

セッション ID を持つ設計（`POST` でセッションを作って ID 付きの URL を配る）は、
**導出物の identity を宛先にする**形になる（不変条件 9 の identity 系）。ライブ
セッションは使い捨ての導出物なので、宛先は「このサービスが見たい」という欲求の側で
名指しする --- レベルトリガー（不変条件 5）と同じ形である。

帰結として **idle GC の粒度もサービス単位**になる（そのサービスへのセグメント要求が
一定時間来なければ ffmpeg を止める）。「クライアント 1 人ごとの生存」は追わない。

**プロファイルはクエリ（`?profile=`）で受け、ハッシュ鍵には入れない。**鍵に入れると
同じサービスを別プロファイルで見たときに 2 つの Pod に割れ、チューナーを 2 本掴む。
1 つの Pod の中で 1 チューナーから複数プロファイルを出す。

#### 実装（`internal/streamer`）

```
GET /api/sites/{site}/services/{serviceId}/live/playlist.m3u8[?profile=<name>]
      → application/vnd.apple.mpegurl
GET /api/sites/{site}/services/{serviceId}/live/segments/{name}
      → video/mp2t
```

- **DB を引かない。**パスの `serviceId` は検証せずそのまま mirakc の
  `GET /api/services/{id}/stream?decode=1` に渡す（不明な id は mirakc が拒否する）。
  ここでの `serviceId` は EPG 射影の SI `serviceId` ではなく **Mirakurun 合成
  service id**（`networkId * 100_000 + serviceId`。`internal/mirakc.ServiceID` と
  同じ規則）。フロントが URL を組み立てるときに合成する（`web/src/lib/live.ts`）。
  ライブセッションはインメモリの使い捨てで、認可はリバースプロキシ委譲、同時上限も
  プロセスローカルなので、DB を引く理由が無い
- **トランスコードは必須。**ISDB-T 地上波の映像は MPEG-2 で、ブラウザの HLS 経路
  （hls.js/MSE）は事実上再生できない。mirakc フィルタ + `-c copy` では受信端末を
  満たせないため、ffmpeg で H.264/AAC に変換する（`live.profiles`、
  [configuration.md](../configuration.md) 参照）
- **`profile` クエリが空なら `live.profiles` の先頭を既定として使う。**セグメント
  URL 自体は `?profile=` を持たない --- ffmpeg が書き出すファイル名にプロファイル名を
  接頭辞として焼くため、プレイリストが指す相対パスだけで一意に解決できる
- **1 サービス = 1 ffmpeg プロセス = mirakc の 1 チューナー。**設定済みの全プロファイルを
  1 回の ffmpeg 起動で同時に出す（見られていないプロファイルの CPU も使うトレードオフ
  はあるが、プロファイルを跨いだ ffmpeg の使い分けを実装しない分シンプルになる）
- **チューナー調停は mirakc のリクエスト優先度に一元化する。**ライブの GET には
  `live.tuner_priority`（既定 1）を `X-Mirakurun-Priority` に載せる。ruler が生成する
  schedule の既定 priority（10）より低く保つことで、チューナー枯渇時に mirakc が
  録画側を常に勝たせる（[recording.md](../recording.md) §2「チューナー調停」）。
  予約表を見て拒否する案は採らない --- streamer が予約エンジンに依存し、mirakc 固有の
  優先度概念を永続テーブルに持ち込む誘惑を生む（不変条件 7）。**`live.tuner_priority <
  rules.priority` を Rokuban は強制しない。** 前者は config、後者は DB でユーザーが
  自由に変えられる値で、両者を跨いで検証する権威がどちらの層にも無い（config は
  デプロイ環境の性質、DB は運用中の意思。[configuration.md](../configuration.md) §config
  と DB の境界）。ルールの priority を既定 10 未満まで下げると、この既定値のままでは
  ライブが録画に勝ってしまう --- 運用者が両方の値を意識して選ぶ前提とする
- **同時セッション上限（`live.max_sessions`）はプロセスローカル。**超えた要求・
  mirakc 側のチューナー枯渇はいずれも既存セッションを壊さずに 503 を返す
  （エラーの本文はプレーンテキスト。OpenAPI 対象外のため生成クライアントの契約は無い）
- **idle GC はサービス単位。**セグメント要求（プレイリスト取得も含む）ごとに
  last-access を更新し、`live.idle_timeout` の間要求が来なければ ffmpeg と mirakc への
  接続を止める。クライアント 1 人ごとの生存は追わない
- **セグメントは `live.segment_dir`（tmpfs 前提）に書く。**録画バッファとは別ディスク
  （[operations.md](../operations.md) §5「ライブのセグメントを録画バッファと同じディスクに
  置かない」）。プロセス終了（`--all`/`--roles streamer` の SIGTERM）時は idle GC と同じ
  経路で全セッションを止め、ディレクトリも削除する。**tmpfs はノード再起動でしか
  消えない**（k8s の `emptyDir: {medium: Memory}` はコンテナ / Pod の再起動をまたいで
  残る）ため、SIGKILL によるクラッシュ（SIGTERM が効かない）の後始末はそれだけでは
  終わらない --- 起動時（`NewLive`、HTTP リスナーが立つ前）に `live.segment_dir` の
  **中身**を掃くことで、前回プロセスの残骸を毎起動で必ず消す。**`segment_dir` 自体は
  消さない** --- `emptyDir` を `segment_dir` に直接マウントする構成では、Linux は
  マウントポイント自体への rmdir を EBUSY で拒むため（詳細は
  [configuration.md](../configuration.md) §live）
- **ffmpeg の LookPath 検査は `live.enabled: true` のときだけ行う。**公式イメージ
  （ffmpeg 無し）で streamer ロールを起動する構成（録画配信 / サムネイルのみ）を
  壊さない

### SPA アセット配信

go:embed 配信でハッシュ付きアセット immutable + それ以外 no-cache のヘッダーを正しく付ければ十分（参照: [frontend.md](../frontend.md)）。本気の配信最適化は S3+CDN 経路の仕事。ここに nginx キャッシュを挟むと配信経路が 3 つになり、テストマトリクスが増える割に得るものがない。

### サービスロゴ: ドロップ

mirakc は起動中の局ロゴ抽出をサポートせず、運用者が事前抽出したファイルを静的登録して配るだけの機構しか持たない。Rokuban 側で再取得・自前配信する価値が薄いため実装しない（[data.md](../data.md) の「サービスロゴ: ドロップ」参照）。

## 経緯と失敗事例

- 原本配信は M1-8、派生物（encoded / thumbnail）は M3-4 / M3-5 の成果物。再生位置を
  localStorage に置く決定は issue #14 7c
- **ライブのセッションレス資源同定**は issue #56 の決定。実装は M4-3（issue #91。
  「DB を引かない」の判断は着手前コメント）。Mirakurun 合成 service id をフロント側で
  合成する形は issue #208
- **tmpfs の後始末**: 当初この doc は「tmpfs はコンテナ再起動で消える」前提で書かれて
  いたが誤りで（ノード再起動でしか消えない）、レビュー指摘で起動時スイープに直した
  （`internal/streamer/live.go` の `NewLive` のコメント参照）
- ごみ箱（`deleted_at`）を 404 にする契約は削除エンジン（M3-7 / issue #69）、
  `purged_at` の tombstone は issue #135
