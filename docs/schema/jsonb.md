## 8. jsonb ドキュメント形式

### 予約オプション（reservations.base / program_intents.overrides、同形）

キーは camelCase（Go の JSON 規約と揃える）。overrides は「ユーザーが上書きしたキーのみ」を持つ疎なドキュメント。

```jsonc
{
  "skip": false,                     // true なら mirakc schedule を作らない
  "priority": 1,                     // mirakc RecordingOptions.priority
  "contentPath": "2026/07/タイトル_20260723.m2ts",  // recording.basedir 相対。サニタイズ済み
  "filenameTemplate": "{{.Year}}{{.Month}}{{.Day}}/{{.Hour}}{{.Min}}{{.Sec}}_{{.Title}}_{{.ServiceID}}",  // Go text/template。reconciler が展開し、ルール作成時に検証される（recording.md §3.2）
  "encodeProfiles": ["h265-1080p"],  // 設定ファイル定義のプロファイル名（M2〜）
  "keepOriginal": "untilEncoded"     // "always" | "untilEncoded"
}
```

- M1 では ruler がないため base = NULL、manual 予約の全フィールドが `program_intents.overrides` に入る
- `skip` は overrides のキーではなく `program_intents.action` の列（§3.5）
- `filenameTemplate` と `contentPath` は両方指定されうるが、`contentPath`（展開済みのフルパス）が優先される。`filenameTemplate` はルール由来（ruler が base に載せる）かユーザーの明示的な上書きのどちらか
- 検証はアプリ層（Go の struct へのマッピング）で行う。DB は形を強制しない
- **命名規則の境界**: jsonb 内は camelCase（Go/JSON 規約）、SQL カラムは snake_case。recordings へのスナップショット時にアプリ層が変換する（例: jsonb の `"keepOriginal": "untilEncoded"` → SQL の `keep_original = 'until_encoded'`）

### quality_events（recordings、append-only 配列）

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

mirakc の `FailedReason`（discriminated union）をそのまま格納。

