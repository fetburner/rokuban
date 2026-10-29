-- encode の desired−observed 定期 reconcile（internal/worker/encode_reconcile.go）と
-- api の待機列（internal/api/encode_queue.go）が読むクエリ。record_sweep（watcher の
-- 定期全量突き合わせ）とは対象集合が違うので、クエリを共有せず新規に切る ---
-- record_sweep が見るのは mirakc のエッジに残っている record（DB の外の観測）で、
-- こちらは「原本をコミット済みなのに desired なプロファイルの encoded が揃っていない
-- 録画」（DB だけで閉じる）。
--
-- # 共有する述語
--
-- 述語はスキーマ側の名前に置き、ここには直書きしない:
--
--   * 不足: view `unencoded_desired_profiles`（凍結済みの desired のうち active な
--     encoded が無い (recording_id, profile)。設定では絞らない）
--   * 録画単位: view `encodable_recordings`（ごみ箱でない・原本が active）
--   * プロファイル単位: 関数 `encodable_profile(rec_id, profile, known_profiles,
--     cut_profiles)`（空でない・現在の設定にある・cut なら確認済み）
--   * 試行上限: 関数 `encode_attempts_exhausted(rec_id, profile, attempt_limit)`
--     （failed かつ domain attempt 数が上限以上）
--   * 作り直し: view `unsatisfied_encode_rebuilds`（要求行のうち、押した時点の keep と
--     一致する凍結区間の active な encoded がまだ無いもの。ごみ箱・原本が active でない
--     録画を含まない）。cut の確認済み条件は掛けない
--   * 確認待ち: 関数 `awaiting_review_profiles(cut_profiles)`（desired の cut profile
--     のうち、原本が active で、章の所有・active な encoded・試行行・未充足の
--     作り直し要求が無い (recording_id, profile)）
--
-- 投入一覧（ListMissingEncodeProfiles）と待機列（ListEncodeQueue）は同じ条件で
-- 不足と満たされていない作り直しを選ぶ。片方にだけ条件を書き足すと、待機列に出るのに
-- 投入されない（またはその逆の）録画ができる。条件を足すときは上の述語名の定義を変える。
-- 両経路が同じ集合に合意することは、設定から引数を組み立てる api の配線も含めて
-- internal/api の TestEncodeReconcileAndQueueAgreeOnPendingSet が固定する。
--
-- # until_encoded_deletable_originals との関係
--
-- 削除側の関数 `until_encoded_deletable_originals(desired_store)` も、「desired が揃っている」を
-- unencoded_desired_profiles に行が無いことで判定する。設定での絞り（known_profiles）
-- は encodable_profile にだけ置き、unencoded_desired_profiles には足さない。足すと、
-- 設定からプロファイルを消す編集だけで、それを凍結した until_encoded 録画の原本が
-- 不可逆に消える（docs/storage/retention.md §保持ポリシー。internal/worker の
-- TestUnknownDesiredProfile_PredicateAsymmetry が両側から固定する）。

