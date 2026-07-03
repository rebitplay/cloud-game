package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (h *Hub) recycleNDSGroup(groupID string) {
	if groupID == "" {
		return
	}

	managed := h.ndsSpawner != nil && h.ndsSpawner.ownsGroup(groupID)
	if managed {
		h.ndsSpawner.killGroup(groupID)
	}

	for _, worker := range h.ndsWorkersForGroup(groupID) {
		h.workers.RemoveDisconnect(worker)
	}

	runtimeDir := envString("RUNTIME_DIR", "/tmp/cloud-game/nds-lan")
	if err := wipeNDSGroupRuntime(runtimeDir, groupID); err != nil {
		h.log.Warn().Err(err).Str("group", groupID).Msg("NDS worker group runtime wipe failed")
	}

	if managed && !h.draining.Load() {
		if err := h.ndsSpawner.startGroup(h, groupID, h.ndsSpawner.playersPerGroup); err != nil {
			h.log.Warn().Err(err).Str("group", groupID).Msg("NDS worker group respawn failed")
		}
	}
}

func (h *Hub) ndsWorkersForGroup(groupID string) []*Worker {
	workers := make([]*Worker, 0, 4)
	for worker := range h.workers.Values() {
		if effectiveNDSGroup(worker) == groupID {
			workers = append(workers, worker)
		}
	}
	return workers
}

func effectiveNDSGroup(worker *Worker) string {
	if worker == nil {
		return ""
	}
	if worker.NDSGroup != "" {
		return worker.NDSGroup
	}
	return "default"
}

func wipeNDSGroupRuntime(runtimeDir string, groupID string) error {
	runtimeDir = strings.TrimSpace(runtimeDir)
	groupID = strings.TrimSpace(groupID)
	if runtimeDir == "" || groupID == "" {
		return nil
	}
	root, err := filepath.Abs(runtimeDir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join(root, groupID))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to wipe runtime path outside root: %s", target)
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.MkdirAll(root, 0700)
}
