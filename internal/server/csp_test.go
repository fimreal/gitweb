package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestViewerCSPHasNonceAndSelfScriptSrc 断言 viewer 的 CSP 形态。
// 拆内联脚本到 /static/js/viewer/*.js 依赖 script-src 'self' 已放行——
// 若这条断言失败，说明 CSP 被收紧，拆分方案需要重新评估。
func TestViewerCSPHasNonceAndSelfScriptSrc(t *testing.T) {
	r, reg := newTestServer(t)
	if _, err := reg.Register("https://github.com/o/demo.git", "demo", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/demo/", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("viewer response has no Content-Security-Policy header")
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP script-src must allow 'self' (external viewer scripts depend on it): %s", csp)
	}
	if !strings.Contains(csp, "nonce-") {
		t.Errorf("CSP must carry a per-request nonce for the inline bootstrap: %s", csp)
	}
	// nonce 必须每次请求不同。
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/demo/", nil))
	if csp == w2.Header().Get("Content-Security-Policy") {
		t.Error("CSP nonce did not change between two requests — it is not per-request")
	}
}
