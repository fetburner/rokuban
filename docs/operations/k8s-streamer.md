> [operations.md](../operations.md) §5「streamer のスケール」。索引から辿る。

### streamer のスケール

streamer は録画配信とライブ視聴を担い、置き場所もスケールの仕方も別である。ロールは分けず、デプロイのパラメータだけを変える。

ロールを決めるのは「ソケットを持ち続けるか」だけで、mirakc を触るかどうかは判定軸に入らない（[overview.md](../overview.md) §ロール分類の基準）。同じロール・同じバイナリで置き場所を変える点は、`worker.queues` でキューごとに置き場所が決まるのと同じ構造である。

| | 置き場所 | レプリカ | 前段 | イメージ |
|---|---|---|---|---|
| 録画配信 / サムネイル | メディアストレージの隣 | 水平（N） | 素の round-robin | `Dockerfile.full`（共有 config が `live.enabled: true` で、起動時に ffmpeg を検査するため） |
| 世代付き録画 HLS | メディアストレージの隣 | 既定 1 | 単一 Pod（増やすときは録画 id の consistent hash） | `Dockerfile.full` |
| ライブ視聴 | mirakc に到達できる場所（WAN 越しの pull を許す） | 既定 1 | `(site, networkId, serviceId)` の consistent hash | `Dockerfile.full` |

単一ホスト名の入口は、標準 Ingress の `Exact` / パス要素単位の `Prefix` だけで Service が一意に決まることを判定基準にする。録画配信は現行の `/api/media/recordings`、世代付き録画 HLS は `/api/media/stores/<store>` の Prefix で分ける。ライブは有限の site 名ごとに `/api/sites/<site>/networks` を Prefix 化して streamer Service へ振り分け、既存の URL を維持する。

世代付き録画 HLS の変換とキャッシュはプロセス内にある。同じ録画の master・playlist・セグメントの要求は同じ `rokuban-recording-hls` Pod に届く必要があるため、Deployment は 1 replica にする。置き場を追加するときは、その置き場の Prefix と専用 Pod / Service の組を追加する。replica を増やすときは、固定深さの URL から録画 id を取り、前段の consistent hash で同じ録画を同じ Pod に送る。cookie や ClientIP affinity は使わない。

この経路を載せるかどうかは argv で決める。`recording_hls.enabled` は全 Pod が共有する config のキーなので、config では Pod ごとに切り替えられない。streamer ロールは `recording_hls.enabled` なら既定で載せ、`--no-recording-hls` で外す。中央 streamer と site ごとのライブ streamer はこの引数を付ける。専用の tmpfs とメモリ予算を持たない Pod にも、変換とキャッシュの状態が載ってしまうからである。ingress はこの経路を `rokuban-recording-hls` にしか送らない。単一ホストで `server --all` を使うときは何も付けず、API が返す URL をそのプロセスが応答する。

streamer は 1 プロセスが N サイトを束縛できる。`live.enabled: true` に束縛サイト数の制約は無く、0 サイト束縛（中央の録画配信 Deployment）はライブのルートを持たないだけで他のロールは通常どおり動く。`deploy/k8s/` では中央 streamer と site ごとのライブ streamer を別 Pod として出荷し、overlay が単一 Ingress に site 名を具体化した Prefix を追加する。

WAN に乗せてよいのは、失っても再取得できるストリーム（ingest の pull、ライブの pull）だけである。録画ストリームは乗せない（各サイトの mirakc がローカルに書くため）。[recording/reference.md](../recording/reference.md) §mirakc の多段集約と同じ軸である。素の TS の帯域目安は地上波 約 17 Mbps / BS 約 24 Mbps（1 セッションあたり、規格値）である。

<details><summary>根拠</summary>

- streamer は 1 プロセスで N サイトを束縛できる: `cmd/rokuban/server.go`

</details>

#### ライブのパッケージングを MediaMTX に委ねる形

