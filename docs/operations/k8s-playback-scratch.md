# 録画再生の scratch

ライブ・追っかけ・原本 VOD の HLS セグメントは、録画バッファと archive から分けた scratch に一時保管する。この節は [k8s 運用](k8s.md) の streamer 配置と容量予算を補う。

## 原本 VOD

原本だけの完了録画は、原本 TS を読み取り専用で扱い、変換した HLS だけを scratch に書く。

原本だけの完了録画も `live.segment_dir` を使う。原本 MPEG-2 TS は site streamer が media PVC から read-only で開く。変換済み HLS は scratch に書き、録画バッファや archive PVC は使わない。site streamer の media mount は read-only のままでよい。録画パスのロックは取らず、開いてから DB で再確認するので、media root にロック用ディレクトリを作らない（根拠は [api.md](../api.md) 「録画原本のブラウザ再生」）。

原本 VOD は録画全体の EVENT playlist と全 segment を idle GC まで保持する。tmpfs の容量不足は原本 VOD の HLS 生成失敗として観測し、原本や録画バッファへは書き込まない。字幕 rendition を有効にする構成では WebVTT の scratch も加算する。

## 追っかけ再生

録画中の追っかけ再生は、同じ streamer 内でセッションを共有し、録画開始から EOF までの変換結果を保持する。

録画中の追っかけ再生も同じ `live.segment_dir` の scratch を使う。

- URL は固定深さで、開始位置を指定する場合は `/offset/{offset}` を含む。
- site を前段のルーティングキーにして、録画を担当する site の streamer へ送る。標準の Ingress では各 site overlay が `/api/sites/<site>/recordings` をその Service へ追加する。
- 録画 id は DB 内の mirakc record id を隠す公開キーで、同じ streamer 内のセッションを共有する。同じ録画の異なる開始位置は別セッションになる。
- hash key は `(site, recordings.id)` とし、同じ録画の全オフセットを同じ Pod に固定する。
- ライブ・追っかけ・原本 VOD は同じ process-local な `live.max_sessions` を共有する。メトリクスは `rokuban_live_active_sessions{kind="live"}` / `{kind="chase"}` / `{kind="original_vod"}` に分かれるが、scratch 容量は合算する。

追っかけは録画中の先頭から EOF までの EVENT playlist を `delete_segments` 無しで保持し、ffmpeg 終了後も idle GC まで全セグメントを残す。録画状態が終了へ変わった後も、既存のセッションはこの保持ファイルを配信する。tmpfs の容量不足は録画バッファへ移さず、追っかけセッションの HLS 生成失敗として観測する。recording.basedir または archive PVC と同じ mount / volume に置かない。

<details><summary>根拠</summary>

- 原本 VOD は録画パスのロックを取らない: `rel_path` / `.rokuban-locks`

</details>
