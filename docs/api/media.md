> [docs/api.md](../api.md)（索引）の分割本文。メディア配信（録画バイト配信・サムネイル・ライブ HLS・IPTV / XMLTV エクスポート・SPA アセット）の仕様はここが唯一の権威（openapi.yaml には載せない）。

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
原本だけが残る完了録画は一時 HLS でブラウザ再生する（下記「録画原本のブラウザ再生」）。
ライブ視聴の HLS は別経路（下記「ライブ視聴の HLS」）のまま。

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

**`rel_path` は配信側でも独立に検証する。** `internal/mediapath.Resolve` を
ingest と共有し、メディアディレクトリの外を指す `rel_path` は 404 にする。
書き込み時に検証済みでも、DB に不正な行が入った場合に任意ファイルを
読み出させないため片側だけでは足りない。

**`/seek-tiles` はシークプレビュー用のタイル画像（`kind = 'seek_tiles'`）を返す。**
1 枚の JPEG で、10 秒間隔のタイルが 10 列の格子に並んでいる（枚数が列数の倍数でない
ときの余りは黒）。間隔・1 枚の大きさ・列数・上限は worker の固定値である。
クライアントは同じ値を `web/src/lib/seek-tiles.ts` に持つ ——
メディア配信は `openapi.yaml` の対象外なので値の伝達経路が無い。
**値を変えたら既存のタイルの再生成が要る。**
**未生成なら 404 を返し、クライアントは poster だけの従来の見た目に戻る** ——
要求の経路で生成を待たせない（生成は `thumbnail_reconcile` の定期パスが行う）。
寿命は poster と同じである（原本が消えても派生物として残り、ごみ箱で 404、完全削除で
アセットグループごと消える）。

**`/frame` は CM 検出のロゴ位置を教えるためのコマを返す。**

```
GET /api/media/recordings/{id}/frame?at=<milliseconds> → image/jpeg
```

原本（`kind = 'original'`）だけを入力にする。画像の縮小や SAR の焼き込みはしない。
応答には `X-Coded-Width` と `X-Coded-Height` を付ける。
値は返す JPEG 自身の幅と高さで、先頭のストリームではなく `at` のコマのものである。
途中で解像度が変わる録画でも、幅と高さ（寸法だけ）は画像と一致する。
教えた枠が後半の解像度なら、検出側は録画の解像度（最初の映像ストリーム）と合わず、失敗として残す。
さらに `X-Sample-Aspect-Ratio` を付ける。値は `at` のコマの SAR で、画面はこの SAR で表示比を補正する。
保存する枠は SAR 適用前の記録上の座標を使う。
SAR は ffmpeg が返す JPEG からは読めない。ffmpeg は途中で SAR が変わっても、ストリーム先頭の SAR を出力へ渡す。
そのため ffprobe で `at` の手前から読み、pts が `at` 以上の最初のコマの SAR を採る。
ffmpeg の `-ss` は `at` から 1 コマずれたコマを返すことがある（規則は特定できていない）。
採ったコマの大きさが JPEG の SOF と違えば 1 つ前のコマを見て、どちらも合わなければ 500 を返す。
大きさが同じで SAR だけ変わる境目（720x480 の 4:3 と 16:9 の切り替えなど）では、境目の 1 コマ分の SAR を取り違えうる（未検証）。

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
[frontend/recordings.md](../frontend/recordings.md) §視聴状態。

### IPTV / XMLTV エクスポート

外部プレイヤーには既存のサービス・録画・EPG 射影を投影した標準形式を渡す。
**共有リンクやアプリ内認証は追加しない。**

```text
GET /api/iptv/playlist.m3u?include=all&liveProfile=<name>&recordingProfile=<name>
GET /api/iptv/xmltv.xml
```

`include` は `all`（既定）、`live`、`recordings` を受け付ける。`all` はライブ局と
録画の両方を載せる。`liveProfile` を省略すると既定のライブプロファイルを使う。
`recordingProfile` を省略すると原本を選び、名前を指定するとその encoded profile の
active アセットだけを載せる。録画は完了済み・ごみ箱外・未 supersede で、選択した
アセットが active のものに限る。`live.enabled: false` では `include=live` は 404、
`include=all` は録画だけを返す。

M3U の URL は既存のライブ HLS パスと `/api/media/recordings/{id}/file` を直接指す。
出力するのはルート相対 URL なので、外部プレイヤーには**M3U の URL をネットワークから
直接読み込ませる**。ファイルへ保存してから開くと、URL の基準になる接続先を失う。
XMLTV と M3U の URL にホスト名・ユーザー名・パスワード・署名トークンを含めない。

URL に期限は設けない。配信のたびに既存の streamer が active アセットと録画の削除状態を
確認するため、M3U を取得した後にごみ箱へ移した録画や削除済みアセットは 404 になる。
原本を削除すると既定 M3U からその録画を除く。原本が無くても指定した encoded profile が
active なら、その profile の M3U には残る。期限付きトークンや XMLTV 用の別 ID は作らない。

XMLTV は `epg_services` と `epg_programs` の現在の射影だけを出す。時間範囲は現在時刻の
3 時間前から 7 日間である。録画一覧や予約は XMLTV の正本として扱わない。番組名・説明・
放送時刻は EPG 射影の値を使い、時刻には数値オフセットを付ける。stop が start より後で
ない番組は stop を省略する。

| 外部形式のキー | 対応する既存資源 |
|---|---|
| ライブ局の M3U `tvg-id` / XMLTV `channel id` と `programme channel` | `rokuban.<site>.<networkId>.<serviceId>`。サイトと放送サービスの組を表すエクスポート内の照合値。録画エントリには付けない（局の現在の番組に録画が紐付くため） |
| XMLTV `programme/url`（system=`rokuban-api`） | `/api/sites/{site}/programs/{programId}`。番組 API の既存キー |
| XMLTV `channel/url`（system=`rokuban-live`） | 既存のサービス単位ライブ HLS URL。ライブが無効なら省略 |

