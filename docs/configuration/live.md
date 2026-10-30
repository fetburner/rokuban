> [configuration.md](../configuration.md) §スキーマ構造のうち、live と HW エンコードの節。索引から辿る。

### live

ライブのルートを登録するのは streamer ロールだけである。セグメントの書き出し先は既定値が無く、有効なら必須で、複数プロセスで共有しない。同時セッション数の上限はプロセスごとの値で、全体の天井ではない。

無効（既定）なら、ffmpeg の無い公式イメージで streamer を起動する構成（録画配信とサムネイルのみ）を壊さない（不変条件 4）。

`live.enabled` だけは `GET /api/capabilities` の `live` にも出る。フロントは config を読めず、これが無いと無効な機能への入口を出し続けるためである。ロール分割構成でも config ファイルは共有する（[keys.md](keys.md)「関心事別にセクションを切る」）ので、値はそのまま公開面に写し、streamer には問い合わせない（不変条件 1）。

この値を返すのは api ロールのプロセスに限らない。REST の生成ルートはロールで絞られないので、HTTP を持つどのプロセスに聞いても同じ答えになる。そうしないとロールごとに答えが割れる（[api/rest.md](../api/rest.md)「機能の有効/無効は能力 API で観測する」）。答えが表すのは常に「この config が有効か」であり、**「api の config では有効だが streamer を動かしていない」構成では入口が出る**（押した先はプレイリストの 404）。

#### セグメントの書き出し先

書き出し先は、録画バッファとは別ディスクにする。

`live.segment_dir` が空だと起動時エラーにする。適当なローカルパスへ気づかないうちにフォールバックすることはしない。録画バッファ（mirakc `recording.basedir`）とは別ディスクにする（tmpfs 前提。k8s なら `emptyDir: {medium: Memory}`）。

起動時（HTTP リスナーが立つ前）にこのディレクトリの中身を掃く。tmpfs はノード再起動でしか消えないため、前回プロセスの残骸（SIGKILL 等で SIGTERM の後始末が効かなかった場合）が毎起動で必ず消える。`segment_dir` 自体は消さない。k8s の `emptyDir` を直接マウントする構成では、Linux はマウントポイント自体への rmdir を EBUSY で拒む（Linux コンテナで実測済み）。中身だけを掃けば毎起動 Warn ログは出ない。推奨値 `/dev/shm/rokuban-live` のように tmpfs のサブディレクトリを使う構成では、そもそもマウントポイントではないのでこの問題は起きない。

**複数プロセス / 複数レプリカで共有しない。** 起動時の掃除は「自分がこのパスの唯一の書き手である」という前提に立つ。共有すると後発の起動が先発の生きているセッションディレクトリを消す（視聴は一度途切れるが、次のプレイリスト要求で新しいセッションが起きて自己修復する）。

#### セッション・猶予・優先度・プロファイル

離脱ヒントの猶予は導出し、優先度は録画より低く保つ。

`live.max_sessions` は、このプロセスが同時に持てるライブセッション（≒ ffmpeg プロセス）数である。プロセスローカルな上限であり、グローバルな天井（チューナー数、mirakc が裁定）ではない（[operations.md](../operations.md) §5「既定を 1 にする根拠」）。

`live.idle_timeout` はサービス単位の idle GC 猶予で、この時間セグメント要求が来なければ ffmpeg を止める（クライアント 1 人ごとの生存は追わない）。離脱ヒント（`POST .../live/leave`）を受けたときに詰める短い猶予は、設定キーにしない。`3 × 最長の live.profiles[].segment_seconds + 2s`（既定 8 秒）で導出する。

この猶予は `idle_timeout` ではクリップしない。クリップすると `segment_seconds: 6` + `idle_timeout: 2s` のような設定で猶予がセグメント長を下回り、leave が「他人の視聴を切る道具」になってしまう。猶予が `idle_timeout` 以上になる設定では、ヒントは詰める先が「いま」より後ろにならないので no-op になる（起動を止めるより運用者の意思を尊重する）。

