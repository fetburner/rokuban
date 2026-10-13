> [media.md](../media.md) のライブ HLS。索引から辿る

### ライブ視聴の HLS --- 既定はアプリ配信

`playlist.m3u8` は常に master playlist で、音声 rendition 3 本（下記 §音声）を含む。`live.captions: false`（既定）では `?profile=` のプロファイルだけの master を返す。`true` のときは全プロファイルを 1 つの master にまとめ、ffmpeg の `libaribcaption` で変換した WebVTT 字幕 rendition も含む。variant / 字幕 playlist、 `.ts` / `.vtt` セグメントは同じサービス URL の下で配信する。hls.js は字幕 rendition を字幕トグルとして表示する。VOD とライブのどちらも TS/PES を Rokuban が読むことはない。

ライブセッションはインメモリの使い捨て状態（全体アーキテクチャの crash-only 例外）で、「クライアントがいなくなったら ffmpeg を止める」idle GC が要る。セグメント要求がアプリを通れば last-access の更新がタダで手に入るが、nginx が scratch から直接配るとアプリはクライアントの生存を見失う。`auth_request` やログ監視で回収はできるが、セグメントは数 MB で転送負荷が軽く、複雑さに見合わない。**既定は streamer ロールのアプリ配信のまま**とする。遅延を目標にする構成では、パッケージングと生存判定をまとめて外の packager に移す（下記「パッケージャは MediaMTX の LL-HLS を選べる形にする」）。

**`live.enabled: false` ならこれらのルートは登録されず、404（JSON）になる**。SPA フォールバックには落とさない（[rest.md](../rest.md)「機能の有効/無効は能力 API で観測する」）。落とすと「無い」が HTML の 200 になり、probe するクライアントが成功と誤認する。導線そのものを出さない判断は `GET /api/capabilities` 側。

#### 遅延の目標は置かない --- `segment_seconds` を短くしても縮まない

**アプリ配信の経路では、ライブの glass-to-glass 遅延を目標値にしない。** 実クラスタ・実チューナー・実ブラウザで 2 秒と 1 秒のセグメント長を比べても、遅延の改善は 6.2 秒 → 5.0 秒どまりである。4 秒には届かない。代わりに 30 分あたりの stall は 3 回 → 454 回、dropped frames は 0.006% → 2.1% になった。

**セグメントの公開間隔そのものが揺らぐので、`segment_seconds` は遅延の目標値にならない。** 実測では p95 が 2.3 秒 → 3.6 秒、最大が 4.7 秒 → 10.2 秒へ広がった。プレイヤーはライブ端より数本後ろから開始するので、公開の揺らぎはそのまま再生の飢えになる。**短くするほど遅延が縮むという関係は、ここには無い。**

1 秒級を狙うなら、セグメントをさらに短くするのではなくパッケージャを替える（下記「パッケージャは MediaMTX の LL-HLS を選べる形にする」）。また `live.profiles[].segment_seconds` はライブと追っかけ再生で共有しているので、ライブだけ短くするには値の分離も要る。

**この値はセグメントの公開端とプレイヤーの latency からの導出値である。** 放送時刻を映像に焼き込んだ比較ではない（ffmpeg の再 mux で TDT/TOT が落ちるため）。したがって「放送から 6.2 秒」と断言はしない。測定の条件と経路ごとの内訳は [frontend/live/metrics.md](../../frontend/live/metrics.md) §「遅延・バッファの計器」に置く。

#### パッケージャは MediaMTX の LL-HLS を選べる形にする

**遅延を目標にする構成では、パッケージングと生存判定をまとめて MediaMTX に移す。** アプリ配信の根拠は、セグメント要求がアプリを通れば生存判定がタダで手に入ることだった（上記）。この形では生存判定も MediaMTX の HLS セッションの inactive 判定に移るので、その根拠とは衝突しない。代わりに離脱ヒントが効かなくなる（下記）。

実測（同一局・720p・視聴者 1・30 分・実 Chrome + hls.js）では、アプリ配信（`segment_seconds: 2`）の `hls.latency` 中央値は 5.68 秒だった（上記の 6.2 秒とは別の測定）。MediaMTX v1.21.1 の LL-HLS（part 200ms / segment 1s）は 0.92 秒（p95 2.15 秒）である。セグメント長を詰めても 1 秒台には入らない（上記）ので、パッケージャを替える以外に手段が無い。試作は `-tune zerolatency` を含むが、その寄与は高々 0.1〜0.3 秒で、差の大半は part による部分公開に由来する。

