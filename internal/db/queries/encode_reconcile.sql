-- encode の desired−observed 定期 reconcile（internal/worker/encode_reconcile.go）が
-- 読むクエリ。record_sweep（watcher の定期全量突き合わせ）とは対象集合が違う
-- ので、クエリを共有せず新規に切る --- record_sweep が見るのは mirakc のエッジに
-- 残っている record（DB の外の観測）で、こちらは「原本をコミット済みなのに
-- desired なプロファイルの encoded が揃っていない録画」（DB だけで閉じる）。
--
-- # 名前付き述語 until_encoded_deletable_originals との関係
--
-- 「desired が全部揃っているか」は名前付き述語の view
-- `until_encoded_deletable_originals` にも入っている（あちらは**揃っている**側、
-- ここは**欠けている**側）。view をそのまま使えないのは、view の粒度が
-- 「削除してよい原本アセット 1 行」であり、`keep_original = 'until_encoded'` と
-- 「サムネイルが active」という**削除エンジン固有の条件**が畳み込まれているため
-- （このパスの対象は keep_original を問わない録画で、サムネイルの有無とも無関係）。
-- **同じ形の述語がこれで 3 箇所目**であることは意識して書いている ---
-- ドリフトの実績がある（view を切り出した理由そのもの。docs/storage/retention.md）。
-- 現時点の差分は 1 つだけで、意図的:
--
--   * ここは desired を `known_profiles`（現在の encode.profiles）で絞る。view は
--     絞らない。view にとって「設定から消えたプロファイル」は永久に揃わない条件
--     として原本を残す方向（安全側）に効くが、このパスにとっては投入しても
--     EncodeWorker が弾くだけのゴミになる（EncodeReconcileWorker の doc コメント）
--
-- 述語をスキーマ側の名前に寄せる案は検討した上で見送ることを決定済み。理由は 2 つ:
--
--   1. 粒度が違う。このパスの粒度は録画単位で、view の粒度はアセット単位
--      （削除エンジン固有の keep_original / サムネイル条件が畳み込まれている）。
--      寄せるなら view の切り直し（マイグレーション）が要る。
--   2. 本質的な理由。上の差分（known_profiles で絞るかどうか）は補集合ではなく、
--      両側とも今の挙動が正しい仕様として確定している。共通の述語にするには
--      known_profiles を引数に取る必要があり、view は引数を取れないので SQL 関数に
--      なる --- そうするとドリフトのリスクが述語の本体から呼び出し側の引数の
--      渡し方へ移るだけで、しかもそちらの方が読んで気付きにくい（呼び出し側 2 箇所を
--      突き合わせないと分からない）。寄せることはドリフトを減らさない。
--
-- したがって 3 箇所（この述語、until_encoded_deletable_originals、下の
-- ListUnsatisfiableEncodeProfiles）は共通化せず、この非対称を仕様として
-- コメントに固定する。ドリフトの検出は internal/worker のテストが担う
-- （同じフィクスチャで両側の答えが食い違うことを固定する）。

