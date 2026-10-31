> [media.md](../media.md) の録画ファイル配信。索引から辿る

## メディア配信（Range 対応・X-Accel-Redirect オプション）

録画再生・ライブ視聴の HLS プレイリスト/セグメント・サムネイル画像は HTTP GET で配信する。

### 録画済みファイルのストリーミング

録画済みファイルは Go の標準ライブラリの配信関数で配る。ファイル相手なら sendfile が効き、Range 対応も標準である。家庭サーバーの同時視聴数本でギガビット LAN を飽和させるのに問題はなく、**性能を理由とする nginx 導入は不要**。

<details><summary>根拠</summary>

- 標準の配信関数: `http.ServeContent`
- sendfile が効く相手: `*os.File`

</details>

#### 実装

録画のバイナリは streamer ロールが Range 対応で返す。HEAD も登録し、OpenAPI には載せない。

URL は録画 API の `Recording.media` が返す。UI はそれをそのまま使い、自分では組み立てない。

原本とコマは置き場の URL で、その置き場をマウントした streamer だけが配る。派生物（encoded・サムネイル・シークタイル）は置き場を含まない URL で、中央の FS なら streamer が配り、S3 なら 302 で署名付き URL へ送る。

```
GET  /api/media/stores/{store}/recordings/{id}/file              →  video/MP2T（原本、Range 対応）
HEAD /api/media/stores/{store}/recordings/{id}/file              →  ヘッダーのみ
GET  /api/media/recordings/{id}/file?profile=h264 →  video/mp4 等（encoded、Range 対応）
GET  /api/media/recordings/{id}/file?profile=h264&track=subtitles → text/vtt（字幕サイドカー）
HEAD /api/media/recordings/{id}/file?profile=h264 →  ヘッダーのみ
GET  /api/media/recordings/{id}/thumbnail         →  image/jpeg
HEAD /api/media/recordings/{id}/thumbnail         →  ヘッダーのみ
GET  /api/media/recordings/{id}/seek-tiles        →  image/jpeg（シークプレビュー用の格子画像）
HEAD /api/media/recordings/{id}/seek-tiles        →  ヘッダーのみ
```

##### 外向きの形と再生方式

配信 URL は、応答の性質を表す固定接頭辞の下に置く。active な encoded のブラウザ再生は単一ファイルの Range で足り、原本だけが残る完了録画は一時 HLS、ライブ視聴は別経路である。

録画配信は一覧・詳細 API と `/api/recordings/{id}` の部分木を共有する。`{id}` は要求時まで列挙できないので、その後ろで api と streamer が分かれる。そのため標準 Ingress の `Exact` / `Prefix` だけでは、単一ホスト名から一意に振り分けられない。分割可能な外向きの形として、固定接頭辞に移設した。派生物は `/api/media/recordings/{id}/...`、原本は置き場ごとの `/api/media/stores/{store}/recordings/{id}/...` である。メソッド（`GET` / `HEAD`）とクエリは変えない。

録画 API は、active な原本のコピーがある置き場（複数あれば置き場名順で先頭）を使って URL を返す。コピーが無ければ原本とコマの URL を省く。置き場の移動（転送と削除）で原本の置き場が変わると URL も変わるので、クライアントは URL を保存しない。

active な encoded のブラウザ VOD は MP4 progressive + Range を使う。家庭 LAN のオンデマンド再生では、単一ファイルの Range が十分である。原本だけが残る完了録画は一時 HLS でブラウザ再生する（[playback.md](playback.md)「録画原本のブラウザ再生」）。ライブ視聴の HLS は別経路（[live-hls.md](live-hls.md)「ライブ視聴の HLS」）のままである。

##### 転送を持つロール

メディアのバイト転送は streamer ロールが所有する。api ロールはファイルシステムに依存しないので、転送は別ロールに置く。monolith では同じ HTTP リスナーに相乗りし、streamer が有効なときだけ登録する。

