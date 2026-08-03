-- issue #98 決定: 「番組終了時点で捕獲の試みが一度も記録されなかった」という
-- 観測を、reservations.orphaned_at という不可逆な列ではなく recordings の
-- 試行行（status='failed' + quality_events に recording.never-scheduled）と
-- して書く。orphaned_at はもう誰も書かない列になったので落とす
-- （CLAUDE.md 不変条件 12「表は行の寿命で割る」: reservations は ruler の
-- 1 パスの出力だけになる）。
--
-- recordings 行を作るには (site, network_id, service_id, event_id) という
-- 放送イベントの識別と service_name（表示名）が要る。従来の reconciler は
-- mirakc の record/schedule ペイロード（watcher 経由）からこれらを得ていたが、
-- markOrphaned は mirakc に一切問い合わせない（不変条件 1 は api ロール限定
-- だが reconciler も同じ理由で mirakc の live なデータに依存したくない --
-- 番組終了後は mirakc 側の schedule/record が既に無い前提の処理のため）。
-- そこで program_snapshots に event_id / service_name を追加し、他の
-- チャンネル識別列（network_id / service_id / channel_type / channel。#27 /
-- 00009）と全く同じ経路（EPG 射影 epg_programs / epg_services）でスナップ
-- ショットする。
--
-- event_id は mirakc の programId 内部構造（Mirakurun 互換の
-- NID*10^10 + SID*10^5 + EID 合成規則）を割り算して復元しない --- 00009 が
-- 本番コードから追放した依存そのもの。EPG 射影の epg_programs.event_id /
-- epg_services.name から他の列と同じ経路で引く。
--
-- +goose Up
ALTER TABLE program_snapshots
    ADD COLUMN event_id     integer,
    ADD COLUMN service_name text;

-- backfill: 射影にまだ残っている番組だけ event_id / service_name を埋める。
-- 00009 のような算術フォールバックは行わない（このマイグレーション自体が
-- 「算術に頼らない」という規律の対象なので、ここで破ると意味がない）。
-- 埋まらなかった行（射影から既に消えている）は NULL のままで、
-- reconciler.recordNeverScheduled が「識別できない」として同期対象から
-- 外してアラートする（resolveContentPath の service_id NULL 判定と同じ
-- 安全側の判断）。
UPDATE program_snapshots ps
SET event_id     = p.event_id,
    service_name = s.name
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE p.site = ps.site AND p.program_id = ps.program_id;

ALTER TABLE reservations DROP COLUMN orphaned_at;

-- +goose Down

-- **片道であることを明示的に許容する**（導出テーブルなので再構築できる。
-- CLAUDE.md 不変条件 11「導出表は churn がほぼ無害」、00008/00010/00012/00017
-- と同じ前例）。
--
-- 復元できないものは次の 2 つ:
--
--   * 過去に orphaned_at が立っていたという事実。この観測は既に recordings の
--     never-scheduled 行に移設済みだが、逆方向（recordings → orphaned_at への
--     backfill）は書かない --- 観測はもう recordings の管轄で、reservations に
--     書き戻すと 2 つの表に同じ意味の状態が同居する（CLAUDE.md 不変条件 12
--     の再発）。列は NULL のまま復元する
--   * event_id / service_name の逆再建。DROP COLUMN で単純に失うだけで、
--     再収集する手段は用意しない（Down は開発時のロールバック用途であり、
--     本番相当のデータを Down してから再度 Up する運用は想定しない）
ALTER TABLE reservations ADD COLUMN orphaned_at timestamptz;

ALTER TABLE program_snapshots
    DROP COLUMN event_id,
    DROP COLUMN service_name;