-- ListRecordingsMissingEncodes は「原本が active でコミット済み、かつ
-- known_profiles に含まれる desired のうち少なくとも 1 つについて active な
-- encoded が無い」録画のうち recording_id が after_recording_id より大きいものを
-- recording_id 昇順で返す。
--
-- 条件の意味:
--   * recording_encode_policy に行がある = エンコードポリシーが凍結済み
--     （行が無い = 未凍結。不変条件 10。JOIN で自然に落ちる）
--   * cardinality(encode_profiles) > 0 = desired が空でない
--   * 原本 media_assets（kind='original', state='active'）の EXISTS = ingest
--     コミット済み。ingest 未完了の録画を対象にしない。state='active' まで見るのは
--     EnqueueMissingEncodes 側の判定（GetActiveOriginalMediaAsset）と一致させる
--     ため --- 原本が until_encoded で物理削除済みの録画をここで候補に挙げても、
--     EnqueueMissingEncodes が no-op を返すだけで前進しない
--   * r.deleted_at IS NULL = ごみ箱の録画は対象外。ヒント経路（ingest 完了 /
--     POST /api/recordings/{id}/encode-profiles）は「今その録画に何かが起きた」
--     という個別のイベントで発火するが、この定期パスは全録画を毎回なめるので、
--     ユーザーが捨てた録画のエンコードを延々と再投入し続けることになる。
--     until_encoded_deletable_originals が同じ述語を持つのと同じ理由
--   * want.profile = ANY(known_profiles) = 現在の設定に存在するプロファイルだけを
--     欠落判定の対象にする。設定から消えたプロファイルを候補に含めると、投入しても
--     EncodeWorker が `unknown encode profile` で弾く（encode.go）録画が窓を
--     恒久的に占有し続ける（他の候補が減らない限り）。空文字列のプロファイル名が
--     ここで自動的に落ちるのも同じ仕組み（設定側の名前は必須検証済みなので
--     known_profiles に空文字列は入らない）
--   * p.recording_id > after_recording_id = 呼び出し側（EncodeReconcileWorker）が
--     持つ、プロセスローカルな再開位置。前パスが LIMIT ちょうどまで埋まったなら
--     続きから、そうでなければ 0（先頭）から見る。窓が「毎パス先頭から」ではなく
--     「前回止まった位置の続きから」開くことで、候補が一度も減らない最悪条件
--     （録画単位の恒久失敗が LIMIT 件を超える）でも有限パス数で全候補に到達する
--     （EncodeReconcileWorker の doc コメント「窓を回す」）。recording_id は PK
--     なのでこの keyset 述語はそのまま索引に乗る。
--
-- **known_profiles は non-NULL でなければならない。** `x = ANY(NULL::text[])` は
-- false ではなく NULL なので、NULL（Go 側の nil スライス）を渡すと EXISTS が
-- 常に偽になり候補が全滅する。しかも下の ListUnsatisfiableEncodeProfiles の
-- `NOT (... = ANY(...))` も同時に NULL になるため、**空振りしていることすら
-- 見えなくなる**（バックストップが無症状で死ぬ）。呼び出し側が渡すのは
-- config.EncodeConfig.ProfileNames() で、空設定でも non-nil を返す契約
-- （internal/config の TestEncodeConfig_ProfileNames_EmptyIsNonNil、
-- internal/worker の TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent
-- が両側から固定している）。
--
-- until_encoded_deletable_originals は known_profiles で絞らない。
-- これは意図的な非対称（安全側の仕様。上の「名前付き述語」節と
-- docs/storage/retention.md §保持ポリシー）であって揃え忘れではない。
--
-- name: ListRecordingsMissingEncodes :many
SELECT p.recording_id
FROM recording_encode_policy p
JOIN recordings r ON r.id = p.recording_id
WHERE p.recording_id > sqlc.arg('after_recording_id')::bigint
  AND cardinality(p.encode_profiles) > 0
  AND r.deleted_at IS NULL
  AND EXISTS (
    SELECT 1 FROM media_assets o
    WHERE o.recording_id = p.recording_id
      AND o.kind = 'original'
      AND o.state = 'active'
  )
  AND EXISTS (
    SELECT 1 FROM unnest(p.encode_profiles) AS want(profile)
    WHERE want.profile = ANY(sqlc.arg('known_profiles')::text[])
      AND NOT EXISTS (
        SELECT 1 FROM media_assets e
        WHERE e.recording_id = p.recording_id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile = want.profile
      )
  )
ORDER BY p.recording_id
LIMIT sqlc.arg('row_limit');

-- ListUnsatisfiableEncodeProfiles は上のクエリが**落とした**側 --- 凍結済みの
-- desired が known_profiles に無いために永久に満たせない (プロファイル名, 録画数)
-- を返す。
--
-- 落とす判断（上のクエリ）と落としたことを見せる責務を分けている。プロファイルを
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
-- name: ListUnsatisfiableEncodeProfiles :many
-- `want.profile::text` の明示キャストは必須（sqlc は unnest 由来の列型を
-- 推論できず interface{} で生成する。CLAUDE.md「sqlc は式の型を推論しきれない」）。
SELECT want.profile::text AS profile, count(*)::bigint AS recordings
FROM recording_encode_policy p
JOIN recordings r ON r.id = p.recording_id
CROSS JOIN LATERAL unnest(p.encode_profiles) AS want(profile)
WHERE r.deleted_at IS NULL
  AND NOT (want.profile = ANY(sqlc.arg('known_profiles')::text[]))
  AND EXISTS (
    SELECT 1 FROM media_assets o
    WHERE o.recording_id = p.recording_id
      AND o.kind = 'original'
      AND o.state = 'active'
  )
  AND NOT EXISTS (
    SELECT 1 FROM media_assets e
    WHERE e.recording_id = p.recording_id
      AND e.kind = 'encoded'
      AND e.state = 'active'
      AND e.profile = want.profile
  )
GROUP BY want.profile
ORDER BY want.profile;