-- ListMissingEncodeProfiles は、不足プロファイルと満たされていない作り直し要求の和を
-- (recording_id, profile) 単位で返す。requested_keep は作り直し要求の行だけ非 NULL。
-- 両方に同じ録画・試行上限・対象 ID・継続カーソルの条件を掛け、作り直しは設定済みの
-- cut profile に限る。不足側は encodable_profile で設定とチャプター所有を確認し、同じ
-- profile に未充足の作り直しがあれば除外する（作り直し要求が優先する）。
-- recording_id が after_recording_id より大きい録画を recording_id 昇順で row_limit 件
-- 選んでから profile を展開するので、row_limit は profile 行数ではなく録画件数の上限になる。
-- 候補の判定は LIMIT 前に行い、完全な録画を先に消費しない。
--
-- 条件の意味:
--   * recording_encode_policy に行がある = エンコードポリシーが凍結済み
--     （行が無い = 未凍結。不変条件 10。unencoded_desired_profiles に行が出ない）。
--     desired が空なら不足の行も無いので候補にならない
--   * encodable_recordings = ごみ箱でなく、原本 media_assets が active（ingest
--     コミット済みで、until_encoded で物理削除されていない）。対象録画を絞った
--     即時パスもこのクエリを使うため、全件パスとごみ箱の扱いがずれない
--   * encodable_profile = 現在の設定に存在する非空のプロファイルで、cut なら
--     recording_chapter_ownership の行がある（不変条件 10: 行の存在 = ユーザーが
--     確認済み）。設定から消えたプロファイルを候補に含めると、投入しても
--     EncodeWorker が `unknown encode profile` で弾く（encode.go）録画が窓を
--     恒久的に占有し続ける。cut を確認前に投入すると、誤検出のまま本編が削られ、
--     原本がごみ箱を経由せずに消えて取り返せなくなる。確認 UI は cut でない
--     プロファイルの encoded で再生するので、原本 TS をブラウザで再生できないという
--     循環もここで断つ
--   * recording_id > after_recording_id = 継続ジョブの引数に保存したページ位置。
--     LIMIT まで埋まったページは最後の id を次のジョブに渡し、`--once` が worker を
--     作り直しても続きから見る。短いページでは継続ジョブを投入せず、次の定期パスが
--     0（先頭）から始まる（EncodeReconcileWorker の doc コメント「候補窓の継続」）。
--   * recording_id が NULL でなければ、その録画だけに候補を絞る（ingest 完了後や
--     encode_profiles の事後追加で投入する即時パス）。対象パスは after_recording_id
--     を 0 にし、全件パスの継続カーソルとは独立させる
--
-- **known_profiles は non-NULL でなければならない**（encodable_profile の注意。
-- NULL を渡すと候補が全滅する）。しかも下の ListUnsatisfiableEncodeProfiles の
-- `NOT (... = ANY(...))` も同時に NULL になるため、**空振りしていることすら
-- 見えなくなる**（バックストップが無症状で死ぬ）。呼び出し側が渡すのは
-- config.EncodeConfig.ProfileNames() で、空設定でも non-nil を返す契約
-- （internal/config の TestEncodeConfig_ProfileNames_EmptyIsNonNil、
-- internal/worker の TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent
-- が両側から固定している）。
--
-- name: ListMissingEncodeProfiles :many
WITH rebuilds AS (
  SELECT q.recording_id, q.profile, q.requested_keep,
         true AS is_rebuild, NULL::bigint AS ordinality
  FROM unsatisfied_encode_rebuilds q
  JOIN encodable_recordings er ON er.recording_id = q.recording_id
  WHERE q.profile = ANY(sqlc.arg('cut_profiles')::text[])
    AND q.recording_id > sqlc.arg('after_recording_id')::bigint
    AND (sqlc.narg('recording_id')::bigint IS NULL OR q.recording_id = sqlc.narg('recording_id')::bigint)
    AND NOT encode_attempts_exhausted(
      q.recording_id, q.profile, sqlc.arg('attempt_limit')::integer
    )
), missing AS (
  SELECT u.recording_id, u.profile, NULL::int8multirange AS requested_keep,
         false AS is_rebuild, min(u.ordinality) AS ordinality
  FROM unencoded_desired_profiles u
  JOIN encodable_recordings er ON er.recording_id = u.recording_id
  WHERE u.recording_id > sqlc.arg('after_recording_id')::bigint
    AND (sqlc.narg('recording_id')::bigint IS NULL OR u.recording_id = sqlc.narg('recording_id')::bigint)
    AND encodable_profile(u.recording_id, u.profile, sqlc.arg('known_profiles')::text[], sqlc.arg('cut_profiles')::text[])
    AND NOT EXISTS (
      SELECT 1 FROM unsatisfied_encode_rebuilds rebuild
      WHERE rebuild.recording_id = u.recording_id
        AND rebuild.profile = u.profile
    )
    AND NOT encode_attempts_exhausted(
      u.recording_id, u.profile, sqlc.arg('attempt_limit')::integer
    )
  GROUP BY u.recording_id, u.profile
), pending_profiles AS (
  SELECT * FROM rebuilds
  UNION ALL
  SELECT * FROM missing
), candidate_recordings AS (
  SELECT DISTINCT recording_id
  FROM pending_profiles
  ORDER BY recording_id
  LIMIT sqlc.arg('row_limit')
)
SELECT p.recording_id, p.profile, p.requested_keep, p.is_rebuild
FROM pending_profiles p
JOIN candidate_recordings c ON c.recording_id = p.recording_id
ORDER BY p.recording_id, p.is_rebuild DESC,
         CASE WHEN p.is_rebuild THEN p.profile END,
         p.ordinality;

