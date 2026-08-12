> [frontend.md](../frontend.md) の一部。索引から辿る

# 予約の UI

## 予約はワンタップ + トーストから取消

確認ダイアログを挟まない。誤タップは 2 つの手段で緩和する。

- 予約ボタンは行右端の固定幅に置き、行本体（詳細展開）のタップ領域と分離する。
  最小 44px を確保する
- 実行直後にトーストで「予約しました [取消]」を出す。確認より速く、かつ取り返しがつく

**楽観的更新はしない。** SSE と REST 再取得で確定させる（レベルトリガー）。
操作中の行だけを pending にする（mutation の `isPending` を全行に渡すと、
1 件予約する間にリスト全行のボタンが無効化される）。

この「行右端の固定幅 + 44px」という配置文法は録画一覧の再生ボタンにも流用する
（[recordings.md](recordings.md) §録画のブラウザ再生「再生ボタンは行右端に独立させる」）。
ただし挙動までは揃えない --- 予約はワンタップで確定してよい安価な操作（DB 行を
作るだけ）だが、再生は帯域を伴う操作なので、ボタンの役目は「プレイヤーへの導線」
までに留め、実際のデータ転送は利用者のもう一段の操作に委ねる。

## 予約詳細は base / overrides を 1 画面に統一する

`base`（ruler が書く）と `overrides`（api が書く）は同形の jsonb なので、
ルール由来予約と手動予約を同じ画面で扱える。EPGStation は編集画面が分裂しているが、
その形を最初から避ける。

**機能しないコントロールは置かない。** `encodeProfiles` / `keepOriginal` は
ルール作成/編集と予約 overrides で編集できる。プロファイル一覧は
`GET /api/encode-profiles`（設定名だけ。機微情報なし）。
`keepOriginal: until_encoded` はプロファイル空だとクライアントでも止め、API も 400。
予約 overrides の PATCH は「既存 override + このパッチ + ルールの base」をマージした
実効値で判定するため、`keepOriginal` だけを送る・`encodeProfiles` だけを reset する
という 1 リクエストでは見えない組み合わせも実効値としては空プロファイルなら弾かれる。
priority など mirakc 差分が必要な項目は reconciler が差分を反映してから編集可能にする。

## 予約が録られない理由を出す

`skip` が立っている予約はバッジで示し、重複排除の判定根拠があれば
「重複（録画 #id, 類似度 0.83）」まで出す。このバッジ表示は機能の受け入れ条件
そのものとして定めた
（[recording.md](../recording.md) §3.1「なぜスキップされたかを説明可能にする」）。

`skip` は列ではなく `base` + `overrides` + `program_intents.action` からの導出値なので、
API が解決済みの boolean を返す（クライアントで jsonb を合成させない）。
`action = 'record'` が重複判定に勝ったケースでは根拠を残したまま `skip = false` になるため、
「重複と判定したが録る」と表示できる。

## 経緯と失敗事例

- ワンタップ + トーストの形は M1 で決めた。当時は全予約が manual（overrides のみ）
  で、確認ダイアログに出す内容が実質なかった
- 予約 overrides の PATCH を実効値で判定する仕様は issue #104
- priority などの編集解禁の前提（reconciler の差分反映）は
  [issue #19](https://github.com/fetburner/rokuban/issues/19)
- skip バッジと判定根拠の表示は M2-6 の受け入れ条件だった
