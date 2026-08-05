-- 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2 が最初の
-- 適用）の前段: 同一 active-event (site, network_id, service_id, event_id) に
-- status='failed' の行が「生きて」（deleted_at IS NULL AND superseded_at IS NULL、
-- 00023 で追加した recordings_unique_active_event の述語）残っていれば、
-- superseded_at を立てて枠を明け渡させる。呼び出し側（internal/watcher の
-- createRecording）はこのクエリを CreateRecording の直前に同じトランザクション内で
-- 呼ぶ。
--
-- **1 つの WITH 句にまとめて CreateRecording 側の INSERT と一体化させなかった。**
-- 最初はその形（1 クエリで完結させる CTE）で書いたが、Postgres の WITH 内の
-- データ変更文は「主クエリと同時並行に実行され、順序は不定」（PostgreSQL
-- documentation, `WITH` Queries (Common Table Expressions)）で、この CTE を
-- 主 INSERT が参照していない（RETURNING id を読み捨てるだけ）ため、実際に
-- INSERT が一意制約違反で失敗するケースを手元のテストで確認した
-- （TestProcessRecord_SupersedesFailedRecording が最初に落ちた形。「実装を
-- 壊すと落ちることを確認する」の逆側の学びとして残す）。2 つの独立した
-- 文（この UPDATE を先に確定させてから次の INSERT を発行）に分けることで、
-- 同一トランザクション内でコマンドカウンタが進み、後続の INSERT が
-- 確実に更新後の索引状態を見る。
--
-- superseded_at は「この行が active-event の枠を明け渡した」という不可逆な事実
-- だけを持つ列で、ユーザーのごみ箱操作を表す deleted_at とは別物にした
-- （不変条件 9: 2 つの事実を同じ列に同居させない。deleted_at を流用すると
-- ごみ箱ビュー・GC がユーザー操作でない行をユーザー操作と誤読する）。
--
-- WHERE status = 'failed' に絞っているので、'recording'/'finished'/'canceled' の
-- 生きている行は巻き込まない —— それらと衝突する INSERT は素の一意制約違反として
-- 従来どおりエラーになる（同一イベントの本物の重複 record を黙って追い出すのは
-- このクエリの責務ではない）。
--
-- media_assets を持つ failed 行（途中まで録れて failed になった行）でも扱いは同じ:
-- superseded にするだけで media_assets.recording_id は書き換えない。ファイルの
-- 所有者は superseded になった旧 recordings 行のままで、物理削除は媒体削除
-- reconcile が recordings.deleted_at を見て判断するので、superseded だけでは
-- 何も物理的に消えない（internal/watcher の
-- TestProcessRecord_SupersedesFailedRecordingWithMediaAsset で固定）。
--
-- 対象の failed 行が無ければ 0 行のまま何もしない。record_sweep 等が同一 record を
-- 再処理しても、processRecord は record_sync の行ロックで 2 回目以降
-- createRecording 自体を呼ばない（internal/watcher/watcher.go の AcquireRecordSync
-- 参照）ので、このクエリも 2 回目以降は呼ばれず、superseded_at が二重に進んだり
-- 行が重複したりしない。
-- name: SupersedeFailedRecording :execrows
UPDATE recordings
SET superseded_at = now(), updated_at = now()
WHERE site = sqlc.arg('site')
  AND network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND event_id = sqlc.arg('event_id')
  AND deleted_at IS NULL AND superseded_at IS NULL AND status = 'failed';

-- issue #98 の決定: 「番組終了時点で捕獲の試みが一度も記録されなかった」という
-- reconciler 自身の観測を recordings の試行行として書く（reservations.orphaned_at
-- は廃止。00025）。呼び出し元は internal/reconciler の recordNeverScheduled のみ
-- （watcher の CreateFailedRecording とは別のクエリにしてある。理由は下記）。
--
-- **書き込み条件は「その放送イベントに生きている recordings 行が無いこと」**
-- （issue #59 の解消）。ON CONFLICT ... DO NOTHING がこれを担う ---
-- recordings_unique_active_event（(site, network_id, service_id, event_id)
-- WHERE deleted_at IS NULL AND superseded_at IS NULL）に既に生きている行が
-- あれば何もしない。生きている行は「本物の record」（watcher が作った
-- recording/finished/canceled/failed のどれか）でも「前パスで作った
-- never-scheduled 行」でもよく、どちらであっても「この放送イベントについて
-- 既に何か記録されている」という同じ結論になる:
--
--   - 本物の record が既にある場合 → 成功録画（またはその他の観測）を
--     never-scheduled 行が上書きすることはない（#59 本体の解消）
--   - 前パスの never-scheduled 行が既にある場合 → 2 回目のパスで 2 行目を
--     作らない（CLAUDE.md 不変条件 5: レベルトリガーの冪等性）
--
-- CreateFailedRecording（handleRecordingFailed 用）と分けたのは、あちらは
-- ON CONFLICT で quality_events を追記する意味論（mirakc からの繰り返し通知に
-- 同じ理由が積み増されることを許容する）だから。never-scheduled は
-- reconciler が毎パス同じ内容を送るだけなので、DO NOTHING で「初回だけ書く」
-- 意味論にする方が正確（CreateFailedRecording の DO UPDATE を流用すると、
-- 猶予期間中は毎パス quality_events の配列が伸び続けてしまう）。
--
-- never_scheduled（issue #161、00033）は quality_events のマーカーを型付き列に
-- 昇格したもので、この INSERT だけが true を書く。quality_events のマーカー
-- 自体は内訳ログとして引き続き積む（消さない）。
-- name: CreateNeverScheduledRecording :execrows
INSERT INTO recordings (
    rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title,
    program_start_at, program_duration_ms,
    status, quality_events, never_scheduled
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10,
    $11, $12,
    'failed', $13, true
)
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL AND superseded_at IS NULL
DO NOTHING;

