package protocol

import "net/http"

type testResponseWriter struct {
	header http.Header
	body   []byte
	status int
}

func newTestResponseWriter() *testResponseWriter {
	return &testResponseWriter{header: make(http.Header)}
}

func (w *testResponseWriter) Header() http.Header { return w.header }
func (w *testResponseWriter) Write(data []byte) (int, error) {
	w.body = append(w.body, data...)
	return len(data), nil
}
func (w *testResponseWriter) WriteHeader(status int) { w.status = status }
func (w *testResponseWriter) Flush()                 {}
