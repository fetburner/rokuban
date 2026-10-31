# サーキットブレーカー

発動バナーは置き場名と再開対象を明示し、確認操作はその置き場の行だけに届く。

`circuit-breaker-store.mjs` は S3 置き場のブレーカーが発動した状態で、バナーに置き場名が表示され、確認後の再開がその置き場の URL を使うことを Chromium で確認する。

API はブラウザ内でスタブする。実 API の保存と再開先は Go の `internal/api` テストが確認し、この判定はビルド済み画面の表示・ダイアログ操作・送信 URL を見る。

```sh
cd web && pnpm build
pnpm preview --host 127.0.0.1 --port 4173 --strictPort
```

別のターミナルから実行する。

```sh
cd web && E2E_URL=http://127.0.0.1:4173 pnpm e2e:circuit-breaker-store
```
