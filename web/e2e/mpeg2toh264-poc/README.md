# mpeg2toh264 PoC（#1674 の判定用）

原本の放送 TS を mpeg2toh264（WASM）と MSE で実 Chrome に再生させる使い捨てのページ。製品コードではない（不変条件 6 の例外として、判定のためだけに置く）。

**録画・切り出し・フレーム画像はコミットもアップロードもしない**（`.gitignore` で `*.ts` / `*.m2ts` / `*.png` / `out/` / `dist/` を除いている）。

## ビルド

ライブラリを判定に使った版で取得し、WASM と yadif を手元でビルドする。Playwright を使うので、先に web の依存を入れておく。

```bash
git clone https://github.com/otya128/mpeg2toh264 /path/to/mpeg2toh264
git -C /path/to/mpeg2toh264 checkout 7b008c6
rustup target add wasm32-unknown-unknown
cargo install wasm-bindgen-cli --version 0.2.126 --locked   # clone の Cargo.lock に合わせる
LIB=/path/to/mpeg2toh264 ./build-wasm.sh                      # dist/ に WASM と yadif を出す
```

## 使い方

再生と配信と測定は、それぞれ別のスクリプトで行う。どれも実 Chrome を 1 つずつ起動する。

- `driver.mjs`: 実 Chrome（headed、channel chrome）で再生し、状態と画面の取り込みを出す
- `serve.mjs`: Range 対応の静的サーバ。`--grow` は書き込み中のファイル（NFS の見え遅れ）を模す
- `measure-run.mjs`: CPU と、CDP の Media ドメインから取ったデコーダ名
- `measure-boundaries.mjs` と `measure-compare.mjs`: 章の境界へのシークと、ffmpeg の参照フレームとの輝度 MAE
- `measure-av-*.mjs`: 出力フラグメントの時刻と元の PTS の突き合わせ
- `control-decoder.mjs`: PoC を通さない 1080i / 1080p の対照
- `make-synthetic.sh`: 合成 TS（testsrc2 と sine）
- `measure-run.mjs --convert main|worker|pool [--pool N]`: 変換を走らせる場所。`main` はメインスレッドで `Session.push` を同期に呼ぶ。`worker` は `session-worker.js` に移す。`pool` はさらに `picture-worker.js` を N 個（既定 3）立て、`pushDeferred` / `complete` で絵を分ける（同梱の player と同じ分け方）
- `measure-run.mjs --gop idr|recovery-point`: Session の `openGopRecovery`。既定はライブラリの既定の `idr` で、周期のランダムアクセス点ごとに IDR とその複製の 2 サンプルを足す。Chrome はこの 2 枚を `droppedVideoFrames` に数える。`recovery-point` は絵を足さない。`measure-boundaries.mjs` と `measure-av-chrome.mjs` も `--gop` を受け、変換はワーカーで行う。`measure-av-node.mjs` は 6 番目の引数で受ける
- `measure-run.mjs --yadif-double`: yadif をフィールドごとに 1 枚出す（`doubleRate`）。集計の `yadif.late` は表示されなかったフィールドの絵の数
- 集計の `contentFrames.missing`: rVFC の `mediaTime` の飛びから数えた、画面に出なかった元のフレームの数。`presented` と `callbacks` が等しいときだけ有効
- `measure-run.mjs --window-at-seek`: シークの直後から測る（再開の立ち上がりを含める）
- `gpu-info.mjs` と `decoders.mjs`: `chrome://gpu` の要点と、各 run の最終的なデコーダ名
- `.github/workflows/poc-1674-windows.yml`: 合成素材だけで Windows の Chrome を測る。ただし GitHub-hosted の Windows runner には GPU が無い（`Microsoft Basic Render Driver`、`Video Decode: Software only`）。そのため HW デコードの可否はこれでは判定できない

## 既知の制限

測定の数字は、次の PoC 側の制限を含む。

- 変換の既定はメインスレッドで同期（`--convert main`）。ワーカーとプールは `--convert` で選ぶ。MSE と yadif は常にメインスレッドに置く
- 再生の開始とシーク後の再開は、2 秒先までバッファしてから行う（最初の断片で始めると、立ち上がりで 1〜2 フレームを失った）
- `make-synthetic.sh` の素材は、インターレースの符号化だが内容はプログレッシブ（`idet` で全フレーム progressive）。フィールドの動きを測るには、59.94p を `tinterlace` で TFF にした素材を使う。MPEG-2 のフィールドピクチャと RFF は ffmpeg で作れないので、実録画が要る
- シークは新しい Session の最初の GOP に着地し、目標より最大 1.2 秒手前になる。測定スクリプトはバッファ後に再シークする
- 第 2 音声を選んでも、出力が既定の音声と同じになった。原因は未特定
