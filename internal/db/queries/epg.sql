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

-- has_programs は「射影全体で 1 件でも番組を持つか」であり、表示中の時間窓では
-- 判定しない。時間窓に依存させると、フロントのピッカー候補が「ページを読み込む
-- ほど増える」形になり、しかもサーバー側で serviceId 絞り込みをかけたときに
-- 候補が絞り込み結果に連動して縮む（1 局に絞ると他局へ切り替えられなくなる）
-- 問題を再発させる。候補集合を絞り込みから独立させるのがこの列を足す理由
-- そのものなので、判定は射影全体で固定する（docs/frontend.md「番組リスト」）。
-- name: ListEpgServices :many
SELECT es.*,
    EXISTS (
        SELECT 1 FROM epg_programs ep
        WHERE ep.site = es.site
          AND ep.network_id = es.network_id
          AND ep.service_id = es.service_id
    ) AS has_programs
FROM epg_services es
WHERE es.site = $1
ORDER BY es.channel_type, es.remote_control_key_id, es.service_id;

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
--
-- exact_network_ids / exact_service_ids は同じ添字が 1 組で、呼び出し側が必ず
-- 同じ長さにして渡す。複数組は OR。空/NULL ならその条件を効かせない。
-- `unnest(a, b)` は sqlc の組み込み analyzer が解決できないため、start_delay.sql と
-- 同じ generate_subscripts + 添字参照を使う。
-- name: ListEpgProgramsForList :many
SELECT site, program_id, network_id, service_id, event_id,
       start_at, duration_ms, end_at, is_free, name, description, genre_lv1
FROM epg_programs
WHERE site = $1
  AND start_at < sqlc.arg(window_end)::timestamptz
  AND end_at   > sqlc.arg(window_start)::timestamptz
  AND (
    coalesce(cardinality(sqlc.arg(exact_network_ids)::integer[]), 0) = 0
    OR EXISTS (
      SELECT 1
      FROM generate_subscripts(sqlc.arg(exact_network_ids)::integer[], 1) AS i
      WHERE (sqlc.arg(exact_network_ids)::integer[])[i] = epg_programs.network_id
        AND (sqlc.arg(exact_service_ids)::integer[])[i] = epg_programs.service_id
    )
  )
ORDER BY start_at, network_id, service_id;

-- name: GetEpgProgram :one
SELECT * FROM epg_programs
WHERE site = $1 AND program_id = $2;

-- 意図・上書きの書き込み時に、program_snapshots へスナップショットする番組の事実
-- （title / 開始時刻 / 尺 / チャンネル識別）を EPG プロジェクションから引く。
-- mirakc の programId 内部構造への算術（NID*10^10 + SID*10^5 + EID）に頼らないのは
-- 元々の理由のままだが、title / start_at / duration_ms も返すのは #27 の決定
-- （「値の出所を EPG 射影ただ 1 つに固定する」）による: 以前はチャンネル識別だけ
-- 射影から引き、title / 開始時刻 / 尺はクライアント申告を信じていたため、GC の
-- 比較対象（program_snapshots.start_at + duration_ms）がクライアントの古い番組表に
-- 引きずられ得た（api.ensureProgramSnapshot から使う）。
-- event_id / s.name (service_name) は issue #98 で追加した参照 --- program_snapshots
-- 側の event_id / service_name 列（00025）を埋めるのに使う。他のチャンネル識別列と
-- 同じ経路（射影から直接引く。mirakc の programId 分解には頼らない）。
-- name: GetProgramSnapshotSource :one
SELECT p.name AS title, p.start_at, p.duration_ms,
       s.network_id, s.service_id, s.channel_type, s.channel,
       p.event_id, s.name AS service_name
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE p.site = $1 AND p.program_id = $2;
