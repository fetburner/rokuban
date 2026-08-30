package api

import "context"

// ListSites は config.mirakcs レジストリの site 名一覧を返す
// (GET /api/sites、issue #184 M4-12)。
//
// api は不変条件 1 によりどの site にも束縛されないため、フロントが
// パスパラメータ {site} に埋める値をどこかから取得する必要がある --- この
// エンドポイントがその公開面。ListEncodeProfiles と同じ規律で機微情報
// （mirakc の URL）は返さない。定義順（h.siteNames）で返す。
func (h *Server) ListSites(_ context.Context, _ ListSitesRequestObject) (ListSitesResponseObject, error) {
	return ListSites200JSONResponse(h.siteNames), nil
}
