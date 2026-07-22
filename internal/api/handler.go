package api

import "context"

// version はビルド時に -ldflags で注入する (e.g. -ldflags "-X ...api.version=1.0.0")
var version = "dev"

type Server struct{}

func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}
