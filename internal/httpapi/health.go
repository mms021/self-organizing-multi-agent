package httpapi

import "net/http"

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	details := map[string]string{}

	if err := s.DB.PingContext(r.Context()); err != nil {
		status = "degraded"
		details["sqlite"] = err.Error()
	}
	if s.RedisPinger != nil {
		if err := s.RedisPinger.Ping(r.Context()); err != nil {
			status = "degraded"
			details["redis"] = err.Error()
		}
	}

	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "details": details})
}