-- ListEncodeQueue は encode 待機列を (recording_id, profile) 単位で返す。候補の述語は
-- ListMissingEncodeProfiles と同じ unencoded_desired_profiles / encodable_recordings /
-- encodable_profile / unsatisfied_encode_rebuilds / encode_attempts_exhausted を使う
-- （ファイル冒頭「共有する述語」）。満たされていない作り直しは、active な encoded が
-- 残っていても待機列に入る（active な asset の存在だけでは、押された keep で作られた
-- ことを意味しない）。満たされた要求は入らない。row_limit や after_recording_id は適用しない。
--
-- 作り直しの側には encodable_profile の cut の確認条件（recording_chapter_ownership）を
-- 掛けず、設定にある cut プロファイルかだけを見る。押した時点の keep が利用者の確認
-- そのものだからである（encodable_profile の定義の近くのコメントと同じ判断）。
--
-- 各行は「現在の desired で未観測」のプロファイルで、running は試行表に running 行が
-- ある印（行は queued と running の分割になる）。active な encoded が既にある、
-- ごみ箱、設定から消えた profile の running 行は pending_profiles か述語で落ちるので
-- 数えない。`encode_attempts_exhausted` は ListMissingEncodeProfiles と同じ判定を
-- 共有し、上限に達した failed 行だけを両方から落とす。旧 worker の attempt_count = 0
-- は 1 回と数える。再要求で failed 行が消えると両方の集合に戻る。
--
-- name: ListEncodeQueue :many
WITH pending_profiles AS (
  SELECT u.recording_id, u.profile
  FROM unencoded_desired_profiles u

  WHERE encodable_profile(u.recording_id, u.profile, sqlc.arg('known_profiles')::text[], sqlc.arg('cut_profiles')::text[])

  UNION

  SELECT q.recording_id, q.profile
  FROM unsatisfied_encode_rebuilds q
  WHERE q.profile = ANY(sqlc.arg('cut_profiles')::text[])
)
SELECT
  m.recording_id,
  m.profile,
  EXISTS (
    SELECT 1 FROM recording_encode_attempts a
    WHERE a.recording_id = m.recording_id
      AND a.profile = m.profile
      AND a.state = 'running'
  )::boolean AS running
FROM pending_profiles m
JOIN encodable_recordings er ON er.recording_id = m.recording_id
WHERE NOT encode_attempts_exhausted(
  m.recording_id, m.profile, sqlc.arg('attempt_limit')::integer
)
ORDER BY m.recording_id, m.profile;

-- GetEncodedAssetRebuildRequest はエンコード worker の preflight が読む。
-- ロックせず、desired・章・encoded 行と同じ RepeatableRead snapshot に置く。
-- 行が無ければ pgx.ErrNoRows。
-- name: GetEncodedAssetRebuildRequest :one
SELECT q.requested_keep
FROM encoded_asset_rebuild_requests q
WHERE q.recording_id = sqlc.arg('recording_id')
  AND q.profile = sqlc.arg('profile');

