-- +goose Up
CREATE OR REPLACE VIEW public.removed_encoded_assets AS
 SELECT a.id AS asset_id,
    a.recording_id
   FROM ((public.media_assets a
     JOIN public.encoded_asset_removal_requests q ON (((q.recording_id = a.recording_id) AND (q.profile = a.profile))))
     JOIN public.recording_encode_policy p ON ((p.recording_id = a.recording_id)))
  WHERE ((a.kind = 'encoded'::text) AND (NOT (a.profile = ANY (p.encode_profiles))) AND (EXISTS ( SELECT 1
           FROM public.media_assets other
          WHERE ((other.recording_id = a.recording_id) AND (((other.kind = 'original'::text) AND (EXISTS ( SELECT 1
                   FROM public.active_present_media_asset_copies(other.id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))) OR ((other.kind = 'encoded'::text) AND (NOT (EXISTS ( SELECT 1
                   FROM public.encoded_asset_removal_requests oq
                  WHERE ((oq.recording_id = other.recording_id) AND (oq.profile = other.profile))))) AND (EXISTS ( SELECT 1
                   FROM public.active_present_media_asset_copies(other.id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))))));

COMMENT ON VIEW public.removed_encoded_assets IS 'removed_encoded_assets は、ユーザーが外した encoded 版のうち、他の版に active・実体ありのコピーが残るものを表す。他の版は active_present_media_asset_copies で判定し、実体無しの観測を最後の版を消さない根拠にする。外す encoded 自身は実体無しでも除かない。削除側は unlink の ErrNotExist から削除済みの確定まで進むので、除くと実体無しの行が残り続ける。';

-- +goose Down
CREATE OR REPLACE VIEW public.removed_encoded_assets AS
 SELECT a.id AS asset_id,
    a.recording_id
   FROM ((public.media_assets a
     JOIN public.encoded_asset_removal_requests q ON (((q.recording_id = a.recording_id) AND (q.profile = a.profile))))
     JOIN public.recording_encode_policy p ON ((p.recording_id = a.recording_id)))
  WHERE ((a.kind = 'encoded'::text) AND (NOT (a.profile = ANY (p.encode_profiles))) AND (EXISTS ( SELECT 1
           FROM public.media_assets other
          WHERE ((other.recording_id = a.recording_id) AND (((other.kind = 'original'::text) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(other.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))) OR ((other.kind = 'encoded'::text) AND (NOT (EXISTS ( SELECT 1
                   FROM public.encoded_asset_removal_requests oq
                  WHERE ((oq.recording_id = other.recording_id) AND (oq.profile = other.profile))))) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(other.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))))));

COMMENT ON VIEW public.removed_encoded_assets IS 'removed_encoded_assets は、ユーザーが外した encoded 版のうち、他の版が active で残るものを表す。実体無しを除かない。削除側は unlink の ErrNotExist から削除済みの確定まで進むので、除くと実体無しの行が残り続ける。他の版の判定も実体無しを除かないため、実体無しの原本も「残る版」として数える（この判定を実体で見るべきかは未決）。';