猶予は「生きている視聴者の次の要求が来るまでの間隔」より長くなければならない。その間隔を決めているのはセグメント長そのものなので、独立したキーにすると 2 つの値が矛盾する組み合わせを運用者が書けてしまう（[api.md](../api.md) §ライブ視聴の HLS「離脱は『ヒント』であって停止命令ではない」）。[basics.md](basics.md)「config と DB の境界」と同じく、別の値から必ず導けるものはキーにしない。

`live.tuner_priority` は mirakc への X-Mirakurun-Priority である。チューナーが足りないときに録画を勝たせるため、ruler が生成する schedule の既定 priority（10）より低くする（[recording.md](../recording.md) §2「チューナー調停」）。**Rokuban はこの不等式を強制しない。** ルールの priority は DB 側（`rules.priority`、既定 10）でユーザーが自由に変えられ、ライブ側は config 側なので、両者を跨いで比較検証する権威がどちらにも無い。ユーザーがルールの priority を 0 や 1 まで下げると、この既定値のままではライブが録画に勝ってしまう。運用者が両方の値を意識して選ぶ前提とする。

`live.profiles` は `encode.profiles` とは別の構造体である。HLS はセグメント長・プレイリスト長という VOD には無い制約を持つ。`name` は `?profile=` から参照する一意な名前で、英数字・ハイフン・アンダースコアのみ（セグメントファイル名の接頭辞に使うため）。ISDB-T の映像は MPEG-2 でブラウザの HLS 経路では事実上再生できないため、H.264 への変換が前提となる。この並びが `/live` の画質セレクタの並びで、先頭が `?profile=` 省略時の既定になる。一覧は `GET /api/live-profiles` が返し、名前と `height` だけで、コーデック・品質・extra_args は出さない。

### encode/live の HW エンコード

構造化するのは、追加引数では届かない位置、つまり入力より前のオプションとアプリが握るスケール filter だけである。コーデック指定より後ろのオプションは既存の追加引数で足りるので構造化しない。

