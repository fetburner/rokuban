> [media.md](../media.md) の録画ファイル配信。索引から辿る

## メディア配信（Range 対応・X-Accel-Redirect オプション）

録画再生・ライブ視聴の HLS プレイリスト/セグメント・サムネイル画像は HTTP GET で配信する。

### 録画済みファイルのストリーミング

Go の `http.ServeContent` は `*os.File` 相手なら sendfile が効き、Range 対応も標準。家庭サーバーの同時視聴数本でギガビット LAN を飽和させるのに問題はなく、**性能を理由とする nginx 導入は不要**。

#### 実装

```
GET  /api/media/recordings/{id}/file              →  video/MP2T（原本、Range 対応）
HEAD /api/media/recordings/{id}/file              →  ヘッダーのみ
GET  /api/media/recordings/{id}/file?profile=h264 →  video/mp4 等（encoded、Range 対応）
GET  /api/media/recordings/{id}/file?profile=h264&track=subtitles → text/vtt（字幕サイドカー）
HEAD /api/media/recordings/{id}/file?profile=h264 →  ヘッダーのみ
GET  /api/media/recordings/{id}/thumbnail         →  image/jpeg
HEAD /api/media/recordings/{id}/thumbnail         →  ヘッダーのみ
GET  /api/media/recordings/{id}/seek-tiles        →  image/jpeg（シークプレビュー用の格子画像）
HEAD /api/media/recordings/{id}/seek-tiles        →  ヘッダーのみ
```

録画配信は一覧・詳細 API と `/api/recordings/{id}` の部分木を共有する。`{id}` は要求時まで列挙できないので、その後ろで api と streamer が分かれる。そのため標準 Ingress の `Exact` / `Prefix` だけでは、単一ホスト名から一意に振り分けられない。分割可能な外向きの形として、応答の性質を表す固定接頭辞 `/api/media/recordings/{id}/...` に移設した。メソッド（`GET` / `HEAD`）とクエリは変えない。

**active な encoded のブラウザ VOD は MP4 progressive + Range を使う。**
家庭 LAN のオンデマンド再生では、単一ファイル + `http.ServeContent` の Range が十分である。
原本だけが残る完了録画は一時 HLS でブラウザ再生する（[playback.md](playback.md)「録画原本のブラウザ再生」）。
ライブ視聴の HLS は別経路（[live-hls.md](live-hls.md)「ライブ視聴の HLS」）のまま。

**HEAD も登録する。** VLC やブラウザはシーク前に HEAD で `Content-Length` と
`Accept-Ranges` を取るため、405 を返すとシーク再生に失敗しうる。
`http.ServeContent` は HEAD ならヘッダーだけを書くので実装は共通。

**OpenAPI には載せない。** SSE と同じ理由で、生成クライアントは JSON を前提にする
（`customInstance` が `response.json()` を呼ぶ）ためバイナリ配信では誤った
クライアントが生成される。UI は URL を `<video>` の src や `<img>` の src、
保存リンクに直接使い、生成フックを経由しない。守るべきスキーマがないので
生成物から得るものもない。利用可能なプロファイル（名前 + サイズ）は
`Recording.encodedAssets`（一覧 API）で返す。

**メディアのバイト転送は streamer ロールが所有する。** api ロールはファイルシステムに依存しないため、転送は別ロールに置く。monolith では同じ HTTP リスナーに相乗りし、streamer が有効なときだけ登録する。

**`/file` は `profile` クエリが無いときは原本（`kind = 'original'`）、あるときは
`kind = 'encoded'` かつそのプロファイル名を返す**。**`/thumbnail` はサムネイル
（`kind = 'thumbnail'`）を返す**。ブラウザ UI は encoded を優先し、原本 TS は VLC 等
向けのダウンロードリンクに残す。原本が `until_encoded` で消えた後も派生物だけで
再生できる（アセット解決は kind ごとに独立）。

エンコードプロファイルに `subtitles: webvtt` を指定した場合、字幕は MP4/MKV に
内蔵せず、encoded ファイルと同じ basename の `.vtt` として保存する。配信 URL は
`/file?profile=<name>&track=subtitles`。サイドカーは `media_assets` に独立行を
持たないため、encoded 行が active であることと隣接ファイルの存在を配信時に確認する。
字幕が無い番組ではサイドカーは作られず、映像エンコードは成功する。

**一覧 API はプロファイルが字幕サイドカーを持つかを返さない**。`<track>` は
再生側が無条件に描画し、サイドカーが無ければ 404 を返すだけにする（字幕を隠したい
要求が出るまで `hasSubtitles` のような能力フィールドは作らない）。この非対称の
帰結として、字幕を使っていない全 encoded 再生でも、サイドカー欠損の 404 が定常的に
発生する。ただしこれはコミットと実ファイルの不整合を示す WARN では扱わない（配信側は
サイドカーかどうかを知っているので、その経路だけログを出さない）。