**代わりに stall と dropped frames が増える。** 同じ 30 分で stall は 1 回 → 15 回、dropped frames は 0 → 0.14% だった。試作側は WAN 越しの port-forward・ソフトウェアエンコード・part 単位の取得（24 req/s）という条件の差を含む。差のどこまでが MediaMTX に由来するかは未検証である。

- **必須にはしない。** MediaMTX の設定テンプレートに mirakc の知識（合成 service id・URL 組み立て）は置かない。`runOnDemand` が rokuban のコマンドを起動する形にすれば、`live.enabled` と同じ位置づけ（無くても現行のアプリ配信で動く）にできる
- **idle GC は packager 側に移り、下限は約 30 秒になる。** 最後の要求から publisher が止まるまで、既定では約 60 秒かかる。`hlsMuxerCloseAfter` と `runOnDemandCloseAfter` を 1 秒に詰めても約 32 秒である（HLS セッションの inactive 判定が支配的）。現行の `idle_timeout`（既定 30 秒）と同等である
- **未解決: 離脱ヒントの猶予（`3 × segment_seconds + 2s` = 8 秒）に相当するものが MediaMTX に無い。** ヒントを受けた rokuban が publisher を止める経路を作れるかは未検証である。作れなければ、チューナーが 1 本のサイトでは切り替えのたびの解放が 8 秒から約 30 秒へ伸びる
- **`/live/segments/` の実パスは契約ではない。** hls.js も VLC もプレイリスト URL からの相対解決しかしないので、外から見える契約はプレイリスト URL だけである。現行の `segments/<name>` は ffmpeg の `-hls_base_url` が書く値で、クライアントは組み立てない。MediaMTX の LL-HLS も `init.mp4` / `*_partN.mp4` / `*_segN.mp4` をプレイリストと同じディレクトリの相対 URI で書く
- **前段はプレイリストだけでなく全要求の接頭辞を写す。** 相対 URI は、クライアントが要求した `.../live/` の下で解決される。そのため前段は全要求の接頭辞 `/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/` を、MediaMTX のパス `live/{site}/{networkId}/{serviceId}/` へ書き換える。加えて `playlist.m3u8` を `index.m3u8` へ写す。試作では計器自身が立てた同一オリジンのリバースプロキシがこの写しを担い、実 Chrome + hls.js が 30 分再生できた
- **プロファイルはパスを分ける。** 1 パスに映像 2 本を publish しても、MediaMTX は 2 本目を `skipping track 3 (H264)` として捨て、ABR の master を書かない（実測）。 `?profile=` の選択を保つには、プロファイルごとのパスと、それを束ねる master の書き手が要る
- **未解決: 音声レンディションと字幕。** 試作では音声 2 本が rendition として master に載った。上記の契約（標準 / 主 / 副の 3 本、`captions: true` の WebVTT 字幕）を MediaMTX の経路でどう満たすかは未測定である
- **未解決: MediaMTX の LL-HLS はセッション ID を URI のクエリに載せる**（`?session=<uuid>`）。master が書く variant の URI（`video1_stream.m3u8?session=…`）にも載る。つまり hls.js が取り直し続ける variant の URL が session を握る。MediaMTX の再起動・ハッシュの担当移動・idle GC の後に、未知の session を持つ variant 要求へ MediaMTX が何を返すかは未検証である。404 なら下記「資源同定」が塞いでいる「セッション ID を握ったクライアントが 404 で詰む」経路そのものになり、この形は採れない

#### 資源同定: セッション ID を持たない

プレイリストとセグメントの URL は **`/api/sites/{site}/networks/{networkId}/services/{serviceId}/live...`** の形にする。そして**セッション ID を URL にもクッキーにも置かない**。ライブセッションはサービスに対して 1 つで、同じサービスを見ている視聴者はそれを共有する。

