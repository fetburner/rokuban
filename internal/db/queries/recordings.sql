-- 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2 が最初の
-- 適用）の前段: 同一 active-event
-- (site, network_id, service_id, event_id, program_start_at) に
-- status='failed' の行が「生きて」（deleted_at IS NULL AND superseded_at IS NULL、
-- recordings_unique_active_event の述語）残っていれば、
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
-- event_id は同一サービス内で永続的な一意性を保証しない。ARIB TR-B14 第四編
-- 8.2.1 が保証するのはイベント終了から 24 時間なので、program_start_at も
-- 条件に含める。この発火条件は索引 recordings_unique_active_event と揃えてある:
-- failed 行が新しい record をブロックする条件はもともと program_start_at の
-- 一致そのものなので、この一致自体は機能的な穴を開けない。4 列だけで絞ると
-- event_id 再来時に無関係な過去の failed 行へ superseded_at を立ててしまう
-- （不変条件 9: 「この行が枠を明け渡した」という不可逆な事実を無関係な行に
-- 誤って書き込む）。
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
  AND program_start_at = sqlc.arg('program_start_at')
  AND deleted_at IS NULL AND superseded_at IS NULL AND status = 'failed';

-- 「番組終了時点で schedule が一度も観測されなかった」欠測の記録は recordings
-- ではなく never_scheduled_events 表（放送イベントキー）の行の存在で表す
-- （CreateNeverScheduledEvent、internal/db/queries/reservations.sql）。
-- recordings は観測された試行だけを持つ脊椎に戻り、書き手は watcher 1 人になる。

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

-- ON CONFLICT の対象列と述語は recordings_unique_active_event（issue #129 症状 2 で
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
-- ON CONFLICT の相手は生きている（deleted_at / superseded_at が NULL）同一
-- active-event の failed 行で、mirakc からの繰り返し通知に同じ理由を積み増す。
-- 欠測は recordings に行を作らなくなった（never_scheduled_events 表に移設）ので、
-- ここで欠測行と衝突することはもう無い。
ON CONFLICT (site, network_id, service_id, event_id, program_start_at)
    WHERE deleted_at IS NULL AND superseded_at IS NULL
DO UPDATE SET
    quality_events = recordings.quality_events || EXCLUDED.quality_events,
    updated_at     = now();

-- records API や schedules API から再構成した failed を保存する。SSE の
-- recording.failed は同じ通知を履歴として複数追記する CreateFailedRecording を
-- 使う一方、周期 sweep は同じ観測を何度も見るため、active-event の行を再利用する
-- このクエリで recordings の重複を防ぐ。
--
-- 再利用するのは status='failed' の行に限る。recordings_unique_active_event の
-- 述語は status を持たないので、ON CONFLICT の DO UPDATE 側で failed を要求する。
-- これがないと、sweep が failed 行を作った後に本物の success record が届いて
-- supersede した後、次パスでまだ残る failed schedule を観測したとき、supersede
-- 済み failed 行ではなく生きている success 行に衝突してその id を返し、
-- 成功した行へ recording.failed を追記してしまう（「本物の record が推論に必ず
-- 勝つ」の逆転）。DO UPDATE の WHERE が偽だと RETURNING は 0 行になり、呼び出し側
-- は pgx.ErrNoRows として「再利用する failed 行が無い」ことを検出して失敗を
-- 帰属させずに返す。
-- name: CreateOrGetFailedRecording :one
INSERT INTO recordings (
    rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16,
    'failed'
)
ON CONFLICT (site, network_id, service_id, event_id, program_start_at)
    WHERE deleted_at IS NULL AND superseded_at IS NULL
DO UPDATE SET
    updated_at = recordings.updated_at
WHERE recordings.status = 'failed'
RETURNING id;

-- 同じ失敗理由を records/schedules sweep が繰り返し観測しても品質イベントを
-- 増殖させない。event と reason の組を同一録画内の観測識別子として扱う。
-- record-broken など SSE のイベント履歴は既存の AppendQualityEvents でそのまま
-- 追記するので、mirakc から同じイベントが複数回届いた事実は失わない。
-- name: AppendQualityEventsIfMissing :execrows
UPDATE recordings AS r
SET quality_events = r.quality_events || sqlc.arg('events')::jsonb,
    updated_at = now()
WHERE r.id = sqlc.arg('id')
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_array_elements(r.quality_events) AS existing_event
      CROSS JOIN jsonb_array_elements(sqlc.arg('events')::jsonb) AS incoming_event
      WHERE existing_event->>'event' = incoming_event->>'event'
        AND existing_event->'reason' = incoming_event->'reason'
  );