`extra_args` が既に出ている位置は既存のキーで足りる。`-global_quality / `-cq` / `-q:v` のような品質オプションもこれに当たり、キー化しない。

| キー | 説明 |
|---|---|
| `encode.profiles[].scaler` / `live.profiles[].scaler` | スケール filter の系統名 |
| `encode.profiles[].deinterlace` / `live.profiles[].deinterlace` | インターレース解除の系統スイッチ |
| `encode.profiles[].qp` / `live.profiles[].qp` | `-qp`（品質指定） |
| `encode.profiles[].hwaccel` | プロファイル毎の `-i` 前置ブロック |
| `live.hwaccel` | `live:` 直下の `-i` 前置ブロック |
| `encode.profiles[].input_extra_args` / `live.input_extra_args` | `-i` 直前の追加引数 |
| `encode.profiles[].extra_args` / `live.profiles[].extra_args` | 既存キー（改名なし） |
| `encode.profiles[].subtitles` | `webvtt` のみ |
| `encode.profiles[].cut` | カット版を出力するか（[cut.md](cut.md)「カット版」） |
| `live.captions` | WebVTT 字幕 rendition 付きの master にするか |

字幕は、VOD では `encode.profiles[].subtitles` が原本の ARIB 字幕を encoded ファイル隣の `.vtt` サイドカーに出力する。ライブでは `live.captions` を `true` にすると、全プロファイルを 1 つの master playlist にまとめ、WebVTT 字幕 rendition を出力する。字幕なし番組では映像・音声のみの master を出力する。どちらも libaribcaption 入りの ffmpeg が必要で、ライブは無ければ起動エラーになる。複数プロファイルの `segment_seconds` / `playlist_size` は同一値が必要である。

起動エラーになる組み合わせは次の通り。矛盾していて実行時に決められない設定である。

- `crf` と `qp` の同時指定（優先順位を実行時に決めさせない）
- 未知の `scaler`
- `height` が 0 なのに `scaler` を書く
- `hwaccel` ブロックがあるのに `kind` が空
- `crf`・`qp` の負値

#### scaler・deinterlace・hwaccel

filter の文字列は直接書かせず、系統名と真偽値で指定する。hwaccel は VOD ではプロファイルごと、ライブでは live セクション直下に置く。

`scaler` は系統名を取り、`-vf` の filter 文字列そのものは受け付けない。既定は `""`（= `software`）で、許す値は `software` と `vaapi` のみである。qsv / cuda はこの環境の ffmpeg で `scale_qsv` / `scale_cuda` の綴りを確認できておらず未検証のため、除外してある。`height` が 0 のときに書くと起動エラーになる。

`deinterlace` の既定は `false`（現行互換）である。`true` なら `scaler` から software=`yadif` / vaapi=`deinterlace_vaapi` を導出し、scale があれば解除の後ろに連結する。bool にして filter の実体を `scaler` から導出するのは、矛盾した設定を表現できないようにするためである。`scaler: software` なのに `deinterlace_vaapi` を書く設定や、`-vf` を別名で自由に書く設定がその例である。`deinterlace: false`（省略）のときは、生成する argv を従来から変えない。

`encode.profiles[].hwaccel` は、プロファイル毎の `-i` 前置ブロックで、`kind` は必須、`device` と `output_format` は任意である。VOD はプロファイルごとに入力を開き直すため、プロファイル単位で持たせられる。`live.hwaccel` は `live.profiles[]` の中には置けず、`live:` 直下に置く。ライブは 1 回の ffmpeg で入力 1 本・出力 N 本なので、プロファイル毎に持たせると「プロファイル 2 つが別の hwaccel を要求する」という表現できない設定が書けてしまう。セクション直下に置けば、それが表現不可能になる。

`encode.profiles[].input_extra_args` と `live.input_extra_args` は、`-i`（VOD）/ `-f mpegts -i pipe:0`（live）の直前に追加する引数である。

device ノードのマウントはデプロイ側の仕事である（k8s `resources.limits` / Docker `--device`）。`hwaccel.device` の存在は起動時に検査しない。検査すると公式イメージや device の無い CI が落ちる。無い device を書いたプロファイルは、ジョブ / セッションの失敗として現れる。

#### argv の順序

ffmpeg の argv は、アプリ所有の先頭、入力側の引数、入力、変換、出力側の引数、アプリ所有の末尾の順に並ぶ。

VOD の順序は次の通り。

```
-hide_banner -nostats -y                                      # アプリ
[-hwaccel K] [-hwaccel_device D] [-hwaccel_output_format F]   # hwaccel ブロック
[input_extra_args…]                                            # ユーザー（入力側）
-i INPUT                                                       # アプリ
-c:v VC -c:a AC
[-vf <deinterlace[, scaler が決めた scale]>]                   # deinterlace=true または height>0 のときだけ、常に 1 個
[-crf N | -qp N] [-preset P]
[extra_args…]                                                  # ユーザー（出力側）
-f CONTAINER -progress pipe:1 -loglevel error OUTPUT           # アプリ所有の末尾
```

ライブは同じ規則を入力 1 本・出力 N 本に展開する。入力側は `live.hwaccel` → `-probesize`/`-analyzeduration` → `live.input_extra_args` → `-f mpegts -i pipe:0` と並ぶ。そのあと、プロファイルごとに `-map`（映像 1 本と音声 3 本）→ `-c:v`/`-c:a` → 主 / 副の `-filter:a:N` と並ぶ。

その後ろに `[-vf]`（captions 有効時は `-filter:v:N`）→ `[-crf|-qp]` → `[-preset]` が続く。さらに `-force_key_frames` → `profile.extra_args` → `-var_stream_map` / `-master_pl_name` → `-f hls ...` と並ぶ。filter は `deinterlace` が true なら解除を先頭に置き、height があれば scale をその後ろに 1 個だけ連結する。音声 3 本の意味は [api/media.md](../api/media.md) §音声にある。

**`extra_args` の位置が 1 点だけ動いた。** VOD 側は以前 `-f`（コンテナ）の後ろだったが、今は前に移った。「ユーザーのオプションはコーデック/品質/スケール指定の後・アプリ所有の末尾の前」という規則を VOD と live で 1 つにするためである。`-f` は [cut.md](cut.md)「追加引数の許可リスト」に含まれないので、この移動でユーザーが相対順序に依存していた挙動が変わることはない。