ライブのパッケージングを MediaMTX に委ねる形（opt-in）では、MediaMTX と publisher を同じコンテナに置く。

publisher は `runOnDemand` が子プロセスとして起動する rokuban のコマンドである。したがって MediaMTX のコンテナに rokuban・ffmpeg（VAAPI ならデバイスも）が要り、そこから mirakc に到達できる必要がある。前段の `(site, networkId, serviceId)` の consistent hash と既定 replicas=1 の可逆性は変わらない。

- 未解決: 前段の書き換えを上記の入口の判定基準で表せない。前段は全要求の接頭辞を MediaMTX のパスへ書き換える（[api/media.md](../api/media.md) §「パッケージャは MediaMTX の LL-HLS を選べる形にする」）。標準 Ingress の `Exact` / `Prefix` は書き換えを持たないので、書き換えを担う前段をどこに置くかを決める必要がある
- MediaMTX の HTTP を前段の外へ直接公開しない。アプリに残る唯一のセキュリティ要件である Host ヘッダー検証（[overview.md](../overview.md) §認証）を通らない
- 配信を外に出す利益は、負荷の軽減ではない。現行経路の streamer Pod の CPU は、視聴者 1 人（30 分）で中央値 353m、3 人（60 秒）で 374.5m だった（+6%）。支配的なのはエンコードで、配信負荷は支配的でない

#### 録画配信はセッション親和性を必要としない

録画配信の Service と Ingress には sticky を書かない。録画配信は完全にステートレスで、貼り付けるべき状態が無い。

録画配信は DB からアセットを解決し、標準ライブラリのファイル配信で返す。sticky を書くと、数 GB の Range 読みという最も分散させたい経路で分散されなくなる。X-Accel-Redirect でバイト転送を前段に委ねる構成とも噛み合わない。

アーカイブは単一である（[§4](storage.md) / [docs/storage.md](../storage.md) §5 の 2 階層。録画バッファはサイトごとのエッジ、アーカイブは 1 つ）。したがって**複数サイト構成でも録画配信の Pod は site 非依存**で、どの Pod でもどの録画を配れる。録画 ID は代理キーなのでサイト間で曖昧にならず、URL に site を持つ必要もない。

再生中に Pod が落ちてもクライアントは Range で再接続して継続する。決めるべきは `terminationGracePeriodSeconds` を進行中の転送を打ち切らない長さにすることだけである。

<details><summary>根拠</summary>

- 録画配信は DB からアセットを解決して配る: `http.ServeContent` / `internal/streamer`
- 録画 ID はサイト間で曖昧にならない: `recordings.id`

</details>

#### ライブ視聴は mirakc の隣、シャード鍵はサービスの同定子

ライブのトランスコードは mirakc と同じサイトで行い、HLS になったものだけをサイトの外へ出す。生 TS は 17 Mbps 連続なので WAN に出さない。

セッションを Pod に貼り付ける代わりに、**前段でサイト・ネットワーク・サービスの組の consistent hash によって振る**。この鍵は既に資源同定の中にある（`/api/sites/{site}/networks/{networkId}/services/{serviceId}/...`。[api.md](../api.md) §ライブ視聴の HLS）。SI の service_id は network をまたぐと一意でないので、鍵も 3 項になる。鍵が資源同定にあるので、レプリカを増やしても URL もクライアントも変わらない。

cookie による affinity は使わない。外部プレイヤー（VLC 等）は cookie を持たない。OSS nginx に `sticky cookie` が無い（nginx-plus の機能）。ingress-nginx の既定 `affinity-mode: balanced` はスケール時に生きている Pod からもクライアントを外す。

同じチャンネルの視聴者は同じ Pod に落ちるので、ffmpeg 1 本・チューナー 1 本を共有する。チューナーは録画と取り合う唯一の共有資源である。これは偶然の利益ではない。鍵をサービスの同定子に取る理由そのものである。