<details><summary>根拠</summary>

- メディア配信のロール境界と monolith での登録: `internal/streamer`、不変条件 1、`api.RouterConfig.Mounter`。`--all` でも streamer が有効なときに登録する

</details>

##### HEAD と OpenAPI の扱い

HEAD も GET と同じく登録する。VLC やブラウザがシーク前に長さと Range 対応の有無を HEAD で取るからである。OpenAPI には載せない。生成クライアントは JSON を前提にするので、バイナリ配信には合わない。

VLC やブラウザが HEAD で取るのは `Content-Length` と `Accept-Ranges` で、405 を返すとシーク再生に失敗しうる。標準の配信関数は HEAD ならヘッダーだけを書くので、実装は共通である。

SSE と同じ理由で、バイナリ配信に生成クライアントを使うと誤ったクライアントになる。UI は URL を `<video>` の src や `<img>` の src、保存リンクに直接使い、生成フックを経由しない。守るべきスキーマがないので、生成物から得るものもない。利用可能なプロファイル（名前 + サイズ）は `Recording.encodedAssets`（一覧 API）で返す。

<details><summary>根拠</summary>

- 生成クライアントが JSON を前提にする根拠: `customInstance`、`response.json()`

</details>

##### 録画 API が返す録画 HLS の URL

録画 API は、再生可能なプロファイルとルート相対 URL を返す。配信自体は OpenAPI に載せず、URL を返す応答の JSON だけを契約にする。録画 HLS を配る経路はまだ無いので、壊れた URL を公開しないよう、公開は既定で無効にする。

URL の世代は、active な原本の行があればその世代を使い、無ければ転送中の一時ファイルの世代を使う。録画の状態ではなく DB の行だけで決める。api ロールはファイルに触れず、コミットは DB の行だからである。録画中の世代は commit で原本の行へ持ち越すので、録画が終わっても URL は変わらない。バイト列の世代を URL に含めることで、再取り込み後の内容と以前のキャッシュを区別する。

URL には site ではなく置き場の名前を入れる。規則しだいで 1 つの site の原本が複数の置き場に分かれるからである。置き場の名前は、録画 HLS が読む入力（active な原本のコピー、無ければ ingest temp）がある置き場である。ingress は Prefix `/api/media/stores/<名前>/` で、その置き場をマウントした Pod（録画 HLS と原本の配信）へ振り分ける。置き場が 1 つの時から最終形の URL で作るので、2 つ目の置き場を足すときも URL の形は変わらない。

録画 HLS を配るプロセスは 1 つの置き場だけを読む。録画 HLS を有効にしたプロセスが複数の置き場をマウントするときは、起動時に拒否する。`--no-recording-hls` を付けたプロセスは除く（`cmd/rokuban/server.go`）。この拒否を直接見るテストは無いので、未検証である。

録画 HLS の profile は `live.profiles` をそのまま使う。`live.enabled` が false でも、録画 HLS を有効にしたときは `live.profiles` が必須で、名前は URL の path segment に使えるものだけを許す。

<details><summary>根拠</summary>

- 応答と空配列の条件: `openapi.yaml` の `Recording.hlsPlayback`
- 公開設定と置き場名: `config.example.yml`
- 一覧・詳細の世代選択と非公開条件: `TestRecordingsHLSPlayback`
- profile 名の検証と空の `live.profiles` の拒否: `TestLoadRecordingHLS`

</details>

##### 返すファイルの選び方

プロファイルの指定が無ければ原本を、あれば指定したプロファイルのエンコード済みファイルを返す。種別ごとに独立に解決するので、原本が消えた後も派生物だけで再生できる。ブラウザ UI は encoded を優先して再生し、原本の TS は VLC 等向けのダウンロードリンクに残す。

`/file` は `profile` クエリが無いときは原本、あるときはそのプロファイル名の encoded を返す。`/thumbnail` はサムネイルを返す。アセットの解決は種別ごとに独立しているので、原本が `until_encoded` で消えた後も派生物だけで再生できる。