**`rel_path` は配信側でも独立に検証する。** メディアディレクトリ外を指す値は 404 にする。書き込み時に検証済みでも、DB に不正な行が入った場合に任意ファイルを読み出させないため、配信時の検証も要る。

**`/seek-tiles` はシークプレビュー用タイル（`kind = 'seek_tiles'`）を 1 枚の JPEG で返す。** タイルは 10 秒間隔、10 列で並び、最後の行の余りは黒で埋める。間隔・画像サイズ・列数・上限は worker とクライアントで同じ固定値を使う。

メディア配信は OpenAPI の対象外で、worker から UI へ値を渡す経路はない。値を変えたときは既存タイルを再生成する。

未生成なら 404 を返し、クライアントは poster だけの表示に戻る。要求中に生成を待たず、thumbnail reconcile の定期パスで作る。

タイルの寿命は poster と同じである。原本が消えた後も派生物として残り、ごみ箱では 404、完全削除でアセットグループごと消える。

**`/frame` は CM 検出のロゴ位置を教えるためのコマを返す。**

```
GET /api/media/recordings/{id}/frame?at=<milliseconds> → image/jpeg
```

| 項目 | 挙動 |
|---|---|
| 入力 | `kind = 'original'` の原本だけを使い、縮小や SAR の焼き込みはしない |
| `X-Coded-Width` / `X-Coded-Height` | `at` のコマで返す JPEG 自身の幅と高さを返す。途中で解像度が変わり、渡した枠が録画の最初の映像ストリームと異なる場合、検出は失敗する |
| `X-Sample-Aspect-Ratio` | `at` のコマの SAR を返し、画面表示比を補正する。保存する枠は SAR 適用前の記録上の座標を使う |
| SAR の取得 | JPEG から SAR を読めず、ffmpeg は途中で変化しても先頭ストリームの SAR を渡す。そのため `at` の手前から調べ、pts が `at` 以上の最初のコマの SAR を使う |
| コマの選択 | ffmpeg の `-ss` が `at` から 1 コマずれる規則は未特定。採ったコマのサイズが JPEG の SOF と違う場合は 1 コマ前を調べ、どちらも一致しなければ 500 を返す |

同じ寸法のまま SAR だけが変わる境目では、SAR を 1 コマ取り違える可能性がある（未検証）。

`at` が無い、負数、数値でない場合は 400 を返す。
ごみ箱、原本の行が無い録画、原本の実体が無い録画は 404 を返す。
これらの判定は ffmpeg を起動する前に行う。
このルートは OpenAPI に載せず、HEAD も登録しない。

**配らないもの:** ごみ箱に入った録画（`recordings.deleted_at IS NOT NULL`）、
削除済みアセット（`media_assets.state <> 'active'`）。未 ingest の録画
（指定 kind の `media_assets` 行なし）、存在しないプロファイルも配らない。いずれも 404。
コミット（DB 行）はあるのにファイルが無い不整合も 404 にしつつ WARN で記録する
（孤児回収や外部からの削除）。

**`Cache-Control: private, max-age=0, must-revalidate`。** 原本は一度書いたら
変わらないが、ごみ箱からの復元で同じ URL の中身が入れ替わりうるので
`immutable` は付けない。`Last-Modified` による条件付きリクエストは効く。

**`media_assets.size_bytes` と実ファイルのサイズが違えば WARN で記録する。**
size_bytes は ingest / encode 時に照合した値なので、
違うならコミット後に改変・切り詰めが起きている。配信自体は続ける
（ユーザーは録画を見たい）。

**再生位置と視聴済みの印は api ロールが DB に持つ**。
streamer は位置を知らない（Range 要求の位置は再生位置ではない）。再開位置は
`/api/recordings/{id}/playback-position` に原本時間軸の ms で保存し、視聴済み印は
`/api/recordings/{id}/watched` で扱う。決定と表の割り方は
[frontend/recordings/playback.md](../../frontend/recordings/playback.md) §視聴状態。

<details><summary>根拠</summary>

- メディア配信のロール境界と monolith での登録: `internal/streamer`、不変条件 1、`api.RouterConfig.Mounter`。`--all` でも streamer が有効なときに登録する
- ingest と共有するパス検証: `internal/mediapath.Resolve`
- seek tile のクライアント側固定値: `web/src/lib/seek-tiles.ts`
- UI に返すプロファイル済みアセット分類: `encoded`
- seek tile を伝える OpenAPI 経路が無い: `openapi.yaml`
- seek tile の定期生成: `thumbnail_reconcile`

</details>

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