-- 計測済みの判定は dropSummary と同じ view 1 つに寄せる（未計測・再計測待ちは 0 行）。
-- state では絞らない: tombstone 済みの原本も計測記録が一致すれば返す。
-- name: ListRecordingDropStats :many
SELECT d.pid, d.packets, d.drops, d.errors, d.scrambled, d.pid_type
FROM drop_stats d
JOIN current_ts_scanned_originals s ON s.media_asset_id = d.media_asset_id
JOIN media_assets a ON a.id = d.media_asset_id
WHERE a.recording_id = $1
ORDER BY d.pid;

-- 位置は PID 別統計とは別の行集合として読み、API 層で PID ごとの配列にまとめる。
-- jsonb_agg の型推論に依存せず、elapsed_ms の NULL を sqlc のポインタ型で保つ。
-- name: ListRecordingDropPositions :many
SELECT p.pid, p.byte_offset, p.elapsed_ms
FROM drop_positions p
JOIN media_assets a ON a.id = p.media_asset_id
WHERE a.recording_id = $1 AND a.kind = 'original'
ORDER BY p.pid, p.byte_offset;

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
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (sqlc.arg('recording_id'), sqlc.arg('keep_original'), sqlc.arg('encode_profiles')::text[], sqlc.arg('cm_detect'));

-- EnqueueMissingEncodes（internal/worker/encode.go）が desired
-- （recording_encode_policy.encode_profiles）を読むためのクエリ。行が無い
-- （未凍結）録画は pgx.ErrNoRows になるので、呼び出し側は「エンコード対象の
-- プロファイルが無い」と同じに扱う（keep_original='always' と同じ扱い。
-- docs/storage.md §6）。
-- name: GetRecordingEncodePolicy :one
SELECT keep_original, encode_profiles, cm_detect FROM recording_encode_policy WHERE recording_id = $1;

-- encode の公開判定 (c) が tx 内で desired を読み直す。FOR SHARE で版を外す
-- tx（LockRecordingEncodePolicy の FOR UPDATE）と直列化する。外す tx が先なら
-- 外した後の desired を見て公開せず、公開が先なら外す側がその版を消す。
-- name: GetRecordingEncodeProfilesForShare :one
SELECT encode_profiles FROM recording_encode_policy WHERE recording_id = $1 FOR SHARE;

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
-- 足し直した profile の外した要求（encoded_asset_removal_requests）は同じ文で消す。
-- 残すと、版がまだ消えていないうちに足し直したとき、view は desired に戻ったので
-- 消さないが、もう一度外したとき古い requested_at が残る。
-- name: AppendRecordingEncodeProfiles :exec
WITH cleared_removal_requests AS (
    DELETE FROM encoded_asset_removal_requests
    WHERE recording_id = sqlc.arg('id')
      AND profile = ANY (sqlc.arg('profiles')::text[])
)
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (
    sqlc.arg('id'),
    'always',
    (SELECT coalesce(array_agg(DISTINCT p ORDER BY p), '{}') FROM unnest(sqlc.arg('profiles')::text[]) AS p),
    false
)
ON CONFLICT (recording_id) DO UPDATE SET
    encode_profiles = (
        SELECT coalesce(array_agg(DISTINCT p ORDER BY p), '{}')
        FROM unnest(recording_encode_policy.encode_profiles || excluded.encode_profiles) AS p
    ),
    updated_at = now();

-- 凍結の 3 つ目の例外（ユーザー起点で 1 本ずつ減らす）の直列化点。同じ録画の
-- 版を同時に外す 2 本の tx が、互いに外す前の状態を見て両方 0 コピー検査を
-- 通らないよう、policy 行を先に取る。
-- name: LockRecordingEncodePolicy :one
SELECT keep_original, encode_profiles
FROM recording_encode_policy
WHERE recording_id = $1
FOR UPDATE;

-- policy 行が無い録画に、凍結済みとみなす既定値 'always' / 空 desired で行を作る。
-- 呼ぶのは active な encoded があるときだけ（AppendRecordingEncodeProfiles と同じ
-- 「実体があるなら凍結済み」）。この経路の録画は ingest を通らないので、ingest の
-- 素の INSERT（FreezeRecordingEncodePolicy）とは衝突しない。
-- name: FreezeRecordingEncodePolicyIfMissing :exec
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', false)
ON CONFLICT (recording_id) DO NOTHING;

-- desired から 1 つ外す（全置換ではない）。until_encoded で desired が空に
-- なるなら always に倒す（recording_encode_policy の CHECK。ingest のクランプと
-- 同じ向き）。版を外しても原本の削除は早まらない: until_encoded は全プロファイル
-- 揃いが条件なので、揃っている 1 本を外しても成否は変わらない。
-- name: RemoveRecordingEncodeProfile :exec
UPDATE recording_encode_policy
SET encode_profiles = array_remove(encode_profiles, sqlc.arg('profile')::text),
    keep_original = CASE
        WHEN keep_original = 'until_encoded'
         AND cardinality(array_remove(encode_profiles, sqlc.arg('profile')::text)) = 0
        THEN 'always'
        ELSE keep_original
    END,
    updated_at = now()
WHERE recording_id = sqlc.arg('recording_id');