- **チューナーが共有される。**別の部屋で同じチャンネルを見ても ffmpeg 1 本・チューナー 1 本で済む。チューナーは録画と取り合う唯一の共有資源なので、これが一番効く
- **スケールアウトの鍵が既に資源同定の中にある**。`(site, networkId, serviceId)` は前段の consistent hash の鍵にそのまま使える。そのため streamer のレプリカを増やしても URL・クライアント・API は変わらない（[operations.md](../../operations.md) §5「streamer のスケール」。URL を固定深さにする制約もそこに書いてある）
- **セッションが消えても URL が死なない。**Pod 死・ハッシュの担当移動・idle GC の後でも、同じ URL への再要求が新しいセッションを起こす。「セッション ID を握ったクライアントが 404 で詰む」経路が存在しない。**セッションを起こすのは master だけではない。** hls.js が取り直し続けるのは variant playlist の方なので、variant の要求もセッションを作り直す。master だけにすると、セッションが消えた後の variant 要求が 404 になり、hls.js は 4xx を再試行せずに止まる

セッション ID を持つ設計（`POST` でセッションを作って ID 付きの URL を配る）は、導出物の identity を宛先にしてしまう。ライブセッションは使い捨てなので、宛先は「このサービスが見たい」という要求で名指しする。

**パスの id 空間は一覧 API に揃える。** `{networkId}` / `{serviceId}` は `GET /api/sites/{site}/services` と同じ SI の値である。mirakc が要求する Mirakurun 合成 service id（`networkId * 100_000 + serviceId`）は streamer 側で変換する。

- **同じ URL 階層に 2 つの id 空間を同居させない。**`services/{serviceId}` が一覧では SI の値、ライブでは合成 id を指す状態は、将来 `GET /api/sites/{site}/.../services/{serviceId}` を足したときにどちらの空間か決められなくする。API の資源同定は差し替えコストが最も高い先払いなので、読者が 1 つ（同梱 SPA）しかいない今のうちに払う
- **mirakc の id 規則を Rokuban の一番外側に置かない。**合成 id は mirakc / Mirakurun の内部規則であり、変換を web に置くと同じ規則の実装が Go と TypeScript に二重化する。URL は永続テーブルより差し替えが高いので、mirakc 固有の概念を置く場所として最悪である。
- **`serviceId` 単独では鍵にならない。**SI の service_id は network をまたぐと一意でない（Mirakurun が合成 id を発明した理由そのもの）。宛先を SI の値で名指しするなら `networkId` も URL に要る --- 放送イベントを `(site, network_id, service_id, event_id)` で引く場合と同じ形

帰結として **idle GC の粒度もサービス単位**になる（そのサービスへのセグメント要求が一定時間来なければ ffmpeg を止める）。「クライアント 1 人ごとの生存」は追わない。

<details><summary>根拠</summary>

- セッション URL に導出 ID を置かない: 不変条件 9
- 要求を宛先にする設計: 不変条件 5
- 資源同定を先に決める: 不変条件 11
- Mirakurun の合成 service id と実装箇所: `internal/programid.ServiceID`
- mirakc 固有概念を外側の設計に持ち込まない: 不変条件 7
- 放送イベントは site / network / service を含む id で引く: 不変条件 9、`(site, network_id, service_id, event_id)`

</details>

#### 離脱は「ヒント」であって停止命令ではない

チャンネルを切り替えた視聴者のセッションが `live.idle_timeout` のあいだ残ると、**1 人の視聴者が 2 本のチューナーを掴む**。チューナーが 2 本しか無い環境では切り替え 1 回で録画が開始できなくなりうる（mirakc の優先度では録画が勝つので、視聴者からは「切り替えたら見られなくなった」と見える）。この窓を縮めるために離脱の受け口を置くが、**「セッションを閉じる API」にはできない**。

```
POST /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave
  → 204 No Content（常に）
```

