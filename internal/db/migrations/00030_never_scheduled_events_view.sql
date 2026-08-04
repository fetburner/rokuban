-- +goose Up

-- 「この放送イベントに never-scheduled の失敗試行行があるか」という述語に
-- 名前を与える（issue #157）。
--
-- この述語は internal/db/queries/reservations.sql の
-- ListReservationsForSyncEvaluation、overlaps.sql の
-- ListOverlappingReservations、capacity.sql の ListCapacityDemand の
-- 3 ファイルに一字一句複製されていた。一致は各ファイルの「揃えること」
-- というコメント（覚えておくべき規則）でしか守られておらず、CLAUDE.md
-- 不変条件 10「あってはいけない組み合わせは CHECK で禁止するより表現不可能
-- にする」の逆側にいた。sqlc がクエリを静的な SQL 文として持つ制約上、
-- 述語が 1 文字でもドリフトすると同期・重なり判定・容量判定が同じ予約に
-- ついて違う答えを出しうる --- しかもテストは 3 本それぞれ独立に通りうる。
--
-- program_investments view（#162。00027_program_investments_view.sql）と
-- 同じ論法: 述語にスキーマ上の名前を与えれば、ズレは規約でなく表現不可能に
-- なる。Postgres は単純な view をクエリにインライン展開するので実行計画は
-- 変わらない。
--
-- 列は (site, network_id, service_id, event_id) という放送イベントの識別と
-- deleted_at / superseded_at を出す。後者 2 列は同期除外 3 クエリ自身は
-- 使わない（"live な行に限る" という絞り込みは同期除外には無い --- 一度
-- never-scheduled と判定された放送イベントは、その後 recordings 行が
-- supersede されても同期対象には戻らない。#129 / #143 の「本物の record が
-- 推論に必ず勝つ」は表示側の話であって、同期除外の対象ではない）。
-- 表示用の never_recorded（reservations.sql の GetReservationFull /
-- GetReservationFullBySiteAndProgramID / ListReservationsBySite）は同じ核に
-- 加えて deleted_at IS NULL AND superseded_at IS NULL という live 限定を
-- 持つ、意図的に別の述語である（reservations.sql 冒頭のコメントが理由の
-- 権威）。この差を view の中に畳み込んで消してはならないので、view 自身は
-- フィルタせず列として出し、live 限定は表示側が呼び出し元で足す。
CREATE VIEW never_scheduled_events AS
SELECT site, network_id, service_id, event_id, deleted_at, superseded_at
FROM recordings
WHERE status = 'failed'
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(quality_events) qe
      WHERE qe->>'event' = 'recording.never-scheduled'
  );

-- +goose Down

DROP VIEW IF EXISTS never_scheduled_events;