これらのエクスポートは `text/plain` / XML の応答で、生成 TypeScript クライアントは
JSON として読むため OpenAPI には載せない。ストリーム本体の配信は従来どおり
`internal/streamer` が所有する。XMLTV の `<channel>` と `<programme>` は
[XMLTV DTD](https://github.com/XMLTV/xmltv/blob/master/xmltv.dtd) に沿って出力する。

認証が必要な場合はリバースプロキシでエクスポート・HLS・録画ファイルの全パスを保護する。
外部プレイヤーにはプロキシの認証を設定し、URL に資格情報を埋め込まない。
実機の Range・字幕・認証確認は [runbook/iptv.md](../runbook/iptv.md) を参照。

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

### ライブ視聴の HLS --- 既定はアプリ配信

`playlist.m3u8` は常に master playlist で、音声 rendition 3 本（下記 §音声）を含む。
`live.captions: false`（既定）では `?profile=` のプロファイルだけの master を返す。
`true` のときは全プロファイルを 1 つの master にまとめ、ffmpeg の
`libaribcaption` で変換した WebVTT 字幕 rendition も含む。variant / 字幕 playlist、
`.ts` / `.vtt` セグメントは同じサービス URL の下で配信する。hls.js は字幕 rendition
を字幕トグルとして表示する。VOD とライブのどちらも TS/PES を Rokuban が読むことはない。

ライブセッションはインメモリの使い捨て状態（全体アーキテクチャの crash-only 例外）で、「クライアントがいなくなったら ffmpeg を止める」idle GC が要る。セグメント要求がアプリを通れば last-access の更新がタダで手に入るが、nginx が scratch から直接配るとアプリはクライアントの生存を見失う。`auth_request` やログ監視で回収はできるが、セグメントは数 MB で転送負荷が軽く、複雑さに見合わない。**既定は streamer ロールのアプリ配信のまま**とする。遅延を目標にする構成では、パッケージングと生存判定をまとめて外の packager に移す（下記「パッケージャは MediaMTX の LL-HLS を選べる形にする」）。

**`live.enabled: false` ならこれらのルートは登録されず、404（JSON）になる**。
SPA フォールバックには落とさない（[rest.md](rest.md)「機能の有効/無効は能力 API で
観測する」）。落とすと「無い」が HTML の 200 になり、probe するクライアントが成功と
誤認する。導線そのものを出さない判断は `GET /api/capabilities` 側。

#### 遅延の目標は置かない --- `segment_seconds` を短くしても縮まない

**アプリ配信の経路では、ライブの glass-to-glass 遅延を目標値にしない。**
実クラスタ・実チューナー・実ブラウザで 2 秒と 1 秒のセグメント長を比べても、遅延の改善は 6.2 秒 → 5.0 秒どまりである。
4 秒には届かない。代わりに 30 分あたりの stall は 3 回 → 454 回、dropped frames は
0.006% → 2.1% になった。

**セグメントの公開間隔そのものが揺らぐので、`segment_seconds` は遅延の目標値にならない。**
実測では p95 が 2.3 秒 → 3.6 秒、最大が 4.7 秒 → 10.2 秒へ広がった。プレイヤーは
ライブ端より数本後ろから開始するので、公開の揺らぎはそのまま再生の飢えになる。
**短くするほど遅延が縮むという関係は、ここには無い。**

1 秒級を狙うなら、セグメントをさらに短くするのではなくパッケージャを替える（下記
「パッケージャは MediaMTX の LL-HLS を選べる形にする」）。また
`live.profiles[].segment_seconds` はライブと追っかけ再生で共有しているので、
ライブだけ短くするには値の分離も要る。

**この値はセグメントの公開端とプレイヤーの latency からの導出値である。** 放送時刻を
映像に焼き込んだ比較ではない（ffmpeg の再 mux で TDT/TOT が落ちるため）。したがって
「放送から 6.2 秒」と断言はしない。測定の条件と経路ごとの内訳は
[frontend/live.md](../frontend/live.md) §「遅延・バッファの計器」に置く。

#### パッケージャは MediaMTX の LL-HLS を選べる形にする

**遅延を目標にする構成では、パッケージングと生存判定をまとめて MediaMTX に移す。**
アプリ配信の根拠は、セグメント要求がアプリを通れば生存判定がタダで手に入ることだった
（上記）。この形では生存判定も MediaMTX の HLS セッションの inactive 判定に移るので、
その根拠とは衝突しない。代わりに離脱ヒントが効かなくなる（下記）。

実測（同一局・720p・視聴者 1・30 分・実 Chrome + hls.js）では、アプリ配信
（`segment_seconds: 2`）の `hls.latency` 中央値は 5.68 秒だった（上記の 6.2 秒とは
別の測定）。MediaMTX v1.21.1 の LL-HLS（part 200ms / segment 1s）は 0.92 秒
（p95 2.15 秒）である。セグメント長を詰めても 1 秒台には入らない（上記）ので、
パッケージャを替える以外に手段が無い。試作は `-tune zerolatency` を含むが、その寄与は
高々 0.1〜0.3 秒で、差の大半は part による部分公開に由来する。

**代わりに stall と dropped frames が増える。** 同じ 30 分で stall は 1 回 → 15 回、
dropped frames は 0 → 0.14% だった。試作側は WAN 越しの port-forward・ソフトウェア
エンコード・part 単位の取得（24 req/s）という条件の差を含む。差のどこまでが
MediaMTX に由来するかは未検証である。

- **必須にはしない。** MediaMTX の設定テンプレートに mirakc の知識（合成 service id・
  URL 組み立て）は置かない。`runOnDemand` が rokuban のコマンドを起動する形にすれば、
  `live.enabled` と同じ位置づけ（無くても現行のアプリ配信で動く）にできる
- **idle GC は packager 側に移り、下限は約 30 秒になる。** 最後の要求から publisher が
  止まるまで、既定では約 60 秒かかる。`hlsMuxerCloseAfter` と `runOnDemandCloseAfter` を
  1 秒に詰めても約 32 秒である（HLS セッションの inactive 判定が支配的）。現行の
  `idle_timeout`（既定 30 秒）と同等である
- **未解決: 離脱ヒントの猶予（`3 × segment_seconds + 2s` = 8 秒）に相当するものが
  MediaMTX に無い。** ヒントを受けた rokuban が publisher を止める経路を作れるかは
  未検証である。作れなければ、チューナーが 1 本のサイトでは切り替えのたびの解放が
  8 秒から約 30 秒へ伸びる
- **`/live/segments/` の実パスは契約ではない。** hls.js も VLC もプレイリスト URL からの
  相対解決しかしないので、外から見える契約はプレイリスト URL だけである。現行の
  `segments/<name>` は ffmpeg の `-hls_base_url` が書く値で、クライアントは組み立てない。
  MediaMTX の LL-HLS も `init.mp4` / `*_partN.mp4` / `*_segN.mp4` をプレイリストと
  同じディレクトリの相対 URI で書く
- **前段はプレイリストだけでなく全要求の接頭辞を写す。** 相対 URI は、クライアントが
  要求した `.../live/` の下で解決される。そのため前段は全要求の接頭辞
  `/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/` を、
  MediaMTX のパス `live/{site}/{networkId}/{serviceId}/` へ書き換える。加えて `playlist.m3u8` を
  `index.m3u8` へ写す。試作では計器自身が立てた同一オリジンのリバースプロキシが
  この写しを担い、実 Chrome + hls.js が 30 分再生できた
- **プロファイルはパスを分ける。** 1 パスに映像 2 本を publish しても、MediaMTX は
  2 本目を `skipping track 3 (H264)` として捨て、ABR の master を書かない（実測）。
  `?profile=` の選択を保つには、プロファイルごとのパスと、それを束ねる master の
  書き手が要る
- **未解決: 音声レンディションと字幕。** 試作では音声 2 本が rendition として master に
  載った。上記の契約（標準 / 主 / 副の 3 本、`captions: true` の WebVTT 字幕）を
  MediaMTX の経路でどう満たすかは未測定である
- **未解決: MediaMTX の LL-HLS はセッション ID を URI のクエリに載せる**
  （`?session=<uuid>`）。master が書く variant の URI（`video1_stream.m3u8?session=…`）
  にも載る。つまり hls.js が取り直し続ける variant の URL が session を握る。
  MediaMTX の再起動・ハッシュの担当移動・idle GC の後に、未知の session を持つ
  variant 要求へ MediaMTX が何を返すかは未検証である。404 なら下記「資源同定」が
  塞いでいる「セッション ID を握ったクライアントが 404 で詰む」経路そのものになり、
  この形は採れない

#### 資源同定: セッション ID を持たない

プレイリストとセグメントの URL は
**`/api/sites/{site}/networks/{networkId}/services/{serviceId}/live...`**
の形にする。そして**セッション ID を URL にもクッキーにも置かない**。ライブセッションは
サービスに対して 1 つで、同じサービスを見ている視聴者はそれを共有する。

- **チューナーが共有される。**別の部屋で同じチャンネルを見ても ffmpeg 1 本・
  チューナー 1 本で済む。チューナーは録画と取り合う唯一の共有資源なので、これが
  一番効く
- **スケールアウトの鍵が既に資源同定の中にある**。`(site, networkId, serviceId)` は
  前段の consistent hash の鍵にそのまま使える。そのため streamer のレプリカを増やしても
  URL・クライアント・API は変わらない（[operations.md](../operations.md) §5
  「streamer のスケール」。URL を固定深さにする制約もそこに書いてある）
- **セッションが消えても URL が死なない。**Pod 死・ハッシュの担当移動・idle GC の
  後でも、同じ URL への再要求が新しいセッションを起こす。「セッション ID を握った
  クライアントが 404 で詰む」経路が存在しない。**セッションを起こすのは master だけ
  ではない。** hls.js が取り直し続けるのは variant playlist の方なので、variant の
  要求もセッションを作り直す。master だけにすると、セッションが消えた後の variant
  要求が 404 になり、hls.js は 4xx を再試行せずに止まる

セッション ID を持つ設計（`POST` でセッションを作って ID 付きの URL を配る）は、
**導出物の identity を宛先にする**形になる（不変条件 9 の identity 系）。ライブ
セッションは使い捨ての導出物なので、宛先は「このサービスが見たい」という欲求の側で
名指しする --- レベルトリガー（不変条件 5）と同じ形である。

**パスの id 空間は一覧 API に揃える**。`{networkId}` / `{serviceId}` は
`GET /api/sites/{site}/services` が返すのと同じ **SI の値そのもの**である。
mirakc が要求する Mirakurun 合成 service id（`networkId * 100_000 + serviceId`）
への変換は、streamer が `internal/programid.ServiceID` で行う。

- **同じ URL 階層に 2 つの id 空間を同居させない。**`services/{serviceId}` が
  一覧では SI の値、ライブでは合成 id を指す状態は、将来
  `GET /api/sites/{site}/.../services/{serviceId}` を足したときにどちらの空間か
  決められなくする。API の資源同定は差し替えコストが最も高い先払い（不変条件 11）
  なので、読者が 1 つ（同梱 SPA）しかいない今のうちに払う
- **mirakc の id 規則を Rokuban の一番外側に置かない。**合成 id は mirakc /
  Mirakurun の内部規則（`internal/programid.ServiceID`）であり、変換を web に置くと
  同じ規則の実装が Go と TypeScript に二重化する。URL は永続テーブルより
  差し替えが高いので、mirakc 固有の概念を置く場所として最悪である（不変条件 7 の
  精神）
- **`serviceId` 単独では鍵にならない。**SI の service_id は network をまたぐと
  一意でない（Mirakurun が合成 id を発明した理由そのもの）。宛先を SI の値で
  名指しするなら `networkId` も URL に要る --- 放送イベントを
  `(site, network_id, service_id, event_id)` で引く不変条件 9 と同じ形

帰結として **idle GC の粒度もサービス単位**になる（そのサービスへのセグメント要求が
一定時間来なければ ffmpeg を止める）。「クライアント 1 人ごとの生存」は追わない。

#### 離脱は「ヒント」であって停止命令ではない

チャンネルを切り替えた視聴者のセッションが `live.idle_timeout` のあいだ残ると、
**1 人の視聴者が 2 本のチューナーを掴む**。チューナーが 2 本しか無い環境では
切り替え 1 回で録画が開始できなくなりうる（mirakc の優先度では録画が勝つので、
視聴者からは「切り替えたら見られなくなった」と見える）。この窓を縮めるために
離脱の受け口を置くが、**「セッションを閉じる API」にはできない**。

```
POST /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave
  → 204 No Content（常に）
```

- **止めるのではなく idle 期限を「いま + 短い猶予」に詰める。** ライブセッションは
  サービス単位で共有される（上記）ので、「離れた側の要求で止める」形にすると
  **別の部屋で同じチャンネルを見ている視聴者の再生を一方的に切れてしまう**。
  他に視聴者がいれば、その人の次のセグメント / プレイリスト要求が last-access を
  更新して期限が元に戻る --- **ヒントは収束を速めるだけで、「誰かが見ているか」と
  いう真実は既存の観測（セグメント要求）が持つ**。レベルトリガー（不変条件 5）と
  同じ形であり、クライアントの identity も参照カウントも要らない
- **宛先は `(site, networkId, serviceId)`。** セッション ID は導入しない（この節の決定）。
  固定深さも保つので前段の consistent hash 鍵の取り出しはそのまま効く
- **セッションを作らない。** 該当セッションが無ければ何もせず 204（存在を漏らさず、
  `sendBeacon` に再送の材料も与えない）。**POST なのは `navigator.sendBeacon` が
  POST しか出せないから** --- モバイル Safari では `unload` が発火しないので、
  ページ離脱時に届く送信手段はこれしかない
- **猶予は設定キーにせず `live.profiles[].segment_seconds` から導出する**
  （`3 × 最長の segment_seconds + 2s`。既定 8 秒）。**`idle_timeout` でクリップしない**
  --- クリップすると `segment_seconds: 6` + `idle_timeout: 2s` のような設定で猶予が
  セグメント長を下回る。猶予が `idle_timeout` 以上になる設定では、ヒントが no-op に
  なる方へ倒す。理由は `leaveGrace` の doc コメントにある。守るべき性質は「猶予 > 生きている視聴者の次の要求が来るまでの間隔」で、
  その間隔を決めているのはセグメント長そのもの。独立したキーにすると
  `segment_seconds: 6` と 1 秒の猶予のような組み合わせが書けてしまい、**leave が
  「他人の視聴を切る道具」に化ける**。導出ならその組み合わせは表現不可能になる
- **期限は縮む方向にしか動かない**。ヒントが**延命**に使えてしまわないよう、
  last-access は巻き戻しだけを許す（猶予が `live.idle_timeout` 以上になる設定
  では、詰め先が現在の期限より後ろになるのでヒントは何も起こさない）
- **待っている客も客**。セッションの起動待ち（mirakc 接続 + ffmpeg 起動 +
  プレイリストの 1 本目が出るまで。最大 `playlistStartupTimeout` = 15 秒）は、
  誰も要求を出さない無音区間に見える。**そこにはそのセッションを待っている
  視聴者がいる**。ここで last-access が止まったままだと、この区間に届いた
  ヒント 1 発で起動待ちの視聴者ごとセッションが回収される（実測: 504）。
  待っている側がポーリングのたびに last-access を更新することで、無音区間
  そのものを無くす。猶予を「起動待ちより長く」する形では、ヒントの効きが
  その分鈍る（この経路は猶予を 8 秒に詰められるようにしたことで生まれた）
- idle GC ループの刻みも猶予の半分にする（`idle_timeout / 2` のままだと、期限を
  詰めても回収が次のパスまで来ずヒントが刻みに飲まれる）

ヒントが届かなくても壊れない（従来どおり `live.idle_timeout` で回収される）し、
余計に届いても壊れない（自分の次の要求が期限を戻す）。

**プロファイルはクエリ（`?profile=`）で受け、ハッシュ鍵には入れない。**鍵に入れると
同じサービスを別プロファイルで見たときに 2 つの Pod に割れ、チューナーを 2 本掴む。
1 つの Pod の中で 1 チューナーから複数プロファイルを出す。

#### 一覧の契約（`GET /api/live-profiles`）

画質セレクタが出すのは `config.live.profiles` の名前である。その公開面は
**`GET /api/live-profiles`** に置く（`LiveProfileSummary { name, height? }`。M4-21）。

- **`GET /api/capabilities` の `live` を object にしない。** あちらの規律は
  「返すのは真偽値だけで、config のキー名・値は載せない」（`ListEncodeProfiles` /
  `ListSites` と同じ）であり、一覧は「何が選べるか」、`live` は「導線を出して
  よいか」という別の問いである。畳むと `lib/capabilities.ts` の 4 値判定
  （`pending` / `unknown` / `enabled` / `disabled`）の派生が濁る
- **載せるのは `name` と表示用の `height` だけ。** `video_codec` / `crf` / `qp` /
  `preset` / `extra_args` / ffmpeg パスは出さない --- 出すとフロントがそれを再現する
  形に育つ（`EncodeProfileSummary` と同じ規律）。`height` は実際の出力高で、
  0 または省略は「スケールしない」
- **順序が既定の根拠である。** 先頭が `?profile=` を省略したときの既定
  （`LiveConfig.profile` と同じ決め方）で、フロントは並びを変えない
- **`live.enabled` は見ない。** 返るのは `config.live.profiles` の写しで、**無効な
  デプロイでも profiles が書かれていれば返る**（`config.compose.yml` は
  `enabled: false` と `profiles` を並べて出荷している）。有効かどうかは
  `GET /api/capabilities` の `live` の問いである --- ここで無効を空配列に潰すと、
  同じ config の状態を 2 箇所で判定することになる。未定義なら空配列（`null` ではない）
- **未知の名前は 400。** `?profile=` が空なら既定（先頭）に落ちるが、一覧に無い名前は
  `unknown live profile` を返す（セッションを起こす前に拒否する）。フロントは
  一覧に照らして先に落とすので、この 400 は直リンク・手書き URL の受け皿になる

**ロール分割で api と streamer に別の config を配る構成では、この一覧と実際に
配られるプロファイルがずれる。** ずれを検出する手段は無い。一覧は api が読む
config から作り、実際に配るのは streamer である。したがって**「一覧にあるから
見られる」とは言えない**（下界主義。[data.md](../data.md) §6.5 と同じ規律）。

#### 実装（`internal/streamer`）

```
GET  /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/playlist.m3u8[?profile=<name>]
       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/segments/{name}
       → video/mp2t
POST /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave
       → 204（離脱のヒント。上記「離脱は『ヒント』であって停止命令ではない」）
```

master から参照される variant playlist（映像・音声）と字幕 playlist は
`.../live/{name}.m3u8` で配信する。受け付けるのは ffmpeg が書く variant の名前だけで
（master の名前や未知のプロファイルは 404）、セッションが無ければ作る
（上記「セッションが消えても URL が死なない」）。`.vtt` は字幕有効時だけ受け付ける。

- **DB を引かない。**パスの `(networkId, serviceId)` から mirakc の
  `GET /api/services/{id}/stream?decode=1` の `{id}` を合成するだけ
  （`internal/programid.ServiceID` の純関数）。ライブセッションはインメモリの
  使い捨てで、認可はリバースプロキシ委譲、同時上限もプロセスローカルなので、
  DB を引く理由が無い
- **id セグメントは 16 bit 符号なし整数の十進正準形としてだけ受け付ける**
  （`strconv.ParseUint(s, 10, 16)` + 先頭ゼロの拒否。SI の network_id /
  service_id の幅）。読めなければ 400 で、mirakc には触れない。**mirakc に渡るのは
  常にここで合成した整数であり、URL の文字列ではない**ので、パス区切り（`%2F`）や
  クエリ（`%3F`）を仕込んで mirakc の別エンドポイントへの要求に化けさせる経路が
  無い。測っているのは
  `TestLiveStreamer_RejectsHostileIDSegments`（`%2F` / `%3F` / 非数値 / 空 /
  符号付き / 16 bit 超 / 桁あふれ / 全角数字 / 先頭ゼロを 400 で止め、偽 mirakc が
  1 件も要求を受け取らないこと）と
  `TestLiveStreamer_MirakcPathIsComposedFromPathSegments`（mirakc が受け取る
  Request-URI が `/api/services/3192053248/stream?decode=1` ちょうどであること）
- **正準形だけを受けるのは前段の hash 鍵が URL 文字列だから。**`1024` と `01024` は
  streamer 内部では同じセッション鍵（合成後の整数）になるが、
  [operations.md](../operations.md) §5 の `map $uri $live_key` は URL の文字列を
  鍵にするので、別名を許すと同じチャンネルが 2 つの Pod に落ちてチューナーを
  2 本掴む。「同じチャンネルの視聴者は同じ Pod」という鍵の取り方の前提を、
  URL の正準性に暗黙依存させない
- **「不明な id を mirakc がどう扱うか」は測っていないし、依存もしていない**。
  実在しない id での起動失敗は、他の失敗（チューナー枯渇・ffmpeg 起動失敗）と同じく
  503 にまとまる。`TestLiveStreamer_UpstreamRejectionBecomes503` は、上流が拒否
  ステータスを返したとき、本文も含めて他の起動失敗と同じ 503 になることを見る
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
- **チューナー調停は mirakc のリクエスト優先度に一元化する**。ライブの GET には
  `live.tuner_priority`（既定 1）を `X-Mirakurun-Priority` に載せる。ruler が生成する
  schedule の既定 priority（10）より低く保つことで、チューナー枯渇時に mirakc が
  録画側を常に勝たせる（[recording.md](../recording.md) §2「チューナー調停」）。
  予約表を見て拒否する案は採らない --- streamer が予約エンジンに依存し、mirakc 固有の
  優先度概念を永続テーブルに持ち込む誘惑を生む（不変条件 7）。**`live.tuner_priority <
  rules.priority` を Rokuban は強制しない**。前者は config、後者は DB でユーザーが
  自由に変えられる値である。両者を跨いで検証する権威がどちらの層にも無い（config は
  デプロイ環境の性質、DB は運用中の意思。[configuration.md](../configuration.md) §config
  と DB の境界）。ルールの priority を既定 10 未満まで下げると、この既定値のままでは
  ライブが録画に勝ってしまう --- 運用者が両方の値を意識して選ぶ前提とする
- **同時セッション上限（`live.max_sessions`）はプロセスローカル。**超えた要求・
  mirakc 側のチューナー枯渇はいずれも既存セッションを壊さずに 503 を返す
  （エラーの本文はプレーンテキスト。OpenAPI 対象外のため生成クライアントの契約は無い）
- **idle GC はサービス単位。**セグメント要求（プレイリスト取得も含む）ごとに
  last-access を更新し、`live.idle_timeout` の間要求が来なければ ffmpeg と mirakc への
  接続を止める。クライアント 1 人ごとの生存は追わない。**離脱ヒント
  （`POST .../live/leave`）はこの last-access を巻き戻すだけ**で、止めるのは
  あくまで idle GC である（上記「離脱は『ヒント』であって停止命令ではない」）
- **新しいセッションの起動が mirakc に拒否されたとき、またはプロセス内上限に達した
  ときだけ、最古の idle セッションを退避して
  1 回だけ再試行する**。最長の `segment_seconds` の 2 倍より長く要求が来ていない
  セッションを候補にする。該当するものが無ければ従来どおり 503 を返す。候補の選択に
  mirakc の現在状態は使わない。状態を取得しても読み取りと再試行の間に古くなるためで、
  自分側の last-access と起動失敗だけで判断しても外れた場合の結果は従来の 503 と同じになる。
  退避したセッションの `stop()` が完了しても mirakc の tuner 解放は非同期なので、再試行の
  前に実測値（2.35〜4.18 秒）に余裕を持たせた 5 秒の解放待ちを 1 回だけ入れる
- **退避は上流拒否の理由を区別しない。** mirakc はチューナー枯渇も存在しない
  service への要求も同じ 404/503 で返すため、streamer 側では区別できない。結果として、
  存在しないチャンネルへの要求でも idle セッションの退避が走る（近似分析が
  受け入れている性質）
- **セグメントは `live.segment_dir`（tmpfs 前提）に書く**。録画バッファとは別ディスクに
  する（[operations.md](../operations.md) §5「ライブのセグメントを録画バッファと同じディスクに
  置かない」）。プロセス終了（`--all`/`--roles streamer` の SIGTERM）時は idle GC と同じ
  経路で全セッションを止め、ディレクトリも削除する。**tmpfs はノード再起動でしか
  消えない**（k8s の `emptyDir: {medium: Memory}` はコンテナ / Pod の再起動をまたいで
  残る）。そのため SIGKILL によるクラッシュ（SIGTERM が効かない）の後始末は、
  それだけでは終わらない。起動時（`NewLive`、HTTP リスナーが立つ前）に `live.segment_dir` の
  **中身**を掃くことで、前回プロセスの残骸を毎起動で必ず消す。**`segment_dir` 自体は
  消さない** --- `emptyDir` を `segment_dir` に直接マウントする構成では、Linux は
  マウントポイント自体への rmdir を EBUSY で拒むため（詳細は
  [configuration.md](../configuration.md) §live）
- **ffmpeg の LookPath 検査は `live.enabled: true` のときだけ行う。**公式イメージ
  （ffmpeg 無し）で streamer ロールを起動する構成（録画配信 / サムネイルのみ）を
  壊さない

#### 音声（二重音声の主 / 副）

**音声はプロファイルごとに 3 本の代替音声 rendition（標準 / 主 / 副）で出し、
選ぶのはプレイヤーである。** サーバーは選択を知らず、セッションも作り直さない。
選択は視聴者ごとの状態で、共有セッションの寿命に載せると作り直しのたびに黙って
既定へ戻る（idle GC の後に既定の要求が作り直す等）。

- **主 / 副は出力側の `pan` で作る。** 二重音声の既定デコード（`-dual_mono_mode`
  無し）は L = 主 / R = 副のステレオである。片側を両耳へ写せば主 / 副になる
  （`pan=stereo|c0=c0|c1=c0` / `pan=stereo|c0=c1|c1=c1`）。`-dual_mono_mode` は
  デコーダ側のオプションで 1 回の起動に 1 つしか選べないので使わない。
  実測（ffmpeg 9.0.2）: モノラル AAC 2 本を 1 フレームに継いだ二重音声
  （SCE 2 つ、`channel_configuration=2`）で、`pan` の出力は
  `-dual_mono_mode main|sub` の出力とバイト一致した
- **標準はフィルタ無しで `DEFAULT=YES`。** 今までと同じエンコードなので、
  音声を選ばない視聴者の音は変わらない
- **放送にある音声を列挙しない。** 二重音声は 1 本の AAC ES の中の 2 つの SCE で、
  ffprobe では通常のステレオと区別できない。区別できるのは記述子だけで、
  それは不変条件 6 の外である。そこで選択肢は常に 3 つで、二重音声でない番組で
  主 / 副を選ぶと片側のチャンネルだけになる
- **並び順が UI との契約である。** グループ内の 0 = 標準 / 1 = 主 / 2 = 副で選ぶ。
  master の `NAME` は ffmpeg が `audio_<n>` で固定し、n はプロファイル数でずれる
- **ライブの playlist には `EXT-X-PROGRAM-DATE-TIME` を付ける。** 無いと hls.js は、
  前に聴いた音声へ戻ったときに止まる。止まるのは、ライブの窓
  （`playlist_size` × `segment_seconds`）より後で戻った場合である。判定は `web/e2e/live-audio.mjs`（PDT を外すと落ちる）
- **captions 無効時はプロファイル別の出力のまま、各出力が自分の master を持つ。**
  1 つの master にまとめると `hls_time` が 1 つになり、プロファイルごとの
  `segment_seconds` が書けなくなる（captions 有効時はそのため同一値を要求している）
- **2 本目の音声 ES（`-map 0:a:1`）は選べない**
- **追っかけ再生は画質を `?profile=` で選べる。** 追っかけのセレクタは
  `/recordings/$id?liveProfile=<name>#chase` に置き、一覧 API の名前を streamer へ
  渡す。画質の切替は `(recordingID, offset)` で同定された既存セッションの別
  プレイリストを取るだけで、セッションを作り直さない。再生位置は profile によらず
  原本時間軸で保存する。**追っかけには rendition
  を出さない。** 音声を別 rendition に分けると配信の形（master + 映像だけの
  セグメント + 別の音声）が変わり、追っかけの seek や再生位置の復元を確かめる
  判定が無いので、音声は従来の形のままにする
- **ライブの `extra_args` / `input_extra_args` では `-an` `-vn` `-sn` `-map` を拒否する。**
  ストリームの並びは `-var_stream_map` が持つ。並びを変えると ffmpeg が起動時に落ちる
- 未検証: 実放送の二重音声が `channel_configuration=2` + SCE 2 つの形か /
  実 Safari・iOS での切替（WebKit では取得する rendition が替わることまで確認）

### 録画中の追っかけ再生

録画中の `recordings` を、録画開始位置から現在の録画末尾まで HLS で追従再生する。
ライブ視聴と同じ `LiveStreamer` のセッション管理・`live.max_sessions`・idle GC・離脱
ヒントを共有するが、資源の同定子は録画の durable id である。

```
GET  /api/sites/{site}/recordings/{id}/chase/playlist.m3u8[?profile=<name>]
	       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/playlist.m3u8[?profile=<name>]
	       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/segments/{name}
GET  /api/sites/{site}/recordings/{id}/chase/{name}       （字幕付き master の variant / subtitle playlist）
	       → video/mp2t / text/vtt / application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/segments/{name}
GET  /api/sites/{site}/recordings/{id}/chase/offset/{offset}/{name}
	       → video/mp2t / text/vtt / application/vnd.apple.mpegurl
POST /api/sites/{site}/recordings/{id}/chase/leave
POST /api/sites/{site}/recordings/{id}/chase/offset/{offset}/leave
	       → 204（離脱のヒント）
```

これらは録画ファイル配信と同じく `openapi.yaml` には載せない。`{id}` は
`recordings.id` の十進正準形で、DB の `record_sync` から `(site, record_id, status)`
を逆引きする。録画行と同期行がどちらも `recording` のときは新しいセッションを開始
できる。正常終了した録画は、既に開始済みのセッションが保持する EVENT playlist と
セグメントを idle GC まで取得できる。ごみ箱・終了済みで保持セッションの無いもの・
失敗・未束縛・存在しない id は 404 である。URL の `site` は `cmd/rokuban` の site
束縛へルーティングするための値で、DB の録画 site と一致しない要求は 404 にする。

mirakc へは `GET /api/recording/records/{record_id}/stream` を Range なし・優先度
ヘッダーなしで要求する（オフセット省略時、従来どおり録画先頭から追従する経路）。
`{offset}` は録画開始からの 0 以上の整数秒である。省略は `0` と同じである。
オフセット付きでは
`GET /api/recording/records/{record_id}` の `recording.startTime` と `content.length` から
概算バイト位置を求める。TS パケット境界に合わせて `Range: bytes=<position>-` で要求する。
mirakc の Range 応答は要求時点までの有限のスナップショットである。
streamer は本文を読み切るたびに消費済みバイト位置から次の Range を要求して録画末尾へ追従する。
録画先頭からの読み捨ては行わない。録画終了との競合で 416/空の 206 と終了状態を
同時に観測した場合は、同じ位置を一度だけ再確認してから EOF とするため、終了直前に
追記された最終差分を読み残さない。

Range を無視してオフセット付き要求に 200（先頭からの本文）を返す mirakc は安全のため
受け付けず、オフセット再生を 503 にする。Range 対応は mirakc の録画配信 API における
seek + finite response の契約が必要である。現在の運用対象はこの契約を含む
`mirakc 4.0.0-dev.0` 系（実際に配備するイメージは同じ API 契約を満たす版に固定する）で、
古い版では `{offset}` を使わず従来の URL を使う。録画ファイルがまだ 0 バイトなら 204、
追従中に現在位置が末尾へ到達したら 416/空の 206 を返し得るため、いずれも録画終了まで
一定時間待って再試行する。起動待ちの上限（既存の playlist 起動上限 15 秒）を超えたとき
だけ 503/504 とする。204/416 の待機は上流接続失敗として数えない。

オフセットが現在の録画可能範囲以上なら新しいセッションを作らず 416 を返す。録画開始時刻・
コンテンツ長からの初期位置は可変ビットレート等の影響を受ける概算であり、開始後は通常の
HLS シークで補正できる。許容誤差は放送・エンコーダーごとに異なるため、固定値を API 契約
として保証しない。

追っかけの ffmpeg は通常ライブの「直近だけを残す」HLS と異なり、`EVENT` playlist、
`hls_list_size=0`、`temp_file` を使い、`delete_segments` を使わない。mirakc の入力が
EOF になれば `ENDLIST` を出し、ffmpeg が**正常終了した場合**は idle GC が回収するまで
playlist と全セグメントを保持する。これにより、録画完了直後にブラウザが最後の playlist /
segment を取りに来る窓を失わない。保持中は全プロファイルのプレイリストが残るので、
終了後でも再起動なしに `?profile=` を切り替えられる
（`TestFinishedChaseProfileSwitchServesRetainedPlaylists`）。ffmpeg が異常終了した場合は壊れたセッションを保持せず、
map とファイルを直ちに解放して次の playlist 要求で再起動できるようにする。

ライブと追っかけのセッション数は合算し、Prometheus の
`rokuban_live_active_sessions{kind="live"|"chase"}` で内訳を見る。セグメントの保存先は
どちらも `live.segment_dir` 配下で、録画バッファとは別の tmpfs / scratch に置く。追っかけ
は録画時間ぶんのセグメントを idle GC まで保持するため、同時視聴数と録画時間に応じた
容量を見積もる。詳細は [operations.md](../operations.md) §5 を参照する。

### 録画原本のブラウザ再生

完了済みで active な原本 MPEG-2 TS を、site streamer が FFmpeg → HLS の一時セッションとして
変換する。これは `media_assets` に保存する派生物ではない。資源同定は
`(recordings.id, offset)` で、profile は出力 playlist の選択にだけ使うため、同じ録画・同じ
offset の視聴者と profile 切替は 1 本の FFmpeg セッションを共有する。省略 offset と `0` は
同じ先頭セッションである。live / chase と同じ `live.max_sessions`、idle GC、離脱ヒントを使う。
Prometheus は `rokuban_live_active_sessions{kind="original_vod"}` に内訳を出す。

```
GET  /api/sites/{site}/recordings/{id}/original-vod/playlist.m3u8[?profile=<name>]
         → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/playlist.m3u8[?profile=<name>]
         → application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/segments/{name}
GET  /api/sites/{site}/recordings/{id}/original-vod/{name}
         → video/mp2t / text/vtt / application/vnd.apple.mpegurl
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/segments/{name}
GET  /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/{name}
         → video/mp2t / text/vtt / application/vnd.apple.mpegurl
POST /api/sites/{site}/recordings/{id}/original-vod/leave
         → 204（離脱のヒント）
POST /api/sites/{site}/recordings/{id}/original-vod/offset/{offset}/leave
         → 204（離脱のヒント）
```

`{offset}` は録画先頭からの秒を表す正準な 10 進整数である。`007`、`+5`、`5.0` などの
非正準形は 400、原本の長さ以上は 416 にする。offset ごとに session key と HLS scratch を分ける。
offset 付き playlist / segment / leave はすべて同じ offset の session を参照する。
各 offset は通常の原本 HLS セッションとして `live.max_sessions` に数え、leave ヒント・idle GC・
容量圧力時の idle session 退避も共有する。`offset/0` と offset 無しの URL は同じ session と scratch
を使う。

これらのバイナリ配信ルートは `openapi.yaml` に載せない。開始時に DB から同じ site の
`finished` 録画と `state='active'` の original を引く。原本を read-only で open してから、
同じ asset ID がまだ active で `rel_path` も一致することを DB で再確認する（open-then-verify）。
一致しなければ 404 にする。`rel_path` lock は取らず、streamer は media に何も書かない。

lock が要らない根拠は 3 つある。original の canonical は行の commit より前に rename で置かれる。
unlink は `MarkMediaAssetDeleting` の commit より後にしか起きない。live な行がある `rel_path` へは
別の書き手が公開できない（`media_assets_rel_path_idx`）。したがって再確認で active なら、
open した inode はその行の原本である（`TestOriginalVODVerifiesDBTargetAfterOpen`）。
trash・purge・supersede・失敗・原本不在・別 site / 未束縛 site は 404 にする。
その場合、セッションや DB 行を作らない。

開始後のセグメント要求は asset の状態を見ない。見るのは利用者の操作である録画のごみ箱・purge・
supersede だけで、該当すれば 404 にしてセッションと scratch を回収する。
FFmpeg は開いた inode を読み続ける。
エンコード完了直後の `until_encoded` 削除で canonical path が unlink されても、
視聴中の再生は止まらない。
テストは `TestOriginalVODRetainedSessionSurvivesOriginalDeletion` である。
この確認で DB が `ErrNoRows` 以外のエラーを返したときは、警告を出して配信を続ける。
再生を DB の可用性に依存させないためである。

FFmpeg は既存の live profile、音声 rendition、任意の WebVTT 字幕設定を使う。
**playlist は `-hls_playlist_type event` にする。** `vod` は playlist を FFmpeg の終了時にしか書かない。
ffmpeg 9.0.2 で 20 秒の MPEG-2 TS を実時間で流すと、vod は変換中 `.m3u8` が 0 個で終了後に初めて出た。
この形だと、15 秒で変換が終わらない録画はすべて playlist 待ちの 504 になる。
event は変換の先頭から playlist が書かれ、終了時に `#EXT-X-ENDLIST` が付く。
`-hls_list_size 0` と `temp_file` を使い、`delete_segments` は付けない。
変換中のシーク可能範囲は変換の先端まで伸びていき、末尾まで届くのは変換の終了後になる。
正常終了後も、`#EXT-X-ENDLIST` と全 segment を idle GC まで保持する。
シークや遅れて届く segment 要求には、この保持したファイルで応える。
異常終了時は scratch とセッションを回収する。
原本が引き続き active なら次の要求で作り直せる
（`TestOriginalVODServesPlaylistWhileFFmpegIsStillConverting`）。

offset 付きでは完成済み原本の「映像の長さ」を同じ開いたファイル記述子から ffprobe で調べ、FFmpeg に渡す前に
範囲を確認する。原本は `cmd.ExtraFiles` の先頭として子プロセス fd 3 に渡し、FFmpeg は
`/dev/fd/3` を入力にする。fd は seek 可能な同じ inode を指すため、`until_encoded` が canonical path
を unlink した後も変換できる。通常のパイプ `pipe:0` では seek が効かないため、この fd 方式を選ぶ。
ffprobe が descriptor を読み終わった後は親側で先頭へ戻し、FFmpeg の fd も seek 可能な状態から始める。
offset が 0 より大きいときは `-ss {offset}` を `-i /dev/fd/3` より前に置く。FFmpeg の入力側 seek は
入力の seek point から offset までを decode して捨てる。`-copyts` は付けず、HLS の再生時間軸は
offset ごとに 0 から始めるので、再生位置は `offset + currentTime` として扱う。

**範囲の判定は format の duration でなく映像ストリームの終端で行う。**
format の duration は音声など最長のストリームで決まり、映像より長い。
実バイナリで測った合成 660 秒 TS は format 660.010 秒・映像 660.000 秒で、
format と比べる判定では、`[映像の終端, format の duration)` に入る整数 offset（660）が通った。
FFmpeg は何も出力せず、playlist 待ちの 15 秒後に 504 になった。
いまは映像の終端（`start_time + duration - format の start_time`）の手前 0.5 秒より後ろの offset を 416 にする。
終端ちょうどの offset も最後のフレームより後ろを指して出力が空になりうるので、この余白を取る。
同じ 660 秒 TS で offset 659 は 200（0.14 秒）、660 / 661 / 99999 は 416（0.03 秒）だった。
範囲外の offset は利用者入力の結果なので ERROR ではなく INFO で記録する。

**原本 VOD の退避は mirakc のチューナー解放待ち（`liveMirakcReleaseWait`、5 秒）を挟まない。**
原本 VOD はチューナーを持たない。`max_sessions: 2` で容量が埋まった状態からのシークは、
待ちを挟むと約 5.4 秒、挟まないと 0.13 秒だった（実バイナリ、合成 660 秒 TS）。

#### offset seek の精度と開始時間の測定

精度と範囲は CI の `TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange` が固定する。
このテストは実 ffmpeg / ffprobe で `runSession` の経路を通り、`ROKUBAN_REQUIRE_FFMPEG` を立てると skip できない。入力は lavfi で作る 40 秒の MPEG-2 + MP2 の TS で、
映像の輝度が録画先頭からの秒に比例し、音声は 41.5 秒まで続く。HLS の先頭セグメントの先頭フレームの輝度から時刻を読む。
FFmpeg 9.0.2、macOS arm64 で offset 0 / 7 / 20 / 38 の誤差はすべて 0.00 秒だった。
offset 40 / 41 / 42 は 416 が 3 秒以内に返る。
format の duration と比べる判定に戻すと、40 と 41 が 15 秒待って 504 になった（変異で確認）。
この条件は 40 秒の合成映像だけで、PTS の不連続・wraparound を含む放送 TS の実録画では未検証である。

HLS の先頭セグメントの映像 PTS は、どの offset でも 1.48 秒だった（上のテストのログ）。
mpegts muxer の既定の遅延で、offset によらない。ブラウザの `currentTime` は 0 から始まる。
HLS の PTS 自体を 0 にそろえているわけではない。

10 分以上の入力での精度は 660 秒の合成 TS で 1 回測った。
25 fps の 320×80 映像に録画先頭からの `秒:フレーム番号` を焼き込んだ。
映像は MPEG-2（GOP 15、B-frame 2、closed GOP）、音声は MP2 で、1 Mbit/s MPEG-TS muxrate でまとめた。
format duration は 660.010022 秒、video stream duration は 660.000000 秒である。
測定に使った runner は不変条件 4（ffmpeg の exec は worker / streamer のみ）を破るため、リポジトリには置いていない。
再実行できる形では残っていない。

| 要求した offset | HLS の先頭フレーム表示 | offset との差 |
| ---: | ---: | ---: |
| 0 秒 | 0.00 秒 | 0.00 秒 |
| 61 秒 | 61.36 秒 | +0.36 秒 |
| 307 秒 | 307.32 秒 | +0.32 秒 |
| 603 秒 | 603.20 秒 | +0.20 秒 |
| 659 秒 | 659.36 秒 | +0.36 秒 |

同じ 5 offset を各 3 回 FFmpeg で変換し、原本 open 後から master playlist が読めるまでを測った。
非ゼロ offset は duration probe と FFmpeg 起動を含む。DB lookup、HTTP/router、原本 open は計時外である。

| offset | master ready 中央値（最小–最大） |
| ---: | ---: |
| 0 秒 | 80 ms（78–93 ms） |
| 61 秒 | 94 ms（92–96 ms） |
| 307 秒 | 93 ms（93–93 ms） |
| 603 秒 | 93 ms（93–95 ms） |
| 659 秒 | 59 ms（59–60 ms） |

非ゼロ offset の duration probe は 17–20 ms だった。659 秒では残り 1 秒の短い出力になる。

原本だけの完了録画はこの経路をブラウザ再生の既定にする。active な録画は既存の chase、
active な encoded がある完了録画は MP4 progressive + Range を使う。原本 MPEG-2 decoder を
持たない FFmpeg build は `live.enabled` の streamer 起動前に拒否し、再生要求後に空の HLS
player だけが出る状態を避ける。

原本 VOD の HLS scratch も `live.segment_dir` に置き、録画バッファ / archive から分離する。
正常終了後も idle GC まで録画全体の変換済み segment が残るため、録画時間、全 live profile の
出力 bitrate、同時セッション数を掛けて tmpfs 容量を見積もる（[operations.md](../operations.md)
§5）。

### SPA アセット配信

go:embed 配信でハッシュ付きアセット immutable + それ以外 no-cache のヘッダーを正しく付ければ十分（参照: [frontend.md](../frontend.md)）。本気の配信最適化は S3+CDN 経路の仕事。ここに nginx キャッシュを挟むと配信経路が 3 つになり、テストマトリクスが増える割に得るものがない。

### サービスロゴ: ドロップ

mirakc は起動中の局ロゴ抽出をサポートせず、運用者が事前抽出したファイルを静的登録して配るだけの機構しか持たない。Rokuban 側で再取得・自前配信する価値が薄いため実装しない（[data.md](../data.md) の「サービスロゴ: ドロップ」参照）。
