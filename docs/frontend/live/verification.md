> [live.md](../live.md) の実機確認について。索引から辿る

### 実機確認について

実機確認の手段・判定項目・回帰の記録は、[runbook.md](../../runbook.md)（ライブ視聴の節）とブラウザ e2e の README にある。ここに残すのは設計に跳ね返る 1 点だけで、iOS 実機（iPhone Safari）は誰も確認していない。

判定は macOS の 4 エンジンで実測して固定した。しかし iOS の判定結果が、macOS の WebKit と同じ「maybe」になる保証は無い。違っていた場合、iOS は hls.js 経路へ落ちる。iOS 17.1 以降は ManagedMediaSource で再生できる。それ未満は、[playback.md](playback.md) の 3 段目の最後の手段で、動画要素に直接渡る。

この判定が正しいかは、[runbook.md](../../runbook.md)（ライブ視聴の節）の実機確認でしか確定しない。

<details><summary>根拠</summary>

- 手段と記録の置き場: `web/e2e/`、`web/e2e/README.md`
- iOS で保証が無い戻り値: `canPlayType('video/mp2t')`、`'maybe'`
- 最後の手段で直接渡す先: `<video>`

</details>