- **止めるのではなく idle 期限を「いま + 短い猶予」に詰める。** ライブセッションはサービス単位で共有される（上記）ので、「離れた側の要求で止める」形にすると**別の部屋で同じチャンネルを見ている視聴者の再生を一方的に切れてしまう**。他に視聴者がいれば、その人の次のセグメント / プレイリスト要求が last-access を更新して期限が元に戻る --- **ヒントは収束を速めるだけで、「誰かが見ているか」という真実は既存の観測（セグメント要求）が持つ**。レベルトリガーと同じ形であり、クライアントの identity も参照カウントも要らない
- **宛先は `(site, networkId, serviceId)`。** セッション ID は導入しない（この節の決定）。固定深さも保つので前段の consistent hash 鍵の取り出しはそのまま効く
- **セッションを作らない。** 該当セッションが無ければ何もせず 204（存在を漏らさず、`sendBeacon` に再送の材料も与えない）。**POST なのは `navigator.sendBeacon` が POST しか出せないから** --- モバイル Safari では `unload` が発火しないので、ページ離脱時に届く送信手段はこれしかない
- **猶予は独立した設定キーにせず、最長の `live.profiles[].segment_seconds` から導出する**。値は `3 × 最長の segment_seconds + 2s`（既定 8 秒）で、`idle_timeout` ではクリップしない。
- **猶予が `idle_timeout` 以上になる構成ではヒントを no-op にする。** たとえば `segment_seconds: 6` / `idle_timeout: 2s` をそのままクリップすると、猶予がセグメント長を下回る。
- **猶予は生きている視聴者の次の要求までの間隔を上回る。** この間隔を決めるのはセグメント長であり、独立した短い猶予を許すと leave が別の視聴者の再生を切る道具になる。
- **期限は縮む方向にしか動かさない。** last-access の巻き戻しだけを許し、ヒントでセッションを延命させない。猶予が `live.idle_timeout` 以上なら詰め先が現行期限より後ろになるため、ヒントは何も起こさない。
- **待っている客も客。** セッションの起動待ち（mirakc 接続 + ffmpeg 起動 + playlist の 1 本目が出るまで。最大 15 秒）は、誰も要求を出さない無音区間に見える。**そこにはそのセッションを待っている視聴者がいる**。ここで last-access が止まったままだと、この区間に届いたヒント 1 発で起動待ちの視聴者ごとセッションが回収される（実測: 504）。待っている側がポーリングのたびに last-access を更新することで、無音区間そのものを無くす。猶予を「起動待ちより長く」する形では、ヒントの効きがその分鈍る（この経路は猶予を 8 秒に詰められるようにしたことで生まれた）
- idle GC ループの刻みも猶予の半分にする（`idle_timeout / 2` のままだと、期限を詰めても回収が次のパスまで来ずヒントが刻みに飲まれる）

ヒントが届かなくても壊れない（従来どおり `live.idle_timeout` で回収される）し、余計に届いても壊れない（自分の次の要求が期限を戻す）。

**プロファイルはクエリ（`?profile=`）で受け、ハッシュ鍵には入れない。**鍵に入れると同じサービスを別プロファイルで見たときに 2 つの Pod に割れ、チューナーを 2 本掴む。1 つの Pod の中で 1 チューナーから複数プロファイルを出す。

<details><summary>根拠</summary>

- 猶予の導出式: `leaveGrace`
- 起動待ちの上限: `playlistStartupTimeout`
- イベントはヒント、真実は既存観測: 不変条件 5

</details>

#### 一覧の契約（`GET /api/live-profiles`）

画質セレクタが出すのは `config.live.profiles` の名前である。その公開面は **`GET /api/live-profiles`** に置く（`LiveProfileSummary { name, height? }`）。

- **`GET /api/capabilities` の `live` を object にしない。** あちらの規律は「返すのは真偽値だけで、config のキー名・値は載せない」（`ListEncodeProfiles` / `ListSites` と同じ）であり、一覧は「何が選べるか」、`live` は「導線を出してよいか」という別の問いである。畳むと `lib/capabilities.ts` の 4 値判定（`pending` / `unknown` / `enabled` / `disabled`）の派生が濁る
- **載せるのは `name` と表示用の `height` だけ。** `video_codec` / `crf` / `qp` / `preset` / `extra_args` / ffmpeg パスは出さない --- 出すとフロントがそれを再現する形に育つ（`EncodeProfileSummary` と同じ規律）。`height` は実際の出力高で、0 または省略は「スケールしない」
- **順序が既定の根拠である。** 先頭が `?profile=` を省略したときの既定（`LiveConfig.profile` と同じ決め方）で、フロントは並びを変えない
- **`live.enabled` は見ない。** 返るのは `config.live.profiles` の写しで、**無効なデプロイでも profiles が書かれていれば返る**（`config.compose.yml` は `enabled: false` と `profiles` を並べて出荷している）。有効かどうかは `GET /api/capabilities` の `live` の問いである --- ここで無効を空配列に潰すと、同じ config の状態を 2 箇所で判定することになる。未定義なら空配列（`null` ではない）
- **未知の名前は 400。** `?profile=` が空なら既定（先頭）に落ちるが、一覧に無い名前は `unknown live profile` を返す（セッションを起こす前に拒否する）。フロントは一覧に照らして先に落とすので、この 400 は直リンク・手書き URL の受け皿になる

