-- +goose Up

-- 予約行にチャンネル識別情報をスナップショットする。
--
-- これまで予約行はチャンネルを持たず、mirakc の programId 内部構造
-- （Mirakurun 互換の NID*10^10 + SID*10^5 + EID 合成規則）を割り算して
-- serviceId を都度復元していた（internal/reconciler/reconciler.go）。
-- mirakc 固有の合成規則に本番コードが依存する状態であり、また issue #21
-- （チューナー容量超過の判定）の需要単位が (channel_type, channel) であるため、
-- どのみち予約行にチャンネルを持たせる必要がある。
--
-- すべて nullable にするのは、このマイグレーションを失敗させないため。
-- 移行前の予約行の中には、番組の放送が終わって EPG プロジェクション
-- （epg_programs / epg_services）から既に消えている行がありうる。そういう行は
-- 4 列とも埋めようがない。
--
-- NULL の意味は「移行前の残骸」のみ。新規に作られる予約は api の
-- CreateReservation が EPG プロジェクションから引いた値を必ず埋めるので、
-- 移行が終わった後に新規行が NULL になることはない
-- （reconciler は service_id が NULL の予約を意図的に schedule 化しない）。
ALTER TABLE reservations
    ADD COLUMN network_id   integer,
    ADD COLUMN service_id   integer,
    ADD COLUMN channel_type text,
    ADD COLUMN channel      text;

ALTER TABLE reservations
    ADD CONSTRAINT reservations_channel_type_check
    CHECK (channel_type IS NULL OR channel_type IN ('GR', 'BS', 'CS', 'SKY'));

-- backfill 1 段目: EPG プロジェクションから引く。
-- epg_programs で programId から network_id/service_id/event_id を、
-- epg_services で name/channel_type/channel を引く「本来の」経路。
-- 番組の放送がまだ EPG プロジェクションに残っている行はこれで埋まる。
UPDATE reservations r
SET network_id   = s.network_id,
    service_id   = s.service_id,
    channel_type = s.channel_type,
    channel      = s.channel
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE p.site = r.site AND p.program_id = r.program_id;

-- backfill 2 段目: 1 段目で埋まらなかった行のうち、programId から算術で
-- NID/SID を導出すれば epg_services を引ける行を救う。
--
-- この算術（Mirakurun 互換の ID 合成規則の逆算）は移行専用の便宜であり、
-- 本番コードから消す方針の依存そのものである。この UPDATE 以外の場所
-- （api / reconciler）で同じ式を書いてはならない。
UPDATE reservations r
SET network_id   = s.network_id,
    service_id   = s.service_id,
    channel_type = s.channel_type,
    channel      = s.channel
FROM epg_services s
WHERE r.network_id IS NULL
  AND s.site = r.site
  AND s.network_id = (r.program_id / (100000::bigint * 100000::bigint))::integer
  AND s.service_id = ((r.program_id / 100000::bigint) % 100000::bigint)::integer;

-- +goose Down

ALTER TABLE reservations
    DROP CONSTRAINT IF EXISTS reservations_channel_type_check,
    DROP COLUMN IF EXISTS network_id,
    DROP COLUMN IF EXISTS service_id,
    DROP COLUMN IF EXISTS channel_type,
    DROP COLUMN IF EXISTS channel;
