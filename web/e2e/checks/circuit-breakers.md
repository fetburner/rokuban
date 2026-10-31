# サーキットブレーカー

発動バナーは置き場名と再開対象を明示する。

`circuit-breaker-store.mjs` は FS 置き場と S3 置き場の同名ブレーカーが両方発動した状態で、バナーに両方の置き場名が表示されることを Chromium で確認する。あわせて、S3 の行の再開が S3 置き場の URL だけに POST されることも確認する。バナーの表示と送信 URL を測るもので、サーバー側でどの行が消えるかは測らない。

API はブラウザ内でスタブする。サーバー側で、指定した置き場の行だけが消えることは Go の `internal/api` テストが確認する。この判定はビルド済み画面の表示・ダイアログ操作・送信 URL を見る。

```sh
cd web && pnpm build
pnpm preview --host 127.0.0.1 --port 4173 --strictPort
```

別のターミナルから実行する。

```sh
cd web && E2E_URL=http://127.0.0.1:4173 pnpm e2e:circuit-breaker-store
```