-- name: CreateRecording :one
INSERT INTO recordings (
    rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16,
    $17, $18, $19
) RETURNING id;

-- name: UpdateRecordingStatus :exec
-- status は 'finished' / 'failed' / 'canceled' に達したら降格させない
-- （out-of-order な 'recording' イベントが後から来ても上書きしない。
-- 'canceled' は録画が再開しない取消なので他の 2 つと同じ終端として扱う。
-- issue #130）。
UPDATE recordings SET
    status     = CASE WHEN status IN ('finished', 'failed', 'canceled') THEN status ELSE sqlc.arg('new_status') END,
    started_at = COALESCE(started_at, sqlc.arg('started_at')),
    ended_at   = CASE WHEN sqlc.narg('ended_at')::timestamptz IS NOT NULL THEN sqlc.narg('ended_at') ELSE ended_at END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- ON CONFLICT の述語は recordings_unique_active_event（00023 で
-- `AND superseded_at IS NULL` を追加済み）と一字一句一致させる必要がある
-- （Postgres は ON CONFLICT の対象インデックスを述語込みで照合するため、
-- ずれると「there is no unique or exclusion constraint matching」で落ちる）。
-- 一致させておくことで、この INSERT が狙う相手は常に「生きている」行
-- （superseded 済みの過去の failed 行ではない）になる。
-- name: CreateFailedRecording :exec
INSERT INTO recordings (
    rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, quality_events
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16,
    'failed', $17
)
-- ON CONFLICT の相手が前パスの never-scheduled 行（never_scheduled = true）で
-- あることもありうる（reconciler が「schedule 非観測」と判定した後に、mirakc
-- が実は録画を試みて failed を報告してくるケース）。**この経路で
-- never_scheduled を false に落とさない**（issue #161 のレビューで一度入れて
-- 削除した。#161 が求めていたのは quality_events マーカーの型付き列への
-- 昇格だけで、「in-place 更新で never_scheduled をリセットする」という本番
-- 挙動の変更は範囲外 --- 旧 jsonb 版でもマーカーは `||` で追記されるだけで
-- 判定は true のまま変わらなかった。リセットは
-- CreateNeverScheduledRecording の ON CONFLICT DO NOTHING と対で「二度と
-- 復帰しない」除外の意味論を崩し、reconciler.go の endGuarded が前提にする
-- 「1 パスで自己解消し、以後 listDesired から二度と戻らない」を壊す。
-- 挙動を変えるならこの PR ではなく別 issue で決定を取る）。
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL AND superseded_at IS NULL
DO UPDATE SET
    quality_events = recordings.quality_events || EXCLUDED.quality_events,
    updated_at     = now();

-- 録画一覧。原本のサイズと PID 別 drop_stats の合計、
-- 再生可能な encoded プロファイル名を同梱する。
-- PID 別の内訳は行数が多く一覧では使わないので ListRecordingDropStats で別に取る。
--
-- encode_profiles は issue #159 で recording_encode_policy 衛星表に切り出された
-- ため r.* には含まれない。LEFT JOIN（policy 行が無い = 未凍結の録画も一覧に
-- 出す必要がある）で引き、COALESCE で '{}' に落とす --- 一覧表示は「凍結された
-- 空配列」と「未凍結」を区別する必要がないので、ここでは両者を同じ表示（省略）に
-- 潰してよい（区別が要る箇所は削除エンジンの until_encoded_deletable_originals
-- 側で、そこは JOIN のみで「行が無ければ対象外」を書いている）。
-- name: ListRecordings :many
SELECT
    r.*,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    COALESCE(p.encode_profiles, '{}')::text[] AS encode_profiles,
    -- ブラウザ再生用。desired（p.encode_profiles）ではなく observed（active encoded）。
    -- sqlc は array_agg の型を推論しきれないことがあるので text[] に明示キャストする。
    (
        SELECT coalesce(array_agg(e.profile ORDER BY e.profile), '{}')::text[]
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile IS NOT NULL
    ) AS available_encoded_profiles
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
WHERE r.site = $1 AND r.deleted_at IS NULL
ORDER BY r.program_start_at DESC, r.id DESC;

