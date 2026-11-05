package cloudfiles

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luskaner/ageServer/common/game"
	"github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/logger"
	"github.com/luskaner/ageServer/server/internal/models"
)

func generateRequestId() string {
	var u [16]byte
	internal.WithRng(func(rand *internal.RandReader) {
		for i := range 10 {
			u[i] = byte(rand.UintN(256))
		}
	})
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	copy(u[10:], []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4],
		u[4:6],
		u[6:8],
		u[8:10],
		u[10:16],
	)
}

func md5Base64(hexSum string) string {
	raw, err := hex.DecodeString(hexSum)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// handleMatchReplay emulates the blob storage for the match replays AoE IV
// uploads after a match. The blobs are stored on this server's disk only
// (models.MatchReplays); every request needs a token the server issued for that
// exact blob, and uploads need a write token.
func handleMatchReplay(w http.ResponseWriter, r *http.Request, key string) {
	g := models.G(r)
	store := g.MatchReplays()
	matchID, kind, ok := models.ParseMatchReplayKey(key)
	if !ok || store == nil || !store.Enabled() {
		http.Error(w, "The specified blob does not exist.", http.StatusNotFound)
		return
	}
	query := r.URL.Query()
	if query.Get("comp") != "" {
		// Block uploads (Put Block / Put Block List) are not emulated: storing a
		// single block as the whole replay would corrupt it.
		http.Error(w, "Block uploads are not supported.", http.StatusNotImplemented)
		return
	}
	write := r.Method == http.MethodPut
	if !store.Authorized(query.Get("sig"), matchID, kind, write) {
		http.Error(w, "Server failed to authenticate the request.", http.StatusForbidden)
		return
	}

	w.Header().Set("Server", "Windows-Azure-Blob/1.0 Microsoft-HTTPAPI/2.0")
	w.Header().Set("x-ms-request-id", generateRequestId())
	w.Header().Set("x-ms-version", models.AzureStorageVersion)
	switch r.Method {
	case http.MethodPut:
		file, stored, err := store.Put(matchID, kind, http.MaxBytesReader(w, r.Body, store.MaxFileSize()))
		if err != nil {
			var tooLarge *http.MaxBytesError
			switch {
			case errors.Is(err, models.ErrFileTooLarge), errors.As(err, &tooLarge):
				http.Error(w, "The request body is too large.", http.StatusRequestEntityTooLarge)
			case errors.Is(err, models.ErrEmptyReplay):
				http.Error(w, "The request body is empty.", http.StatusBadRequest)
			default:
				logger.Printf("cloudfiles: storing replay %s failed: %s\n", key, err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}
		if stored {
			logger.Printf("cloudfiles: stored match replay %s (%d bytes)\n", key, file.Length)
		} else {
			logger.Printf("cloudfiles: match replay %s already stored, upload discarded\n", key)
		}
		w.Header().Set("Content-MD5", md5Base64(file.MD5))
		w.Header().Set("Last-Modified", file.ModTime.UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", fmt.Sprintf(`"0x%X"`, file.ModTime.UnixNano()))
		w.Header().Set("x-ms-request-server-encrypted", "true")
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet, http.MethodHead:
		file, f, ok := store.Open(matchID, kind)
		if !ok {
			http.Error(w, "The specified blob does not exist.", http.StatusNotFound)
			return
		}
		defer func() { _ = f.Close() }()
		created := file.ModTime.UTC().Format(http.TimeFormat)
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Header.Get("Range") == "" {
			// The MD5 describes the whole blob, never a range of it.
			w.Header().Set("Content-MD5", md5Base64(file.MD5))
		}
		w.Header().Set("ETag", fmt.Sprintf(`"0x%X"`, file.ModTime.UnixNano()))
		w.Header().Set("x-ms-meta-filename", kind.Filename())
		w.Header().Set("x-ms-meta-ContentLength", strconv.FormatInt(file.Length, 10))
		w.Header().Set("x-ms-creation-time", created)
		w.Header().Set("x-ms-lease-status", "unlocked")
		w.Header().Set("x-ms-lease-state", "available")
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.Header().Set("x-ms-server-encrypted", "true")
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		http.ServeContent(w, r, kind.Filename(), file.ModTime, f)

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func Cloudfiles(w http.ResponseWriter, r *http.Request) {
	key := strings.Join(strings.Split(r.URL.Path, "/")[2:], "/")
	if models.IsMatchReplayKey(key) {
		handleMatchReplay(w, r, key)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cloudfiles := models.G(r).Resources().CloudFiles()
	if cloudfiles.Credentials == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	info, exists := (*cloudfiles.Credentials).Get(r.URL.Query().Get("sig"))

	if !exists {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	filename, file, ok := cloudfiles.GetByKey(key)
	if ok {
		if file.Key != *info.Data() {
			http.Error(w, "Incorrect signature", http.StatusForbidden)
			return
		}
		data, err := cloudfiles.ReadFile(filename)
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		lengthStr := strconv.Itoa(file.Length)
		w.Header().Set("Content-Length", lengthStr)
		w.Header().Set("Content-Type", file.Type)
		w.Header().Set("Content-MD5", file.Checksum)
		w.Header().Set("Last-Modified", file.Created)
		w.Header().Set("Accept-Range", "bytes")
		w.Header().Set("ETag", file.ETag)
		w.Header().Set("Server", "Windows-Azure-Blob/1.0 Microsoft-HTTPAPI/2.0")
		w.Header().Set("x-ms-request-id", generateRequestId())
		w.Header().Set("x-ms-version", file.Version)
		// TODO: Review
		if models.G(r).Title() != game.AoE3 && models.G(r).Title() != game.AoM {
			w.Header().Set("x-ms-meta-filename", filename)
			w.Header().Set("x-ms-meta-ContentLength", lengthStr)
		}
		w.Header().Set("x-ms-creation-time", file.Created)
		w.Header().Set("x-ms-lease-status", "unlocked")
		w.Header().Set("x-ms-lease-state", "available")
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.Header().Set("x-ms-server-encrypted", "true")
		w.Header().Set("Date", time.Now().Format(time.RFC1123))
		w.Header().Set("Content-Type", file.Type)
		// Serve the indexed payload through the standard file-serving helper:
		// the bytes come from disk, so they must not be treated as a verbatim
		// copy of the request into the response.
		http.ServeContent(w, r, filename, time.Time{}, bytes.NewReader(data))
		return
	}

	http.Error(w, "Not Found", http.StatusNotFound)
}