Pod が落ちても、ハッシュの担当が移っても自己修復する。視聴者の再要求が新しい Pod に落ちてそこで ffmpeg が起き、旧 Pod は要求が来なくなるので idle GC がチューナーを解放する。クライアント側にセッションの概念が無いので、見えるのは再バッファリングだけである。

#### 既定を 1 にする根拠と、増やす判定基準

ライブ視聴の streamer は既定を 1 レプリカにする。天井になるのはサイトのチューナー数である。

ライブ 1 本がチューナー 1 本を占有し、残りが録画に使われる。Pod の CPU は天井にならない。

1 サイトのチューナー数ぶんのトランスコードが 1 Pod に収まる間は、増やす理由が無い。収まらなくなったら replica を増やし、前段に `(site, networkId, serviceId)` の consistent hash を設定するだけでよい。URL・クライアント・API は変わらない。この可逆性を保つために実装側で守るのは次の 3 点である。

1. プレイリストとセグメントの URL を固定深さにする。前段が 1 つの nginx 変数で鍵を取り出せる形にする。OSS nginx なら `map` で鍵を取り出し、`hash $live_key consistent;` で振り分ける。`map` は `map $uri $live_key { ~^/api/sites/(?<s>[^/]+)/networks/(?<n>[^/]+)/services/(?<sv>[^/]+)/live/ "$s/$n/$sv"; }` と書く。可変長パスやクエリ文字列に鍵を置くと書けない（ingress-nginx の `upstream-hash-by` で同じキャプチャがどう書けるかは未検証）
2. 同時セッション上限はプロセスローカルであることを前提にする。グローバルな天井はチューナー数であり、その裁定者は mirakc である。「アプリが握る唯一のグローバル上限」を作ると、レプリカを増やした時点で偽になる
3. セッション数のメトリクスは per-process gauge にする。Prometheus 側で sum する。1 Pod の値を全体として読む UI を作らない

要求レートの実測値は、`segment_seconds: 2`・映像プロファイル 1 本・視聴者 1 人で playlist 0.5 req/s とセグメント 0.5 req/s である。`segment_seconds` を 1 へ詰めると 1.4 req/s と 0.7 req/s になる。増えるのは要求数であって、Pod を増やす理由にはならない。ここに記録するのは、セグメント長を短くする判断が要求数を約 2 倍にすることを、スケールの見積もりで読み違えないためである。

#### ライブのセグメントを録画バッファと同じディスクに置かない

ライブ HLS のセグメントは tmpfs に置き、録画バッファと同じディスクには書かない。同じディスクに書くと、視聴が録画を妨げる。

エッジの録画バッファ（mirakc `recording.basedir`）は I/O が飽和するとドロップへ直結し、要求されるのはレイテンシで、絶対帯域は問われない（[§4](storage.md)）。これは Postgres datadir と encode scratch を分ける指針（[§3](database.md)）と同じ規則である。ライブ・追っかけ・原本 VOD のどの経路でも、scratch を `recording.basedir` や archive PVC と同じ mount / volume に置かない。

ライブ・追っかけ・原本 VOD は同じ `live.segment_dir`（Pod の `live-segments`）を共有し、同じ process-local な `live.max_sessions` を数える。メトリクスは `rokuban_live_active_sessions{kind="live"}` / `{kind="chase"}` / `{kind="original_vod"}` に分かれるが、scratch 容量は合算する。

##### 容量の見積もり

容量はセッションの種類で決まり方が違う。ライブは窓の分だけ、追っかけと原本 VOD は録画全体の分が要る。

k8s では `emptyDir: {medium: Memory}` に `sizeLimit` を設定する。ライブが保持するのは `playlist_size` × `segment_seconds` 秒の窓だけなので、容量は窓の秒数 × 全 profile の bitrate の合計 × 同時セッション数 ÷ 8 で見積もる。追っかけと原本 VOD は録画全体の EVENT セグメントを idle GC まで保持する。容量は録画時間 × 全 profile の出力 bitrate の合計 × 同時セッション数 ÷ 8 で見積もる。どちらの式も、bitrate が bit/s なら結果はバイトである。字幕 rendition を有効にする構成では WebVTT の scratch も加算する。この見積もり式はここだけに置き、[api/media/playback.md](../api/media/playback.md) はここを参照する。

