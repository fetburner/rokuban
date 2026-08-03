-- issue #99 の付随決定: schedule_sync.reservation_id の FK（ON DELETE SET NULL）を
-- 外す。
--
-- 元々このカラムは「この observed schedule がどの reservations 行に対応するか」の
-- 便宜的なポインタとして internal/reconciler.observeSchedules が書いていた
-- （mirakc.IsOurs(s.Tags) で「自分が作った schedule か」を確認したうえで、その場で
-- GetReservationBySiteAndProgramID を引いて埋める。FK の参照整合性には依存して
-- いない --- 書き込み時点で自分で存在確認している）。
--
-- **この FK は現在どのコードからも読まれていない。** ListScheduleSyncsBySite
-- （このカラムを含む唯一の SELECT）はどこからも呼ばれておらず、reconciler の
-- 「自分の schedule か」の判定は常に tags（mirakc.IsOurs）で行っている
-- （observeSchedules・recreateChanged のコメント参照）。つまり FK が
-- ON DELETE SET NULL でこの値を NULL に落としても、それを見て「外部産」と
-- 誤判定する経路は今は存在しない --- issue #99 が挙げた症状（「observed 行が
-- 一時的に外部産と同じ見た目になる」）は、現状は実害の無い schema debt である。
--
-- それでも FK を外すのは、この「見た目」が手動での DB 調査（インシデント対応で
-- schedule_sync を直接 SELECT する場面）を混乱させる余地があるため:
-- ruler が予約を導出削除・再実体化した瞬間（EPG フリッカー・ルール編集）に、
-- 実際には自分の schedule のままなのに reservation_id だけが FK の
-- ON DELETE SET NULL で NULL になる。次の reconciler パス（UpsertScheduleSync の
-- 無条件上書き）で正しい値に戻るとはいえ、その窓を「なぜ NULL なのか」で
-- 調査すると #53 / #98 / #99 と同じ identity の話に毎回たどり着く。FK を外せば
-- 「reservations 側の delete で自動的に何かが起きる」という前提そのものが
-- 無くなり、値は reconciler が最後に観測した時点のまま残る（読み手が
-- reconciler の書き込み契機を知っていれば説明がつく）。
--
-- カラム自体（型・NULL 許容）は変えない --- internal/reconciler/reconciler.go
-- （別タスク #101 が担当中につきこの PR では触らない）の
-- UpsertScheduleSyncParams.ReservationID の呼び出し側はそのままで動く。
--
-- +goose Up
ALTER TABLE schedule_sync DROP CONSTRAINT IF EXISTS schedule_sync_reservation_id_fkey;

-- +goose Down

-- FK が無かった間に reservation_id が指す先の reservations 行が削除された行が
-- あり得る（このカラムを書く reconciler は生存確認せず上書きするため）。
-- 制約を復元する前に、その分だけ元の ON DELETE SET NULL と同じ効果
-- （参照先が無ければ NULL）を適用しておく。これをしないと ADD CONSTRAINT が
-- 既存データで失敗し、Down 自体が壊れる。
UPDATE schedule_sync s
SET reservation_id = NULL
WHERE s.reservation_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.id = s.reservation_id);

ALTER TABLE schedule_sync
    ADD CONSTRAINT schedule_sync_reservation_id_fkey
    FOREIGN KEY (reservation_id) REFERENCES reservations (id) ON DELETE SET NULL;
