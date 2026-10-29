> [live.md](../live.md) の実機確認について。索引から辿る

### 実機確認について

実機ブラウザでの再生は、利用環境で手順に沿って確かめる。

Playwright WebKit は診断用に hls.js 経路を確認する。iPhone Safari を含む iOS 実機での再生は未確認である。`Hls.isSupported()` が偽の環境ではプレイリスト URL を video 要素へ渡さず、HLS 非対応の案内を表示する。

実機のブラウザで再生できるかは、[runbook.md](../../runbook.md)（ライブ視聴の節）の確認手順で判定する。Playwright WebKit の結果を iPhone Safari の確認に置き換えない。

<details><summary>根拠</summary>

- ブラウザ診断の置き場: `web/e2e/`、`web/e2e/README.md`
- 経路判定と非対応案内: `Hls.isSupported()`、`components/live-player.tsx`

</details>