-- 既に外した版をもう一度外しても冪等にする。
-- name: InsertEncodedAssetRemovalRequest :exec
INSERT INTO encoded_asset_removal_requests (recording_id, profile)
VALUES (sqlc.arg('recording_id'), sqlc.arg('profile'))
ON CONFLICT (recording_id, profile) DO NOTHING;

-- 外した後の状態で 0 コピー検査をする。判定は削除 reconcile と同じ名前付き述語
-- removed_encoded_assets（他の版が残ること）に任せ、ここで条件を複製しない。
-- name: IsRemovedEncodedAsset :one
SELECT EXISTS (
    SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = sqlc.arg('asset_id')
);

-- 録画後に原本の保持ポリシーだけを上書きする（issue #697）。UPDATE のみで
-- INSERT アームを持たない --- 行が無い（未凍結）録画は既に 'always' と同じ扱い
-- （docs/storage/retention.md §6）なので行を作る必要が無い。作ると ingest の
-- FreezeRecordingEncodePolicy（ON CONFLICT を意図的に付けない素の INSERT）が
-- PK 衝突し、原本 media_asset の INSERT と同一 tx ごとロールバックする
-- （不変条件 10「意味を持たない行を作らない」）。
--
-- cardinality の述語を UPDATE 自身の WHERE に置くことで、判定を Go 側に
-- 持ち出さずに CHECK 違反（recording_encode_policy_check）を防ぐ。
-- name: SetRecordingKeepOriginal :execrows
UPDATE recording_encode_policy
SET keep_original = sqlc.arg('keep_original'),
    updated_at = now()
WHERE recording_id = sqlc.arg('recording_id')
  AND (sqlc.arg('keep_original') <> 'until_encoded' OR cardinality(encode_profiles) > 0);

-- cm_detect の切り替えは凍結済み policy の更新、または active な原本を持つ
-- 未凍結録画の初回凍結として適用する。原本の状態はこの INSERT/UPDATE の瞬間に
-- 再評価し、読み取りと書き込みの間に原本が消える窓を作らない。
-- name: SetRecordingCMDetection :execrows
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
SELECT r.id, 'always', '{}', true
FROM recordings r
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
WHERE r.id = sqlc.arg('recording_id')
  AND r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
ON CONFLICT (recording_id) DO UPDATE
SET cm_detect = true,
    updated_at = now()
WHERE EXISTS (
    SELECT 1 FROM media_assets o
    WHERE o.recording_id = recording_encode_policy.recording_id
      AND o.kind = 'original' AND o.state = 'active'
      AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
)
  AND EXISTS (
    SELECT 1 FROM recordings r
    WHERE r.id = recording_encode_policy.recording_id AND r.deleted_at IS NULL
);

-- 未凍結録画で false は既定値との同値なので no-op。凍結済み policy だけ更新する。
-- name: ClearRecordingCMDetection :exec
UPDATE recording_encode_policy
SET cm_detect = false, updated_at = now()
WHERE recording_id = sqlc.arg('recording_id');

-- Playback positions are per recording row. The client decides when a position is
-- meaningful using the active media duration.
-- name: RecordingExistsForPlaybackState :one
SELECT EXISTS (
    SELECT 1 FROM recordings
    WHERE id = sqlc.arg('recording_id') AND purged_at IS NULL
);

-- name: UpsertRecordingPlaybackPosition :execrows
INSERT INTO recording_playback_positions (recording_id, position_ms, updated_at)
SELECT r.id, sqlc.arg('position_ms'), now()
FROM recordings r
WHERE r.id = sqlc.arg('recording_id')
  AND r.purged_at IS NULL
  AND sqlc.arg('position_ms')::bigint >= 2000
ON CONFLICT (recording_id) DO UPDATE SET
    position_ms = EXCLUDED.position_ms,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteRecordingPlaybackPosition :execrows
DELETE FROM recording_playback_positions p
USING recordings r
WHERE p.recording_id = r.id
  AND r.id = sqlc.arg('recording_id')
  AND r.purged_at IS NULL;

-- name: UpsertRecordingWatched :execrows
INSERT INTO recording_watched (recording_id, watched_at)
SELECT r.id, now()
FROM recordings r
WHERE r.id = sqlc.arg('recording_id')
  AND r.purged_at IS NULL
ON CONFLICT (recording_id) DO UPDATE SET watched_at = EXCLUDED.watched_at;

-- Remove every marker for the same broadcast event, including trash/superseded rows.
-- name: DeleteRecordingWatchedForEvent :execrows
DELETE FROM recording_watched w
USING recordings selected, recordings watched_recording
WHERE selected.id = sqlc.arg('recording_id')
  AND selected.purged_at IS NULL
  AND watched_recording.network_id = selected.network_id
  AND watched_recording.service_id = selected.service_id
  AND watched_recording.program_start_at = selected.program_start_at
  AND w.recording_id = watched_recording.id;
