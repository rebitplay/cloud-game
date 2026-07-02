package worker

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	stdos "os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/games"
	zipc "github.com/giongto35/cloud-game/v3/pkg/worker/compression/zip"
)

const (
	maxNDSRomBytes  = 512 << 20
	maxNDSSaveBytes = 32 << 20
)

func (c *coordinator) HandleNDSRomInstall(rq api.NDSRomInstallRequest, w *Worker) api.Out {
	fileName := games.NDSFileName(rq.URL, rq.FileName, "game.nds")
	gameName := games.GameNameFromFile(fileName)
	relPath := filepath.Join("nds", fileName)
	fullPath := filepath.Join(w.conf.Library.BasePath, relPath)

	if isBuiltinNDSRom(rq.URL) {
		if _, err := stdos.Stat(fullPath); err != nil {
			c.log.Error().Err(err).Str("rom", fullPath).Msg("cannot find builtin NDS ROM")
			return api.ErrPacket
		}
	} else if err := downloadURLToFile(rq.URL, fullPath, maxNDSRomBytes); err != nil {
		c.log.Error().Err(err).Str("url", rq.URL).Msg("cannot install NDS ROM")
		return api.ErrPacket
	}

	w.lib.Scan()
	c.SendLibrary(w)
	c.SendPrevSessions(w)

	return api.Out{Payload: api.NDSRomInstallResponse{
		Game: gameName,
		Path: filepath.ToSlash(relPath),
	}}
}

func (c *coordinator) HandleNDSSessionPrepare(rq api.NDSSessionPrepareRequest, w *Worker) api.Out {
	if rq.RoomID == "" {
		return api.ErrPacket
	}
	if rq.SaveURL != "" {
		if err := w.installNDSSave(rq.RoomID, rq.SaveURL); err != nil {
			c.log.Error().Err(err).Str("room", rq.RoomID).Msg("cannot prepare NDS save")
			return api.ErrPacket
		}
		w.markPreparedSession(rq.RoomID)
	}
	return api.OkPacket
}

func (w *Worker) installNDSSave(roomID string, rawURL string) error {
	data, err := downloadURL(rawURL, maxNDSSaveBytes)
	if err != nil {
		return err
	}

	name := roomID + ".srm"
	if strings.HasSuffix(strings.ToLower(urlPathBase(rawURL)), zipc.Ext) {
		data, _, err = zipc.Read(data)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxNDSSaveBytes {
			return fmt.Errorf("save exceeds %d bytes", maxNDSSaveBytes)
		}
	}
	if w.conf.Emulator.Libretro.SaveCompression {
		data, err = zipc.Compress(data, name)
		if err != nil {
			return err
		}
	}

	if w.conf.Emulator.Libretro.SaveCompression {
		name += zipc.Ext
	}

	if err := stdos.MkdirAll(w.conf.Emulator.Storage, 0755); err != nil {
		return err
	}
	return stdos.WriteFile(filepath.Join(w.conf.Emulator.Storage, name), data, 0644)
}

func (w *Worker) markPreparedSession(roomID string) {
	w.prepared.mu.Lock()
	w.prepared.sessions[roomID] = struct{}{}
	w.prepared.mu.Unlock()
}

func (w *Worker) consumePreparedSession(roomID string) bool {
	w.prepared.mu.Lock()
	defer w.prepared.mu.Unlock()

	if _, ok := w.prepared.sessions[roomID]; !ok {
		return false
	}
	delete(w.prepared.sessions, roomID)
	return true
}

func downloadURLToFile(rawURL string, path string, limit int64) error {
	if err := stdos.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	resp, err := requestDownload(rawURL, limit)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp, err := stdos.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = stdos.Remove(tmpName) }()

	written, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if written > limit {
		return fmt.Errorf("download exceeds %d bytes", limit)
	}

	return stdos.Rename(tmpName, path)
}

func downloadURL(rawURL string, limit int64) ([]byte, error) {
	resp, err := requestDownload(rawURL, limit)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return data, nil
}

func requestDownload(rawURL string, limit int64) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}

	client := http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}
	if resp.ContentLength > limit {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download content-length exceeds %d bytes", limit)
	}
	return resp, nil
}

func isBuiltinNDSRom(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "builtin" || u.Scheme == "local"
}

func urlPathBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return filepath.Base(u.Path)
}
