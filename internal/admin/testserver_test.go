package admin

import (
	"net/http"
	"net/http/httptest"
)

// newServer 是 httptest.NewServer 的薄封装，集中管理测试用 server。
func newServer(mux *http.ServeMux) *httptest.Server {
	return httptest.NewServer(mux)
}
