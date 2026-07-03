package worker

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	stdos "os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/games"
	zipc "github.com/giongto35/cloud-game/v3/pkg/worker/compression/zip"
)

const (
	maxNDSRomBytes             = 512 << 20
	maxNDSSaveBytes            = 32 << 20
	defaultNDSROMCacheMaxBytes = 20 << 30
)

type preparedNDSSession struct {
	Ref           string
	SaveURL       string
	SaveUploadURL string
}

func (c *coordinator) HandleNDSRomInstall(rq api.NDSRomInstallRequest, w *Worker) api.Out {
	fileName := games.NDSFileName(rq.URL, rq.FileName, "game.nds")
	gameName := games.GameNameFromFile(fileName)
	sha1Hex := strings.ToLower(strings.TrimSpace(rq.SHA1))
	if sha1Hex == "" {
		c.log.Error().Str("rom", fileName).Msg("NDS ROM SHA1 is required")
		return api.ErrPacket
	}
	relPath := filepath.Join("nds", sha1Hex+".nds")
	fullPath := filepath.Join(w.conf.Library.BasePath, relPath)

	if err := installNDSROM(rq.URL, fullPath, fileName, sha1Hex); err != nil {
		c.log.Error().Err(err).Str("url", rq.URL).Msg("cannot install NDS ROM")
		return api.ErrPacket
	}
	if err := evictNDSROMCache(filepath.Dir(fullPath), ndsROMCacheMaxBytes(), fullPath); err != nil {
		c.log.Warn().Err(err).Msg("cannot evict NDS ROM cache")
	}

	w.lib.Scan()
	c.SendLibrary(w)
	c.SendPrevSessions(w)

	return api.Out{Payload: api.NDSRomInstallResponse{
		Game: gameName,
		Path: filepath.ToSlash(relPath),
	}}
}

func installNDSROM(rawURL string, path string, fileName string, wantSHA1 string) error {
	if validCachedNDSROM(path, wantSHA1) {
		now := time.Now()
		_ = stdos.Chtimes(path, now, now)
		return nil
	}

	var data []byte
	var err error
	if isBuiltinNDSRom(rawURL) {
		u, _ := url.Parse(rawURL)
		name := games.NDSFileName(rawURL, fileName, "game.nds")
		if u != nil && u.Path != "" {
			name = filepath.Base(u.Path)
		}
		data, err = stdos.ReadFile(filepath.Join(filepath.Dir(path), name))
	} else {
		data, err = downloadURL(rawURL, maxNDSRomBytes)
	}
	if err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(urlPathBase(rawURL)), zipc.Ext) || strings.HasSuffix(strings.ToLower(fileName), zipc.Ext) {
		data, _, err = zipc.Read(data)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxNDSRomBytes {
			return fmt.Errorf("rom exceeds %d bytes", maxNDSRomBytes)
		}
	}
	if got := sha1String(data); got != strings.ToLower(wantSHA1) {
		return fmt.Errorf("rom SHA1 mismatch: got %s want %s", got, wantSHA1)
	}
	return writeFileAtomic(path, data, 0644)
}

func validCachedNDSROM(path string, wantSHA1 string) bool {
	data, err := stdos.ReadFile(path)
	if err != nil {
		return false
	}
	return sha1String(data) == strings.ToLower(wantSHA1)
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
	}
	w.markPreparedSession(rq.RoomID, preparedNDSSession{
		Ref:           rq.Ref,
		SaveURL:       rq.SaveURL,
		SaveUploadURL: rq.SaveUploadURL,
	})
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

func (w *Worker) markPreparedSession(roomID string, session preparedNDSSession) {
	w.prepared.mu.Lock()
	w.prepared.sessions[roomID] = session
	w.prepared.mu.Unlock()
}

func (w *Worker) consumePreparedSession(roomID string) (preparedNDSSession, bool) {
	w.prepared.mu.Lock()
	defer w.prepared.mu.Unlock()

	session, ok := w.prepared.sessions[roomID]
	if !ok {
		return preparedNDSSession{}, false
	}
	delete(w.prepared.sessions, roomID)
	return session, true
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
	if err := validateNDSRemoteURL(rawURL); err != nil {
		return nil, err
	}

	client := http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return validateNDSRemoteURL(req.URL.String())
		},
	}
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

func validateNDSRemoteURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("missing URL host")
	}
	if !ndsAllowedDownloadHost(host) {
		return fmt.Errorf("download host %q is not allowed", host)
	}
	ips, err := ndsLookupIP(context.Background(), "ip", host)
	if err != nil {
		return fmt.Errorf("resolve download host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("download host %q resolved no IPs", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("download host %q resolved non-public IP %s", host, ip)
		}
	}
	return nil
}

var ndsLookupIP = net.DefaultResolver.LookupIP

func ndsAllowedDownloadHost(host string) bool {
	raw := strings.TrimSpace(stdos.Getenv("NDS_DOWNLOAD_ALLOWED_HOSTS"))
	if raw == "" {
		return false
	}
	for _, pattern := range strings.Split(raw, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !ip.IsUnspecified() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast()
}

func writeFileAtomic(path string, data []byte, perm stdos.FileMode) error {
	_, _, err := writeReaderAtomic(path, bytes.NewReader(data), perm)
	return err
}

func writeReaderAtomic(path string, r io.Reader, perm stdos.FileMode) (string, int64, error) {
	if err := stdos.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", 0, err
	}
	tmp, err := stdos.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = stdos.Remove(tmpName) }()

	written, err := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if err != nil {
		return tmpName, written, err
	}
	if closeErr != nil {
		return tmpName, written, closeErr
	}
	if err := stdos.Chmod(tmpName, perm); err != nil {
		return tmpName, written, err
	}
	if err := stdos.Rename(tmpName, path); err != nil {
		return tmpName, written, err
	}
	return tmpName, written, nil
}

func sha1String(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func ndsROMCacheMaxBytes() int64 {
	raw := strings.TrimSpace(stdos.Getenv("NDS_ROM_CACHE_MAX_BYTES"))
	if raw == "" {
		return defaultNDSROMCacheMaxBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < maxNDSRomBytes {
		return defaultNDSROMCacheMaxBytes
	}
	return n
}

func evictNDSROMCache(dir string, maxBytes int64, keepPath string) error {
	entries, err := stdos.ReadDir(dir)
	if err != nil {
		return err
	}
	type cachedFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []cachedFile
	total := int64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".nds") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		total += info.Size()
		files = append(files, cachedFile{path: path, size: info.Size(), modTime: info.ModTime()})
	}
	if total <= maxBytes {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if filepath.Clean(file.path) == filepath.Clean(keepPath) {
			continue
		}
		if err := stdos.Remove(file.path); err == nil {
			total -= file.size
		}
	}
	return nil
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
