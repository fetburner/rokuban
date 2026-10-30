> [live.md](../live.md) の実機確認について。索引から辿る

### 実機確認について

Safari / iPhone は確認対象にしない。動く根拠は、Chrome と同じ hls.js 経路を通ることである。Playwright WebKit は hls.js 経路の診断に使う。

`Hls.isSupported()` が偽の環境ではプレイリスト URL を video 要素へ渡さず、HLS 非対応の案内を表示する。

hls.js 経路の確認手順は、[runbook/live.md](../../runbook/live.md) §「再生の入口と経路の判定（⓪〜⑥）」にある。

<details><summary>根拠</summary>

- ブラウザ診断の置き場: `web/e2e/`、`web/e2e/README.md`
- 経路判定と非対応案内: `Hls.isSupported()`、`components/live-player.tsx`

</details>
