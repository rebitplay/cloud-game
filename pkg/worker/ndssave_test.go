package worker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNDSSaveUploadDirtyOnly(t *testing.T) {
	withPublicLocalhost(t)
	var uploads int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("content-type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Errorf("empty upload body")
		}
		atomic.AddInt32(&uploads, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	worker := &Worker{}
	upload := &ndsSaveUpload{sess: preparedNDSSession{SaveUploadURL: server.URL}}
	if err := worker.uploadNDSSaveRaw(upload, []byte("dirty")); err != nil {
		t.Fatal(err)
	}
	if err := worker.uploadNDSSaveRaw(upload, []byte("dirty")); err != nil {
		t.Fatal(err)
	}
	if err := worker.uploadNDSSaveRaw(upload, []byte("dirty-again")); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&uploads); got != 2 {
		t.Fatalf("uploads = %d, want 2", got)
	}
	if upload.status != "uploaded" {
		t.Fatalf("status = %q, want uploaded", upload.status)
	}
}

func TestNDSSaveUploadSerializesConcurrentFlushes(t *testing.T) {
	withPublicLocalhost(t)
	var uploads int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&uploads, 1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	worker := &Worker{}
	upload := &ndsSaveUpload{sess: preparedNDSSession{SaveUploadURL: server.URL}}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker.uploadNDSSaveRaw(upload, []byte("dirty")); err != nil {
				t.Errorf("uploadNDSSaveRaw: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&uploads); got != 1 {
		t.Fatalf("uploads = %d, want 1", got)
	}
}

func TestNDSSaveUploadFailureStatus(t *testing.T) {
	withPublicLocalhost(t)
	oldBackoff := ndsSaveUploadBackoff
	ndsSaveUploadBackoff = time.Nanosecond
	t.Cleanup(func() { ndsSaveUploadBackoff = oldBackoff })

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	worker := &Worker{}
	upload := &ndsSaveUpload{sess: preparedNDSSession{SaveUploadURL: server.URL}}
	if err := worker.uploadNDSSaveRaw(upload, []byte("dirty")); err == nil {
		t.Fatal("expected upload error")
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Fatalf("attempts = %d, want 4", got)
	}
	if upload.status != "failed" {
		t.Fatalf("status = %q, want failed", upload.status)
	}
}
