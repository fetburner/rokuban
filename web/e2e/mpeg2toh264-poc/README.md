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

## 既知の制限

測定の数字は、次の PoC 側の制限を含む。

- 変換はメインスレッドで同期に走る。ライブラリ同梱の player が使うワーカーのプールは使っていない
- シークは新しい Session の最初の GOP に着地し、目標より最大 1.2 秒手前になる。測定スクリプトはバッファ後に再シークする
- 第 2 音声を選んでも、出力が既定の音声と同じになった。原因は未特定