同梱設定（`deploy/k8s/base/config.yml` の h264 profile）は CRF も bitrate も指定しない。したがって出力 bitrate は設定から決まらず、入力の絵柄で変わる。測れたのは 1 点だけである。

測定の条件は次の通りである。入力は地上波の NHK 総合（1440x1080i の MPEG-2、音声 ES 1 本）で、自宅 mirakc から 150 秒取った。変換は追っかけと同じ ffmpeg 引数（720p の libx264 veryfast、音声 3 rendition、2 秒セグメント、`delete_segments` 無し）で、FFmpeg 9.0.2（macOS）を使った。出力 HLS ディレクトリは合計 41,776,725 バイトで、約 2.2 Mbit/s（2,225 kbit/s）である。

この値だと `sizeLimit: 1Gi` は 1 セッションで約 64 分、`live.max_sessions` の 4 本が同時に追っかけると 1 本あたり約 16 分で満杯になる。測っていないのは、動きの多い番組・BS（測った site に BS が無い）・民放（測った時点でチューナー 1 基が録画中で、同じ mux の NHK 総合しか取れなかった）である。この値は約 2.2 Mbit/s の 1 点の実測から出した。同梱設定では 64 分を超える録画は、1 本だけでも追っかけと原本 VOD が途中で失敗する。原本 VOD は完了録画の既定の再生経路である（[api/media/playback.md](../api/media/playback.md) 「既定の再生経路と容量」）。対処は `sizeLimit` を上げるか `live.max_sessions` を絞ることで、天井はノードのメモリ（参照クラスタは約 4Gi）である。

##### sizeLimit は満杯で止まる値として置く

上限はノードのメモリ予算に収め、満杯になったら書き込みが失敗する値にする。Pod を落とす値にはしない。

Kubernetes の [volumes 文書の emptyDir 節](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir) は、`medium: Memory` なら tmpfs を mount すると書く。そこに書いたファイルは、書いた container の memory limit に算入される。同じ節は、サイズ未指定の memory-backed volume はノードの allocatable メモリの大きさになると書く。

`sizeLimit` を指定すると tmpfs の大きさがその値になる、という読みは文書の文面からの推論である。次の測定で確かめた。

参照クラスタは K3s v1.36.2+k3s1、containerd 2.3.2（amd64、Linux 6.12.94）である。`sizeLimit: 16Mi`、container memory limit `256Mi` の Pod に `dd` で 32Mi を書くと、16Mi 書いた後に ENOSPC で終了した。Pod は Running のままで、eviction は観察されなかった。観察時間は記録していない。クラスタの CSI driver は `nfs.csi.k8s.io` と `driver.longhorn.io` だが、memory `emptyDir` は CSI を通らずノード内 tmpfs を使う。この測定で確かめたのは小さい上限を超えたときの挙動だけで、ノード全体のメモリ圧迫による eviction は測っていない。

ENOSPC で止まるので、上限を見積もりより大きく取ってセッション数で先に止める必要は無い。ただし ENOSPC はそのセッションに閉じず、`live-segments` を共有する同じ Pod の全セッションに及ぶ。追っかけや原本 VOD が `sizeLimit` を使い切ると、同じ Pod のライブもセグメントを書けない。そのとき書き込みに失敗したライブの ffmpeg が終了するか書き込みを再試行するかは、未検証である。

memory limit は `sizeLimit` + 512Mi（ffmpeg の実測 peak 173Mi の約 3 倍）× `live.max_sessions` で決める。tmpfs のページは container の limit に算入されるので、`sizeLimit` を limit の内側に含める。同梱の 4 セッションなら 1Gi + 4 × 512Mi = 3Gi である。memory request は、普段は空の tmpfs を予約しない。実測 peak × `live.max_sessions` にアプリの 128Mi を足した約 1Gi にする。

