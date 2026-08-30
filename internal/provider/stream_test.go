package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamBody 是被 FetchStream 与 Fetch 共同回放的固定内容，用于断言两条路径字节一致。
var streamBody = []byte("the quick brown fox jumps over the lazy dog, and then again, 0123456789")

// TestFetchStreamContentLengthAndBytes 锁住流式路径存在的理由本身：Content-Length 必须
// 原样透出（前端进度条的分母），字节必须与缓冲版逐字相同。
func TestFetchStreamContentLengthAndBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(streamBody)
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"

	st, err := m.FetchStream(context.Background(), "gitea", gitURL, "main", "big.txt", nil)
	if err != nil {
		t.Fatalf("FetchStream: %v", err)
	}
	defer st.Body.Close()

	if st.ContentLength != int64(len(streamBody)) {
		t.Errorf("ContentLength = %d, want %d", st.ContentLength, len(streamBody))
	}

	got, err := io.ReadAll(st.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	want, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "big.txt", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != string(want) || string(got) != string(streamBody) {
		t.Errorf("stream bytes differ: stream=%q buffered=%q", got, want)
	}
}

// TestFetchStreamStatusParity 锁住两条路径的错误映射一致。同一个 404 若在流式下不再
// 返回 ErrAuthRequired，私有仓库的凭据弹窗会在「查看源码」时静默失效。
func TestFetchStreamStatusParity(t *testing.T) {
	cases := []struct {
		name   string
		status int
		auth   *Auth
		want   error
	}{
		{"403 asks for auth", http.StatusForbidden, nil, ErrAuthRequired},
		{"401 asks for auth", http.StatusUnauthorized, nil, ErrAuthRequired},
		{"404 without auth asks for auth", http.StatusNotFound, nil, ErrAuthRequired},
		{"404 with auth is not found", http.StatusNotFound, &Auth{Type: "token", Token: "secret"}, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			m := newTestManager(t, 1<<20)
			gitURL := srv.URL + "/o/repo"

			_, bufErr := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", tc.auth)
			_, strErr := m.FetchStream(context.Background(), "gitea", gitURL, "main", "f.txt", tc.auth)
			if !errors.Is(bufErr, tc.want) {
				t.Errorf("Fetch err = %v, want %v", bufErr, tc.want)
			}
			if !errors.Is(strErr, tc.want) {
				t.Errorf("FetchStream err = %v, want %v", strErr, tc.want)
			}
		})
	}
}

// TestFetchStreamRejectsOversizedBeforeBody 上游如实报出超限长度时，必须在拿到 body
// 之前就返回 ErrTooLarge —— server 于是还能写出 413 页面而不是半截流。
func TestFetchStreamRejectsOversizedBeforeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 200))
	}))
	defer srv.Close()

	m := newTestManager(t, 50)
	_, err := m.FetchStream(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.bin", nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("expected ErrTooLarge, got %v", err)
	}
}

// TestFetchStreamCapsUndeclaredLength 覆盖谎报/不报长度的上游：没有 Content-Length 可提前
// 判定时，maxSizeReader 必须在流式路径上补上这道闸门，否则本服务会被借去无限外发。
// 用 Hijack 手写响应，才能确保响应里真的没有 Content-Length。
func TestFetchStreamCapsUndeclaredLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
		bw.Write(make([]byte, 200))
		bw.Flush()
	}))
	defer srv.Close()

	m := newTestManager(t, 50)
	st, err := m.FetchStream(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.bin", nil)
	if err != nil {
		t.Fatalf("FetchStream: %v", err)
	}
	defer st.Body.Close()

	if st.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1 (unknown)", st.ContentLength)
	}
	_, err = io.ReadAll(st.Body)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("expected ErrTooLarge while draining, got %v", err)
	}
}

// TestFetchStreamAsksForIdentity 锁住真实百分比的来源：CDN 在可压缩响应上会改用 chunked
// 并省掉 content-length，只有明确要 identity 才拿得到精确分母。
// 缓冲版不在这条约束里——它反正给不出进度，省带宽的 gzip 更划算。
func TestFetchStreamAsksForIdentity(t *testing.T) {
	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Accept-Encoding")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"
	if _, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	st, err := m.FetchStream(context.Background(), "gitea", gitURL, "main", "f.txt", nil)
	if err != nil {
		t.Fatalf("FetchStream: %v", err)
	}
	st.Body.Close()

	bufferedHeader := <-seen
	streamHeader := <-seen
	if streamHeader != "identity" {
		t.Errorf("FetchStream Accept-Encoding = %q, want identity", streamHeader)
	}
	if bufferedHeader == "identity" {
		t.Errorf("buffered Fetch should keep negotiating compression, got identity")
	}
}

// TestFetchStreamRefusesEncodedBody 上游无视 identity 时宁可报错：透明解压只覆盖
// client 自己加的头，覆盖不到我们手工加的那个。
func TestFetchStreamRefusesEncodedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte("not-really-gzip"))
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	_, err := m.FetchStream(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if err == nil {
		t.Fatal("expected refusal for an encoded body, got nil")
	}
	if !strings.Contains(err.Error(), "Content-Encoding") {
		t.Errorf("err = %v, want it to name Content-Encoding", err)
	}
}
