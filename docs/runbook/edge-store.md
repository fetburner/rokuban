> [runbook.md](../runbook.md) の一部。索引から辿る。

# エッジに置き場を持つ構成の導入

エッジの録画機に FS 置き場を持たせ、中央の Rokuban が転送を引き受ける構成の導入手順である。規則がエッジを置き場にしている間と、転送が終わるまでは、エッジの置き場が原本の唯一のコピーである。

以下の例では、エッジの site と置き場を `takamatsu`、中央の置き場を `tokyo` と呼ぶ。

1. config（ConfigMap は 1 つ）で両方の FS 置き場を定義する。エッジの置き場には `internal_url` を付ける。原本の規則は、エッジの site の新しい原本をエッジに置き、古くなった原本を中央へ送る。派生物は中央に置く。転送の帯域と転送待ちの上限は、転送元のエッジの置き場に付ける。

   ```yaml
   storage:
     stores:
       tokyo: { kind: fs, root: /mnt/tokyo }
       takamatsu: { kind: fs, root: /mnt/takamatsu, internal_url: "http://rokuban-recording-hls-takamatsu:40773" }
     originals:
       - match: { site: takamatsu, older_than: 30d }
         store: tokyo
       - match: { site: takamatsu }
         store: takamatsu
       - store: tokyo
     derived: tokyo
     transfer:
       bandwidth: { takamatsu: 50Mbps }
       backlog_limit: { takamatsu: 20 }
   ```

2. `server.allowed_hosts` に、`internal_url` のホスト（この例では `rokuban-recording-hls-takamatsu`）を加える。`server.allowed_hosts` が空でないとき、含まない構成は起動時に拒否される。
3. エッジの置き場の Pod を [deploy/k8s/store](../../deploy/k8s/store) で配る。その Service を、VPN の外から届かないようにする。エッジの worker には `--stores takamatsu` を渡す。
4. 置き場の目印を作って採用する（[setup.md](setup.md) §置き場の目印）。
5. 次の 3 つを確かめる。
   - `rokuban_storage_transfer_waiting_assets` が減る。
   - `rokuban_storage_transfer_last_success_timestamp_seconds` が進む。
   - 転送の実測レートが `bandwidth` の上限の下に収まる。アプリは実測レートを出さないので、網の計測で見る。

> **注意**: 規則がエッジを置き場にしている間と、転送が終わるまでは、エッジの置き場が原本の唯一のコピーである。この例では、30 日未満の高松の原本は desired が `takamatsu` なので、転送の候補にならない。`rokuban_storage_transfer_waiting_assets` が 0 でも、そのコピーはエッジにしか無い。エッジのディスクを交換・初期化する前に、エッジにしかコピーが無い資産が残っていないかを、次のクエリで確かめる。メトリクスにはこの件数を出すものが無い。

```sql
SELECT count(*) FROM media_assets a
WHERE EXISTS (SELECT 1 FROM active_media_asset_copies(a.id, 'takamatsu'))
  AND NOT EXISTS (
    SELECT 1 FROM active_media_asset_copies(a.id, NULL) c WHERE c.store <> 'takamatsu');
```

0 でなければ、ディスクを交換・初期化しない。急ぐ場合は、原本の規則でエッジを置き場から外し、転送でこの件数が 0 になるのを待つ。