-- GetEncodedAssetRebuildRequestForShare は要求行を FOR SHARE で読む。行が無ければ
-- pgx.ErrNoRows。公開 tx では、読んでから公開するまで api の upsert（押し直し）を
-- 待たせ、ジョブの keep と requested_keep が等しいときだけ公開する。
-- name: GetEncodedAssetRebuildRequestForShare :one
SELECT q.requested_keep
FROM encoded_asset_rebuild_requests q
WHERE q.recording_id = sqlc.arg('recording_id')
  AND q.profile = sqlc.arg('profile')
FOR SHARE;

-- ListCutAwaitingReview は共有述語 awaiting_review_profiles から、確認待ちの
-- (recording_id, profile) をプロファイル名別に数える（メトリクス
-- rokuban_cut_awaiting_review）。active な encoded・試行行・未充足の作り直しがある
-- 組は確認待ちではなく、既に別の状態として扱われる。
--
-- **これは失敗ではない。** ユーザーが確認すれば次のパスが拾う正常な待ち状態で、
-- 数が減らないこと自体は異常ではない。出している理由は、cut プロファイルを
-- 選んだのに確認の導線に気付かず「エンコードされない録画」が静かに溜まる形を
-- 見えるようにするためである（ListUnsatisfiableEncodeProfiles が拾うのは設定から
-- 消えたプロファイルで、こちらは別の集合）。
--
-- 録画一覧のフィルタと同じ述語を使い、集計の独自コピーを作らない。
--
-- name: ListCutAwaitingReview :many
SELECT awaiting.profile::text AS profile, count(*)::bigint AS recordings
FROM awaiting_review_profiles(sqlc.arg('cut_profiles')::text[]) AS awaiting(recording_id, profile)
GROUP BY awaiting.profile::text
ORDER BY awaiting.profile::text;

-- ListAwaitingReviewRecordingIDs は確認待ちの録画 ID を 1 録画 1 行で返す。
-- 複数の cut profile があっても一覧行と awaitingReview 件数は録画単位である。
-- name: ListAwaitingReviewRecordingIDs :many
SELECT DISTINCT awaiting.recording_id::bigint AS recording_id
FROM awaiting_review_profiles(sqlc.arg('cut_profiles')::text[]) AS awaiting(recording_id, profile)
ORDER BY awaiting.recording_id::bigint;

-- ListUnsatisfiableEncodeProfiles は ListMissingEncodeProfiles が**落とした**側 --- 凍結済みの
-- desired が known_profiles に無いために永久に満たせない (プロファイル名, 録画数)
-- を返す。
--
-- 落とす判断（ListMissingEncodeProfiles）と落としたことを見せる責務を分けている。
-- 録画単位は同じ encodable_recordings を引く。プロファイルを
-- 改名 / 削除するとその名前で凍結済みの過去録画が一斉にここへ落ちるので、
-- 数を出さないと「エンコードされない録画」が静かに増える（過去に塞いだ
-- 症状そのものを、別の原因で再現してしまう）。
--
-- この値はプロファイル名別の該当録画数で、keep_original を問わず数える
-- （`always` の録画も含むので「回収されない原本の件数」そのものではない。
-- 1 録画が複数の消えたプロファイルを凍結していれば複数のラベルにまたがって
-- 数えられるため、ラベル間で単純合計すると録画数を超える）。この中の
-- until_encoded 録画は until_encoded_deletable_originals にとっても
-- 永久に「揃っていない」ため、原本が回収されない
-- （docs/storage/retention.md §保持ポリシー）。
--
-- cut の確認条件は持たない。cut_profiles は known_profiles と同じ設定
-- （config.EncodeConfig.CutProfileNames は ProfileNames と同じ Profiles から cut: true を
-- 選ぶ）から作る部分集合なので、known_profiles に無いプロファイルは cut でもない。
--
-- name: ListUnsatisfiableEncodeProfiles :many
SELECT u.profile, count(*)::bigint AS recordings
FROM unencoded_desired_profiles u
JOIN encodable_recordings er ON er.recording_id = u.recording_id
WHERE NOT (u.profile = ANY(sqlc.arg('known_profiles')::text[]))
GROUP BY u.profile
ORDER BY u.profile;
