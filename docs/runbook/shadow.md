> [runbook.md](../runbook.md) の一部。索引から辿る。

## EPGStation との並走

同じ mirakc を共有してよい。**チューナーの調停は mirakc が行う**ので、Rokuban が
EPGStation の録画を奪うことはない。ただし物理的な制約は残る。

### 同一チャンネルなら競合しない

mirakc は同じ物理チャンネルのストリームを複数の購読者で共有する。EPGStation が
GR/27 をライブ視聴していて Rokuban が GR/27 を録画する場合、チューナーは 1 本で足りる。

### 別チャンネルはチューナー数で競合する

チューナーが 1 本しかない環境では、EPGStation の録画と Rokuban の録画が別チャンネルに
なった時点でどちらかが録れない。負けた側は `recording.failed` の
`need-rescheduling` になる（`rokuban_recordings_failed_total{reason="need-rescheduling"}`）。

調停は `priority` で行う。Rokuban の既定は 10。EPGStation 側の優先度と揃えるか、
**並走中はチャンネルが重ならない番組で試す**のが安全。

### EPG 収集もチューナーを使う

mirakc の `update-schedules` ジョブ（既定 08:21 / 20:21、timeout 10 分）は
物理チャンネルごとにチューニングして EPG を集める。この時間帯に録画を入れると
競合しやすい。

逆にこのジョブが特定チャンネルで失敗すると、そのチャンネルの番組が
`/api/programs` から返らなくなる。Rokuban は**番組を返さなかったチャンネルの
プロジェクションを消さない**ようにしてあるが、
`rokuban_epg_channels_without_programs` が 0 以外で続くなら mirakc 側の
収集失敗を疑う。

### 二重録画に注意

Rokuban と EPGStation の両方に同じ番組の予約が入っていると、**同じ番組を 2 回録る**
（tag が違うので互いに相手の schedule を消さない）。reconciler は
rokuban tag（`program:{programId}`）が無い schedule を触らない。ディスクとチューナーを二重に消費するので、シャドー
運用中は片方だけに予約を入れる。

### shadow-diff で予約差分を確認する


EPGStation 側の API の形は**実機（v2.10.0）で確認済み**。`GET /api/reserves` は
`{ reserves: [...], total: n }` を返す。各要素は `programId`（数値の Mirakurun ID）を持つ。ほかに
`startAt` / `endAt`（UnixtimeMS）/ `isSkip` / `isConflict` / `isOverlap` /
`isTimeSpecified` / `ruleId`（ルール由来のみ）を持つ。別バージョンで並走する場合は
形が変わりうるので、最初に 1 回だけ確かめるとよい。

```sh
curl -s "$EPGSTATION_URL/api/reserves?type=all&isHalfWidth=false&limit=1&offset=0" | jq
```

並走の出口基準は「予約差分ゼロ or 全件説明可能」。
`rokuban shadow-diff` は Rokuban（DB）と EPGStation（API）の予約集合を programId で
突き合わせ、差分を標準出力にレポートするサブコマンド。

```sh
rokuban shadow-diff --config config.yml --epgstation-url http://localhost:8888
```

出力例（`Both` は件数のみ、`RokubanOnly` / `EPGStationOnly` / `Expected` は明細も出る）:

```
=== shadow-diff レポート ===
Both:            12
RokubanOnly:      1
EPGStationOnly:   0
Expected:         3

-- RokubanOnly（説明できない差分。EPGStation 側に対応する予約がない） --
programId        title       startAt (JST)
327360102415398  ○○第3話  2026-08-01 22:00:00 JST

-- Expected（allowlist で説明可能な差分） --
programId        title    startAt (JST)            reason
327360102415399           2026-08-01 23:00:00 JST  Rokuban 側でこの番組を除外している（program_intents.action = 'skip'）ため、EPGStation 側にのみ存在するのは想定通り
```

時刻は番組表と同じく JST 固定で表示する（実行環境のローカルタイムゾーンではない）。

**終了コード**: 説明できない差分（`RokubanOnly` か `EPGStationOnly` が 1 件でもある）
があれば `1`。CI や運用スクリプトから `rokuban shadow-diff ... && echo ok` のように
`&&` で連ねられる。

以下は allowlist（`Expected` に落ちる、説明可能な差分）:

| 条件 | 理由 |
|---|---|
| EPGStation 側が `isTimeSpecified` または `programId` 欠落 | 時刻指定予約は programId を持たず照合できない。Rokuban に時刻指定予約の機能はない（[recording.md §9](../recording.md)） |
| EPGStation 側が `isSkip` | EPGStation 側でユーザーが除外した予約 |
| EPGStation 側が `isOverlap` | EPGStation の重複排除ロジックで除外された予約 |
| Rokuban 側が skip 意図（`program_intents.action = 'skip'`） | Rokuban で除外した予約。EPGStation 側にだけ有るのは正常 |

**`isConflict` は allowlist に入らない**。チューナー競合は両者で起きる条件が同じはずで、
片方だけの予約に現れているなら `EPGStationOnly` / `RokubanOnly` として報告され、
調査が必要。

