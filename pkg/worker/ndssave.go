package worker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro"
)

const ndsSaveFlushInterval = 60 * time.Second

var ndsSaveUploadBackoff = 250 * time.Millisecond

type ndsSaveUpload struct {
	cancel context.CancelFunc
	roomID string
	sess   preparedNDSSession

	mu      sync.Mutex
	lastSHA string
	status  string
}

func (w *Worker) startNDSSaveUpload(roomID string, app *libretro.Caged, session preparedNDSSession) {
	ctx, cancel := context.WithCancel(context.Background())
	upload := &ndsSaveUpload{cancel: cancel, roomID: roomID, sess: session, status: "unchanged"}

	w.saveUploads.mu.Lock()
	if old := w.saveUploads.uploads[roomID]; old != nil {
		old.cancel()
	}
	w.saveUploads.uploads[roomID] = upload
	w.saveUploads.mu.Unlock()

	go func() {
		ticker := time.NewTicker(ndsSaveFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := w.flushNDSSaveUploadWithApp(upload, app); err != nil {
					w.log.Warn().Err(err).Str("room", roomID).Msg("NDS periodic save upload failed")
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (w *Worker) flushNDSSaveUpload(roomID string) {
	w.saveUploads.mu.Lock()
	upload := w.saveUploads.uploads[roomID]
	w.saveUploads.mu.Unlock()
	if upload == nil {
		return
	}
	r := w.router.FindRoom(roomID)
	if r == nil {
		return
	}
	app := roomWithLibretro(r.App())
	if app == nil {
		return
	}
	if err := w.flushNDSSaveUploadWithApp(upload, app); err != nil {
		w.log.Warn().Err(err).Str("room", roomID).Msg("NDS final save upload failed")
	}
}

func (w *Worker) stopNDSSaveUpload(roomID string) {
	w.saveUploads.mu.Lock()
	upload := w.saveUploads.uploads[roomID]
	delete(w.saveUploads.uploads, roomID)
	w.saveUploads.mu.Unlock()
	if upload != nil {
		upload.cancel()
	}
}

func (w *Worker) flushNDSSaveUploadWithApp(upload *ndsSaveUpload, app *libretro.Caged) error {
	if upload == nil || upload.sess.SaveUploadURL == "" {
		return nil
	}
	raw, err := app.SaveSRAMRaw()
	if err != nil {
		upload.setStatus("failed")
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return w.uploadNDSSaveRaw(upload, raw)
}

func (w *Worker) uploadNDSSaveRaw(upload *ndsSaveUpload, raw []byte) error {
	sha := sha1String(raw)

	upload.mu.Lock()
	if upload.lastSHA == sha {
		upload.mu.Unlock()
		upload.setStatus("unchanged")
		return nil
	}
	upload.mu.Unlock()

	if err := putNDSSave(upload.sess.SaveUploadURL, raw); err != nil {
		upload.setStatus("failed")
		return err
	}
	upload.mu.Lock()
	upload.lastSHA = sha
	upload.status = "uploaded"
	upload.mu.Unlock()
	return nil
}

func (u *ndsSaveUpload) setStatus(status string) {
	u.mu.Lock()
	u.status = status
	u.mu.Unlock()
}

func putNDSSave(rawURL string, data []byte) error {
	if err := validateNDSRemoteURL(rawURL); err != nil {
		return err
	}
	var lastErr error
	backoff := ndsSaveUploadBackoff
	for attempt := 0; attempt < 4; attempt++ {
		err := putNDSSaveOnce(rawURL, data)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < 3 {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return lastErr
}

func putNDSSaveOnce(rawURL string, data []byte) error {
	client := http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return validateNDSRemoteURL(req.URL.String())
		},
	}
	req, err := http.NewRequest(http.MethodPut, rawURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("save upload returned %s", resp.Status)
	}
	return nil
}

func roomWithLibretro(a any) *libretro.Caged {
	if app, ok := a.(*libretro.Caged); ok {
		return app
	}
	return nil
}
