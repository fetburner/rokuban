> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§8）。

## 8. jsonb ドキュメント形式

DB に jsonb で持つドキュメントの形を、予約オプション・品質イベント・失敗理由の順に示す。キーは camelCase にそろえる。

### 予約オプション（reservations.base / program_overrides.overrides、同形）

予約オプションは camelCase のキーを持つ JSON で、検証はアプリ層が行い、DB は形を強制しない。上書き側はユーザーが上書きしたキーだけを持つ疎なドキュメントで、予約の意図の表とは別の上書きの表にある（§3.5）。

```jsonc
{
  "skip": false,                     // true なら mirakc schedule を作らない
  "priority": 1,                     // mirakc RecordingOptions.priority
  "contentPath": "2026/07/タイトル_20260723.m2ts",  // recording.basedir 相対。サニタイズ済み
  "filenameTemplate": "{{.Year}}{{.Month}}{{.Day}}/{{.Hour}}{{.Min}}{{.Sec}}_{{.Title}}_{{.ServiceID}}",  // Go text/template。reconciler が展開し、ルール作成時に検証される（recording.md §3.2）
  "encodeProfiles": ["h265-1080p"],  // 設定ファイル定義のプロファイル名
  "keepOriginal": "untilEncoded"     // "always" | "untilEncoded"
}
```

ruler が基底を供給しない manual 予約は、基底が NULL で、全フィールドが上書き側に入る。スキップは意図の表の行動列に持ち、上書きのキーには入れない（§3.5）。

`filenameTemplate` と `contentPath` は両方指定されうるが、`contentPath`（展開済みのフルパス）が優先される。`filenameTemplate` はルール由来（ruler が基底に載せる）か、ユーザーの明示的な上書きのどちらかである。検証は Go の struct へのマッピングで行う。

jsonb 内は camelCase（Go と JSON の規約）、SQL のカラムは snake_case で、命名規則の境界になる。録画履歴へスナップショットするとき、アプリ層が変換する。例は jsonb の `"keepOriginal": "untilEncoded"` が、SQL の `keep_original = 'until_encoded'` になる変換である。

<details><summary>根拠</summary>

- 意図の表と上書きの表: `program_intents`、`program_overrides`
- 上書きのキーを持つ列: `program_overrides.overrides`
- スキップの置き場: `program_intents.action`、上書きのキー `skip`
- 基底が NULL になる条件: `base`

</details>

### quality_events（recordings、append-only 配列）

品質イベントは、録画の失敗と record 破損の履歴を、追記だけの配列で持つ。理由は mirakc のペイロードをそのまま格納する。

```jsonc
[
  {
    "at": "2026-07-23T21:05:00+09:00",
    "event": "recording.failed",             // recording.failed | recording.record-broken
    "reason": { "type": "io-error", "message": "...", "osError": 28 }  // mirakc の payload そのまま
  }
]
```

### failed_reason（schedule_sync / record_sync）

失敗理由は、mirakc の失敗理由（discriminated union）をそのまま格納する。

<details><summary>根拠</summary>

- mirakc の失敗理由の型: `FailedReason`

</details>