##### 原本 VOD の scratch

原本だけの完了録画は、原本 TS を読み取り専用で開き、変換した HLS だけを scratch に書く。

開くのは site streamer で、media PVC の mount は read-only のままでよい。録画パスのロックは取らず、開いてから DB で再確認するので、media root にロック用ディレクトリを作らない（根拠は [api.md](../api.md) 「録画原本のブラウザ再生」）。原本 VOD は録画全体の EVENT playlist と全 segment を idle GC まで保持する。`sizeLimit` を超えた書き込みの失敗は、この Pod の他セッションにも及ぶ（上記）。原本や録画バッファへは書き込まない。

<details><summary>根拠</summary>

- 原本 VOD は録画パスのロックを取らない: `rel_path` / `.rokuban-locks`

</details>

##### 世代付き録画 HLS の scratch

キャッシュの下限は式で決まり、tmpfs の大きさは見積もりと未検証の余白で決めている。

世代付き録画 HLS は専用 Pod の tmpfs（`recording_hls.segment_dir`、Pod の `recording-hls-scratch`）に 2 つを置く。バイト上限付きのセグメントキャッシュ（`recording_hls.cache_bytes`）と、変換ごとの作業 scratch（ffmpeg の出力がキャッシュへ公開されるまでの置き場）である。`live.segment_dir` とは別の volume で、ライブ・追っかけ・原本 VOD の scratch とは合算しない。

キャッシュの下限は `max_concurrent × (先読み 5 + 要求 1) × 最大 5 rendition（映像・音声 3・字幕）× 16 MiB` である。`cache_bytes` はこの値より大きくし、起動時の検証が下回る設定を弾く。同梱設定は 3 × 6 × 5 × 16 MiB = 1440 MiB に対して 1536 MiB である。既定を 3 にしたのは、追従変換 2 本では 3 人目の視聴中に idle 変換を止めて起動し直す往復が起きるためである。16 MiB は下限を計算するための見積もりで、実セグメントの上限ではない。見積もりを超えるセグメントも配信は止めず、キャッシュ全体より大きい場合だけエラーにする。この式はここだけに置き、[playback.md](../api/media/playback.md) と `config.example.yml` はここを参照する。

同梱の `sizeLimit: 2Gi` は、キャッシュの 1.5Gi に 512Mi の余白を足した値である。作業 scratch の実測値は無い（未検証）ので、この余白は見積もりにとどまる。memory limit の `3584Mi` は、`sizeLimit` + 512Mi × 同時変換数（2Gi + 3 × 512Mi）で決めた。512Mi はライブの ffmpeg の実測 peak に由来し、この経路の ffmpeg の peak は未測定（未検証）である。tmpfs が満杯になったときにこの経路の変換とキャッシュがどう振る舞うかも未検証である。

##### 追っかけ再生の scratch

録画中の追っかけ再生は、同じ streamer 内でセッションを共有し、録画開始から EOF までの変換結果を scratch に保持する。

- URL は固定深さで、開始位置を指定する場合は `/offset/{offset}` を含む。
- site を前段のルーティングキーにして、録画を担当する site の streamer へ送る。標準の Ingress では各 site overlay が `/api/sites/<site>/recordings` をその Service へ追加する。
- 録画 id は DB 内の mirakc record id を隠す公開キーで、同じ streamer 内のセッションを共有する。同じ録画の異なる開始位置は別セッションになる。
- hash key は `(site, recordings.id)` とし、同じ録画の全オフセットを同じ Pod に固定する。

`delete_segments` は使わず、ffmpeg 終了後も idle GC まで全セグメントを残す。録画状態が終了へ変わった後も、既存のセッションはこの保持ファイルを配信する。容量不足は録画バッファへ移さない。`sizeLimit` を超えた書き込みの失敗は、この Pod の他セッションにも及ぶ（上記）。