-- name: ListRecordingDropStats :many
SELECT d.pid, d.packets, d.drops, d.errors, d.scrambled, d.pid_type
FROM drop_stats d
JOIN media_assets a ON a.id = d.media_asset_id
WHERE a.recording_id = $1 AND a.kind = 'original' AND a.state <> 'deleted'
ORDER BY d.pid;

-- name: AppendQualityEvents :exec
UPDATE recordings
SET quality_events = quality_events || sqlc.arg('events')::jsonb,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- ingest が原本 media_asset のコミットと同じ tx で焼く「この録画の望ましい
-- 最終状態」（M3-14、issue #103）。issue #159 で recording_encode_policy 衛星表に
-- 切り出されたため、凍結 = この行の INSERT（不変条件 3「コミット = DB 行」・
-- 不変条件 10「意味を持たない行を作らない」: 行が無い = 未凍結、行がある =
-- 凍結済み。既定値との区別不能が構造的に消える）。ON CONFLICT を付けない ---
-- 呼び出し元 internal/worker/ingest.go の resolveAndSnapshotEncodePolicy は
-- CreateMediaAsset（同じく ON CONFLICT 無しの INSERT）と同一 tx から 1 回だけ
-- 呼ばれる（Work が転送開始前に GetOriginalMediaAssetID で冪等性チェックする
-- ため、この tx 自体が録画ごとに 1 回しか実行されない）。凍結する理由・瞬間・
-- 冪等性の詳細は resolveAndSnapshotEncodePolicy の doc コメント参照。
-- 予約が解決できない録画（手動で mirakc に起こされた録画・GC 済みの GetReservationEncodePolicyByEvent
-- 失敗等）でも呼び出し側は既定値（'always' / '{}'）でこのクエリを呼ぶ ---
-- 凍結自体はスキップしない（原本 media_asset の有無で「凍結済みか」を判定する
-- backfill の基準、および issue #133 の事後追加が「行が既にある」ことを前提に
-- できることの両方を守るため。resolveAndSnapshotEncodePolicy の doc コメント
-- 「解決に失敗しても凍結する」参照）。行が無いのは原本がまだコミットされて
-- いない（ingest 未完了）ときだけ。
-- name: FreezeRecordingEncodePolicy :exec
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES (sqlc.arg('recording_id'), sqlc.arg('keep_original'), sqlc.arg('encode_profiles')::text[]);

-- EnqueueMissingEncodes（internal/worker/encode.go）が desired
-- （recording_encode_policy.encode_profiles）を読むためのクエリ。行が無い
-- （未凍結）録画は pgx.ErrNoRows になるので、呼び出し側は「エンコード対象の
-- プロファイルが無い」と同じに扱う（keep_original='always' と同じ扱い。
-- docs/storage.md §6）。
-- name: GetRecordingEncodePolicy :one
SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1;

-- 凍結の例外としての事後追加（issue #133、docs/storage.md §6「原本 TS の
-- 保持ポリシー」・docs/recording/reservation-model.md §4.5「録画開始後の編集」）。
-- **追加専用**（union + dedup）。全置換にすると、ユーザーが既存のプロファイル
-- 指定を誤って消せてしまう（keep_original='until_encoded' のまま
-- encode_profiles を空にする事故。CHECK 制約は守られるが、意図しない
-- プロファイル消失そのものは防げない）。呼び出し側（api）は原本削除済み
-- （GetActiveOriginalMediaAsset が ErrNoRows）を先に検査して 409 にすること
-- --- このクエリ自体は原本の有無を見ない。
--
-- ON CONFLICT (recording_id) DO UPDATE にしてある --- 行が無い（未凍結）
-- ケースを INSERT で埋める。resolveAndSnapshotEncodePolicy（ingest）を経由
-- しない原本（internal/inplace.Register の災害復旧経路。issue #159 レビューで
-- 発見）は recording_encode_policy 行を作らないため、「原本が active なら
-- 行が必ずある」は不変条件ではない。ここで 0 行をエラーにすると、原本ありの
-- 録画への事後追加依頼そのものが失敗する（issue #133 が解こうとした問題の
-- 再発）。行が無い場合は「原本が active = この録画は凍結済みとみなす」を
-- 適用し、keep_original は既定値 'always'（recordings 旧列の既定値と同じ、
-- 安全側）で新規に凍結する。既存行がある場合は encode_profiles だけ
-- union + dedup で追記し、keep_original は変更しない。
-- name: AppendRecordingEncodeProfiles :exec
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES (
    sqlc.arg('id'),
    'always',
    (SELECT coalesce(array_agg(DISTINCT p ORDER BY p), '{}') FROM unnest(sqlc.arg('profiles')::text[]) AS p)
)
ON CONFLICT (recording_id) DO UPDATE SET
    encode_profiles = (
        SELECT coalesce(array_agg(DISTINCT p ORDER BY p), '{}')
        FROM unnest(recording_encode_policy.encode_profiles || excluded.encode_profiles) AS p
    ),
    updated_at = now();
