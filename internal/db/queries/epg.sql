-- 全量同期のスイープ基準時刻。observed_at は DB の now() で書かれるため、
-- 基準時刻もアプリのクロックではなく DB から取る（クロックスキューで
-- プロジェクション全体を消して再投入する事故を防ぐ）。
-- name: EpgSweepMark :one
SELECT now()::timestamptz AS mark;

-- name: UpsertEpgService :batchexec
INSERT INTO epg_services (
    site, network_id, service_id, type, logo_id, remote_control_key_id,
    name, channel_type, channel, has_logo_data, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (site, network_id, service_id) DO UPDATE SET
    type                  = EXCLUDED.type,
    logo_id               = EXCLUDED.logo_id,
    remote_control_key_id = EXCLUDED.remote_control_key_id,
    name                  = EXCLUDED.name,
    channel_type          = EXCLUDED.channel_type,
    channel               = EXCLUDED.channel,
    has_logo_data         = EXCLUDED.has_logo_data,
    observed_at           = now();

-- name: DeleteStaleEpgServices :execrows
DELETE FROM epg_services
WHERE site = $1 AND observed_at < $2;

-- name: ListEpgServices :many
SELECT * FROM epg_services
WHERE site = $1
ORDER BY channel_type, remote_control_key_id, service_id;

-- name: UpsertEpgProgram :batchexec
INSERT INTO epg_programs (
    site, program_id, network_id, service_id, event_id,
    start_at, duration_ms, end_at, is_free,
    name, description, genre_lv1, extended, genres, video, audios, observed_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13, $14, $15, $16, now()
)
ON CONFLICT (site, program_id) DO UPDATE SET
    network_id  = EXCLUDED.network_id,
    service_id  = EXCLUDED.service_id,
    event_id    = EXCLUDED.event_id,
    start_at    = EXCLUDED.start_at,
    duration_ms = EXCLUDED.duration_ms,
    end_at      = EXCLUDED.end_at,
    is_free     = EXCLUDED.is_free,
    name        = EXCLUDED.name,
    description = EXCLUDED.description,
    genre_lv1   = EXCLUDED.genre_lv1,
    extended    = EXCLUDED.extended,
    genres      = EXCLUDED.genres,
    video       = EXCLUDED.video,
    audios      = EXCLUDED.audios,
    observed_at = now();

-- mirakc の EPG 収集は物理チャンネル単位（1 回チューニングして collect-eits を回す）なので、
-- スイープも「今回番組を返したチャンネルに属するサービス」に限定する。
-- あるチャンネルの収集失敗がそのチャンネルの番組表を消してしまうのを防ぐ。
-- 呼び出し側が対象サービスを network_id ごとにまとめて 1 回ずつ呼ぶ。
-- name: DeleteStaleEpgProgramsForServices :execrows
DELETE FROM epg_programs
WHERE site = $1
  AND observed_at < $2
  AND network_id = $3
  AND service_id = ANY(sqlc.arg(service_ids)::integer[]);

-- name: PruneEpgPrograms :execrows
DELETE FROM epg_programs
WHERE site = $1 AND end_at < $2;

-- name: CountEpgPrograms :one
SELECT count(*) FROM epg_programs WHERE site = $1;

-- name: ListEpgPrograms :many
SELECT * FROM epg_programs
WHERE site = $1
  AND start_at < sqlc.arg(window_end)::timestamptz
  AND end_at   > sqlc.arg(window_start)::timestamptz
  AND (sqlc.narg(network_id)::integer IS NULL OR network_id = sqlc.narg(network_id)::integer)
  AND (sqlc.narg(service_id)::integer IS NULL OR service_id = sqlc.narg(service_id)::integer)
ORDER BY start_at, network_id, service_id;

-- 一覧向けの軽い形。extended / video / audios は返さない（1 行あたり数 KB になり
-- 時間窓を広げたときの転送量が跳ねるため。詳細は GetEpgProgram で取る）。
-- name: ListEpgProgramsForList :many
SELECT site, program_id, network_id, service_id, event_id,
       start_at, duration_ms, end_at, is_free, name, description, genre_lv1
FROM epg_programs
WHERE site = $1
  AND start_at < sqlc.arg(window_end)::timestamptz
  AND end_at   > sqlc.arg(window_start)::timestamptz
  AND (sqlc.narg(network_id)::integer IS NULL OR network_id = sqlc.narg(network_id)::integer)
  AND (sqlc.narg(service_id)::integer IS NULL OR service_id = sqlc.narg(service_id)::integer)
ORDER BY start_at, network_id, service_id;

-- name: GetEpgProgram :one
SELECT * FROM epg_programs
WHERE site = $1 AND program_id = $2;

-- 手動予約の作成時に、予約行へスナップショットする番組の事実（title / 開始時刻 /
-- 尺 / チャンネル識別）を EPG プロジェクションから引く。mirakc の programId
-- 内部構造への算術（NID*10^10 + SID*10^5 + EID）に頼らないのは元々の理由のまま
-- だが、title / start_at / duration_ms も返すのは #27 の決定（「値の出所を EPG
-- 射影ただ 1 つに固定する」）による: 以前はチャンネル識別だけ射影から引き、
-- title / 開始時刻 / 尺はクライアント申告を信じていたため、GC の比較対象
-- （program_snapshots.start_at + duration_ms）がクライアントの古い番組表に
-- 引きずられ得た（api.CreateReservation から使う）。
-- name: GetProgramSnapshotSource :one
SELECT p.name AS title, p.start_at, p.duration_ms,
       s.network_id, s.service_id, s.channel_type, s.channel
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE p.site = $1 AND p.program_id = $2;