<details><summary>根拠</summary>

- `/file` が原本を返す条件: `kind = 'original'`
- `/file?profile=` が返すアセット: `kind = 'encoded'` かつそのプロファイル名
- `/thumbnail` が返すアセット: `kind = 'thumbnail'`

</details>

##### 字幕サイドカー

字幕を出すプロファイルでは、字幕を MP4 / MKV に内蔵せず、WebVTT のサイドカーとして別に配る。字幕が無い番組ではサイドカーは作られず、映像エンコードは成功する。一覧 API は字幕の有無を返さず、再生側が無条件に要求して、欠損なら 404 になる。

エンコードプロファイルに `subtitles: webvtt` を指定した場合、字幕は encoded ファイルと同じ basename の `.vtt` として保存する。配信 URL は `/file?profile=<name>&track=subtitles` である。サイドカーは独立した行を持たないため、encoded 行が active であることと隣接ファイルの存在を、配信時に確認する。

一覧 API は、プロファイルが字幕サイドカーを持つかを返さない。`<track>` は再生側が無条件に描画し、サイドカーが無ければ 404 を返すだけにする。字幕を隠したい要求が出るまで、字幕の有無を表す能力フィールドは作らない。この非対称の結果として、字幕を使っていない全 encoded 再生でも、サイドカー欠損の 404 が定常的に発生する。ただしこれはコミットと実ファイルの不整合を示す WARN では扱わない。配信側はサイドカーかどうかを知っているので、その経路だけログを出さない。

<details><summary>根拠</summary>

- サイドカーが独立行を持たない表: `media_assets`
- 返さない能力フィールド: `hasSubtitles`

</details>

##### 配らないものと配信時の検証

ごみ箱に入った録画、削除済みのアセット、未 ingest の録画、存在しないプロファイルは配らず、いずれも 404 にする。相対パスも配信時に独立に検証し、メディアディレクトリ外を指す値は 404 にする。

コミット（DB 行）はあるのにファイルが無い不整合も 404 にしつつ、WARN で記録する（孤児回収や外部からの削除）。相対パスは、書き込み時に検証済みでも、DB に不正な行が入った場合に任意ファイルを読み出させないため、配信時の検証も要る。アセットのサイズと実ファイルのサイズが違えば WARN で記録する。サイズは ingest / encode 時に照合した値なので、違うならコミット後に改変・切り詰めが起きている。配信自体は続ける。ユーザーは録画を見たいからである。

<details><summary>根拠</summary>