**ロール分割で api と streamer に別の config を配る構成では、この一覧と実際に配られるプロファイルがずれる。** ずれを検出する手段は無い。一覧は api が読む config から作り、実際に配るのは streamer である。したがって**「一覧にあるから見られる」とは言えない**（下界主義。[data.md](../../data.md) §6.5 と同じ規律）。

#### 実装（`internal/streamer`）

```
GET  /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/playlist.m3u8[?profile=<name>]
       → application/vnd.apple.mpegurl
GET  /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/segments/{name}
       → video/mp2t
POST /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave
       → 204（離脱のヒント。上記「離脱は『ヒント』であって停止命令ではない」）
```

master から参照される variant playlist（映像・音声）と字幕 playlist は `.../live/{name}.m3u8` で配信する。受け付けるのは ffmpeg が書く variant の名前だけで（master の名前や未知のプロファイルは 404）、セッションが無ければ作る（上記「セッションが消えても URL が死なない」）。`.vtt` は字幕有効時だけ受け付ける。

- **DB を引かない。** パスの `(networkId, serviceId)` から mirakc の `GET /api/services/{id}/stream?decode=1` の `{id}` を合成する。ライブセッションはインメモリの使い捨てで、認可はリバースプロキシに委譲し、同時上限もプロセスローカルなので DB を引く理由が無い。
- **id セグメントは 16 bit 符号なし整数の十進正準形だけを受け付ける。** SI の network_id / service_id の幅に合わせ、先頭ゼロも拒否する。読めなければ 400 にし、mirakc には触れない。
- mirakc へ渡すのは URL 文字列でなく合成した整数である。そのため `%2F` や `%3F` を含む要求が mirakc の別 endpoint に化ける経路は無い。
- **URL の正準形は前段の hash 鍵に必要である。** streamer 内では `1024` と `01024` が同じ session key になる。[operations.md](../../operations.md) §5 の `map $uri $live_key` は生 URL を使う。別名を許すと同じチャンネルが 2 Pod に分かれ、チューナーを 2 本使う。「同じチャンネルの視聴者は同じ Pod」という前提を守るため、URL の正準性を保つ。
- **不明な id を mirakc がどう扱うかは未測定であり、その挙動には依存しない。** 上流が要求を拒否した場合は他の起動失敗と同じ 503 にする。
- **トランスコードは必須。**ISDB-T 地上波の映像は MPEG-2 で、ブラウザの HLS 経路（hls.js/MSE）は事実上再生できない。mirakc フィルタ + `-c copy` では受信端末を満たせないため、ffmpeg で H.264/AAC に変換する（`live.profiles`、[configuration.md](../../configuration.md) 参照）
- **`profile` クエリが空なら `live.profiles` の先頭を既定として使う。**セグメント URL 自体は `?profile=` を持たない --- ffmpeg が書き出すファイル名にプロファイル名を接頭辞として焼くため、プレイリストが指す相対パスだけで一意に解決できる
- **1 サービス = 1 ffmpeg プロセス = mirakc の 1 チューナー。**設定済みの全プロファイルを 1 回の ffmpeg 起動で同時に出す（見られていないプロファイルの CPU も使うトレードオフはあるが、プロファイルを跨いだ ffmpeg の使い分けを実装しない分シンプルになる）
- **チューナー調停は mirakc のリクエスト優先度に一元化する。** ライブ GET は `live.tuner_priority`（既定 1）を `X-Mirakurun-Priority` に載せる。既定の ruler schedule priority（10）より低いため、枯渇時は録画が優先される（[recording.md](../../recording.md) §2「チューナー調停」）。
- 予約表を見てライブ要求を拒否しない。streamer を予約エンジンに依存させず、mirakc 固有の優先度を永続テーブルに持ち込まない。
- **Rokuban は `live.tuner_priority < rules.priority` を強制しない。** 前者は config、後者は運用中に DB で変える値である。両者を跨いで検証する権威は無い（[configuration.md](../../configuration.md) §config と DB の境界）。ルールの priority を 10 未満に下げれば、既定値のままではライブが録画に勝つ。運用者が両方の値を選ぶ。
- **同時セッション上限（`live.max_sessions`）はプロセスローカル。**超えた要求・mirakc 側のチューナー枯渇はいずれも既存セッションを壊さずに 503 を返す（エラーの本文はプレーンテキスト。OpenAPI 対象外のため生成クライアントの契約は無い）
- **idle GC はサービス単位。**セグメント要求（プレイリスト取得も含む）ごとに last-access を更新し、`live.idle_timeout` の間要求が来なければ ffmpeg と mirakc への接続を止める。クライアント 1 人ごとの生存は追わない。**離脱ヒント（`POST .../live/leave`）はこの last-access を巻き戻すだけ**で、止めるのはあくまで idle GC である（上記「離脱は『ヒント』であって停止命令ではない」）
- **新しいセッションの起動が mirakc に拒否されたとき、またはプロセス内上限に達したときだけ、最古の idle セッションを退避して 1 回だけ再試行する**。最長の `segment_seconds` の 2 倍より長く要求が来ていないセッションを候補にする。該当するものが無ければ従来どおり 503 を返す。候補の選択に mirakc の現在状態は使わない。状態を取得しても読み取りと再試行の間に古くなるためで、自分側の last-access と起動失敗だけで判断しても外れた場合の結果は従来の 503 と同じになる。退避したセッションの `stop()` が完了しても mirakc の tuner 解放は非同期なので、再試行の前に実測値（2.35〜4.18 秒）に余裕を持たせた 5 秒の解放待ちを 1 回だけ入れる
- **退避は上流拒否の理由を区別しない。** mirakc はチューナー枯渇も存在しない service への要求も同じ 404/503 で返すため、streamer 側では区別できない。結果として、存在しないチャンネルへの要求でも idle セッションの退避が走る（近似分析が受け入れている性質）
- **セグメントは `live.segment_dir` の tmpfs に書き、録画バッファとは別に置く**（[operations.md](../../operations.md) §5「ライブのセグメントを録画バッファと同じディスクに置かない」）。

