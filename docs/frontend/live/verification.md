> [live.md](../live.md) の実機確認について。索引から辿る

### 実機確認について

**実機確認の手段・判定項目・回帰の記録は [runbook.md](../../runbook.md)（ライブ視聴の節）と `web/e2e/`（`web/e2e/README.md`）を見る。** ここには設計に跳ね返る 1 点だけ残す。

- **iOS 実機（iPhone Safari）は誰も確認していない**。判定は macOS の 4 エンジンで実測して固定したが、iOS の `canPlayType('video/mp2t')` が macOS の WebKit と同じ `'maybe'` を返す保証は無い。違っていた場合、iOS は hls.js 経路へ落ちる（iOS 17.1 以降は ManagedMediaSource で再生できる。それ未満は[playback.md](playback.md) の 3 段目の最後の砦で `<video>` に直接渡る）。この判定が正しいかは [runbook.md](../../runbook.md)（ライブ視聴の節）の実機確認でしか確定しない