- 配らない録画: `recordings.deleted_at IS NOT NULL`
- 配らないアセット: `storage.stores` の FS 置き場（FS 置き場は今 1 つだけ。[置き場の規則](../../configuration/keys.md#置き場の規則)）の `media_asset_copies.state <> 'active'`
- 未 ingest の判定: 指定 kind の `media_assets` 行なし
- ingest と共有するパス検証: `internal/mediapath.Resolve`
- 相対パスの列: `media_asset_copies.rel_path`
- 配信時に照合するサイズ: `media_assets.size_bytes`

</details>

##### キャッシュ制御

キャッシュは毎回再検証させ、変更不可の指定は付けない。原本は一度書いたら変わらないが、ごみ箱からの復元で同じ URL の中身が入れ替わりうるからである。

応答は `Cache-Control: private, max-age=0, must-revalidate` にする。`immutable` は付けない。`Last-Modified` による条件付きリクエストは効く。

##### シークプレビュー用タイル

シークプレビュー用のタイルは 1 枚の JPEG で返す。未生成なら 404 で、クライアントは poster だけの表示に戻る。

タイルは 10 秒間隔、10 列で並び、最後の行の余りは黒で埋める。間隔・画像サイズ・列数・上限は worker とクライアントで同じ固定値を使う。メディア配信は `openapi.yaml` の対象外なので、worker から UI へ値を渡す経路はない。値を変えたときは既存タイルを再生成する。

要求中に生成を待たず、サムネイル生成の定期 reconcile で作る。タイルの寿命は poster と同じである。原本が消えた後も派生物として残り、ごみ箱では 404、完全削除でアセットグループごと消える。

<details><summary>根拠</summary>

- `/seek-tiles` が返すアセット: `kind = 'seek_tiles'`
- クライアント側の固定値: `web/src/lib/seek-tiles.ts`
- 定期生成: `thumbnail_reconcile`

</details>

##### CM 検出のロゴ位置用のコマ

CM 検出のロゴ位置を教えるために、原本の指定時刻のコマを JPEG で返す。原本だけを使い、縮小や SAR の焼き込みはしない。

```
GET /api/media/stores/{store}/recordings/{id}/frame?at=<milliseconds> → image/jpeg
```

`X-Coded-Width` / `X-Coded-Height` は、`at` のコマで返す JPEG 自身の幅と高さを返す。途中で映像の幅と高さが変わり、渡した枠が録画の最初の映像ストリームと異なる場合、検出は失敗する。

`X-Sample-Aspect-Ratio` は `at` のコマの SAR を返し、画面表示比を補正する。保存する枠は SAR 適用前の記録上の座標を使う。JPEG から SAR を読めず、ffmpeg は途中で変化しても先頭ストリームの SAR を渡す。そのため ffprobe で `at` の手前から読み、pts が `at` 以上の最初のコマの SAR を使う。同じ寸法のまま SAR だけが変わる境目では、SAR を 1 コマ取り違える可能性がある（未検証）。

ffmpeg の `-ss` が `at` から 1 コマずれる規則は未特定である。採ったコマのサイズが JPEG の SOF と違う場合は 1 コマ前を調べ、どちらも一致しなければ 500 を返す。

`at` が無い、負数、数値でない場合は 400 を返す。ごみ箱、原本の行が無い録画、原本の実体が無い録画は 404 を返す。これらの判定は ffmpeg を起動する前に行う。`/frame` は OpenAPI に載せず、HEAD も登録しない。

<details><summary>根拠</summary>

- 使う原本: `kind = 'original'`

</details>

##### 再生位置と視聴済みの印

再生位置と視聴済みの印は api ロールが DB に持つ。streamer は位置を知らない。Range 要求の位置は再生位置ではないからである。

再開位置は `/api/recordings/{id}/playback-position` に原本時間軸の ms で保存し、視聴済み印は `/api/recordings/{id}/watched` で扱う。決定と表の割り方は [frontend/recordings/playback.md](../../frontend/recordings/playback.md) §視聴状態。

#### X-Accel-Redirect（`storage.accel_location`）

X-Accel-Redirect は、認可判定をアプリ、バイト転送を nginx が担うパターンである。性能のためには要らず、既に nginx が前段に居る構成で転送を任せるために、オプションとして実装する。有効時は本文を返さず、ヘッダーで nginx に転送を任せる。

設定フラグ + レスポンスヘッダー 1 個で対応でき、アプリ側コストがほぼゼロである（Mastodon / GitLab の X-Sendfile 対応と同じ位置づけ）。

```yaml
storage:
  media_dir: /mnt/media
  accel_location: /_media/     # 空なら Go が直接配る
```

有効時に返すヘッダーは `X-Accel-Redirect: /_media/stores/<store>/<相対パス>` である。nginx 側は `internal` な location で store 名を除き、`media_dir` を alias する。旧 binary が返す `/_media/<相対パス>` も同じ設定で処理する。

パス検証を通した後にヘッダーを返す。検証前に返すと、細工された相対パスで nginx に任意ファイルを配らせられる。

値は URI として解釈されるので、パス要素を URL エスケープする。番組名由来のファイル名には空白・括弧・日本語が入る。

Range の扱いは nginx 側に移る。`Accept-Ranges` も nginx が付ける。