| 状態 | 後始末 |
|---|---|
| `--all` / `--roles streamer` の SIGTERM | idle GC と同じ経路で全セッションを止め、ディレクトリを削除する |
| SIGKILL などのクラッシュ | tmpfs はノード再起動まで残る。k8s の `emptyDir: {medium: Memory}` もコンテナ / Pod の再起動をまたいで残る |
| 次回起動 | HTTP listener の前に `live.segment_dir` の中身を掃除する。directory 自体は削除しない |

`emptyDir` を `segment_dir` に直接 mount した構成では、Linux が mount point への rmdir を EBUSY で拒む（[configuration.md](../../configuration.md) §live）。
- **ffmpeg の LookPath 検査は `live.enabled: true` のときだけ行う。**公式イメージ（ffmpeg 無し）で streamer ロールを起動する構成（録画配信 / サムネイルのみ）を壊さない

<details><summary>根拠</summary>

- mirakc の service id 合成: `internal/programid.ServiceID`
- id の正準形: `strconv.ParseUint(s, 10, 16)`
- `%2F` / `%3F` / 非数値 / 空 / 符号付き / 16 bit 超 / 桁あふれ / 全角数字 / 先頭ゼロは 400。偽 mirakc は要求を 1 件も受け取らない: `TestLiveStreamer_RejectsHostileIDSegments`
- mirakc の Request-URI は `/api/services/3192053248/stream?decode=1` ちょうどになる。固定するテスト: `TestLiveStreamer_MirakcPathIsComposedFromPathSegments`
- 上流拒否時の 503: `TestLiveStreamer_UpstreamRejectionBecomes503`
- 起動時の tmpfs 内容削除: `NewLive`

</details>

#### 音声（二重音声の主 / 副）

