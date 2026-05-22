package server

// Public /dist/* binary serving. Operators drop agentctl-linux-amd64 +
// agentctl-darwin-arm64 into AGENT_HUB_DIST_DIR; fresh VMs `curl` these
// during /join before they have any credentials. Outside auth middleware
// by design.
//
// Returns:
//   200 with octet-stream + ETag (mtime+size) on success
//   404 if the requested binary isn't present
//   503 if AGENT_HUB_DIST_DIR is unset (operator hasn't configured it)

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// handleDistAgentctl returns a handler closed over a fixed filename — one
// per supported platform binary. Filename is part of the URL path, not a
// path parameter, so there's no traversal risk.
func (a *App) handleDistAgentctl(filename string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.DistDir == "" {
			writeError(w, http.StatusServiceUnavailable, "dist_disabled",
				"AGENT_HUB_DIST_DIR is not configured")
			return
		}

		full := filepath.Join(a.DistDir, filename)
		info, err := os.Stat(full)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "binary_not_found",
					fmt.Sprintf("%s not present in dist dir", filename))
				return
			}
			writeError(w, http.StatusInternalServerError, "stat_failed", err.Error())
			return
		}
		if info.IsDir() {
			writeError(w, http.StatusInternalServerError, "not_a_file",
				"dist target is a directory")
			return
		}

		f, err := os.Open(full)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "open_failed", err.Error())
			return
		}
		defer f.Close()

		// ETag: weak validator derived from mtime + size. Cheap, no full
		// content hash; operators redeploy binaries rarely so this is fine.
		etag := fmt.Sprintf("\"%d-%d\"", info.ModTime().UnixNano(), info.Size())
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=agentctl")

		// http.ServeContent handles If-None-Match against the ETag for free
		// and sets Content-Length correctly.
		http.ServeContent(w, r, filename, info.ModTime(), f)
	}
}