**音声はプロファイルごとに 3 本の代替音声 rendition（標準 / 主 / 副）で出し、選ぶのはプレイヤーである。** サーバーは選択を知らず、セッションも作り直さない。選択は視聴者ごとの状態で、共有セッションの寿命に載せると作り直しのたびに黙って既定へ戻る（idle GC の後に既定の要求が作り直す等）。

- **音声 ES が 1 本なら、主 / 副は出力側の `pan` で作る。** 二重音声の既定デコード（`-dual_mono_mode` 無し）は L = 主 / R = 副のステレオである。片側を両耳へ写す（`pan=stereo|c0=c0|c1=c0` / `pan=stereo|c0=c1|c1=c1`）。 `-dual_mono_mode` はデコーダ側で 1 回の起動に 1 つしか選べないため使わない。実測（ffmpeg 9.0.2）: SCE 2 つを持つモノラル AAC では、この `pan` と `-dual_mono_mode main|sub` の出力がバイト一致した
- **音声 ES が複数なら、2 本目を副に使う。** セッション開始時の ffprobe が数を調べる。標準 / 主は `0:a:0`、副は `0:a:1` を map する。3 本目以降は選ばない。ffprobe が失敗した場合は 1 ES として扱い、従来の pan に戻す
- **標準はフィルタ無しで `DEFAULT=YES`。** 今までと同じエンコードなので、音声を選ばない視聴者の音は変わらない
- **記述子を読んで一 ES 内の二重音声かを分類しない。** 音声 ES が 1 本の番組では主 / 副に L / R の pan を使うので、通常のステレオで選ぶと片側だけになる。実放送の [二] は、2026-10-06 の地上波 NHK の録画でステレオ ES が 2 本と測定した。`program.audios` は先頭が `isMain=true` / `jpn`、2 本目が `isMain=false` / `etc`。先頭 ES の L−R RMS は −81.1 dB、2 本の L RMS 差は −24.4 dB だった。単一 ES の実放送 dual mono は未観測
- **並び順が UI との契約である。** グループ内の 0 = 標準 / 1 = 主 / 2 = 副で選ぶ。master の `NAME` は ffmpeg が `audio_<n>` で固定し、n はプロファイル数でずれる
- **ライブの playlist には `EXT-X-PROGRAM-DATE-TIME` を付ける。** 無いと hls.js は、ライブの窓（`playlist_size` × `segment_seconds`）より後で前に聴いた音声へ戻ったときに止まる。
- **追っかけの EVENT playlist には PDT を付けない。** segment を消さず窓もスライドしないため、PDT が無くても各トラックを 15 秒聴いて戻れる。hls.js が位置を揃える仕組みは未測定である。
- **captions 無効時はプロファイル別の出力のまま、各出力が自分の master を持つ。** 1 つの master にまとめると `hls_time` が 1 つになり、プロファイルごとの `segment_seconds` が書けなくなる（captions 有効時はそのため同一値を要求している）
- **追っかけ再生は画質を `?profile=` で選ぶ。** セレクタは `/recordings/$id?liveProfile=<name>#chase` に置き、一覧 API の名前を streamer へ渡す。画質切替は `(recordingID, offset)` のセッションを共有し、別 playlist を取得するだけである。再生位置は profile によらず原本時間軸で保存する。
- **追っかけ再生は音声 rendition も視聴者ごとに選べる。** 追っかけも profile 別 master から標準 / 主 / 副の 3 rendition を選ぶ。`/recordings/$id?audio=main|sub#chase` に選択を保持し、HLS URL と共有セッションは変えず rendition だけを切り替える。EVENT は過去の segment を保持するため、同じ session 内の seek と offset 再開も続けられる。
- **ライブの `extra_args` / `input_extra_args` では `-an` `-vn` `-sn` `-map` を拒否する。** ストリームの並びは `-var_stream_map` が持つ。並びを変えると ffmpeg が起動時に落ちる
- 未検証: 音声 ES 数が変わる番組境界を跨いだライブセッションの追従。ffmpeg の map は起動時に固定される。実 Safari・iOS の切替も未検証（WebKit では取得する rendition が替わることまで確認）

<details><summary>根拠</summary>

- PDT によるライブ音声の切替・復帰（PDT を外すと落ちる。変異で確認済み）: `web/e2e/live-audio.mjs`
- 追っかけ音声の切替、同一セッション内 seek、offset 再開: `web/e2e/chase-audio.mjs`

</details>
