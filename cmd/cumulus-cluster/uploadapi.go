package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/willove/cumulus/internal/bucket"
)

const (
	uploadFileLimit  = 16 << 20
	uploadBatchLimit = 64 << 20
	uploadBodyLimit  = 65 << 20
	uploadFileCount  = 500
)

func registerUploadFace(mux *http.ServeMux, ensure *nsEnsurer, buckets *bucket.Store) {
	mux.HandleFunc("/v1/ingest/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		namespace := r.URL.Query().Get("ns")
		// Gate before reading even one byte of the multipart body or staging it.
		if !requireHTTPBucket(w, r, buckets, namespace) {
			return
		}
		if r.ContentLength > uploadBodyLimit {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "multipart body exceeds 65 MiB"})
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			writeUploadError(w, fmt.Errorf("multipart/form-data required"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, uploadBodyLimit)
		reader, err := r.MultipartReader()
		if err != nil {
			writeUploadError(w, fmt.Errorf("multipart/form-data required: %w", err))
			return
		}
		root, err := os.MkdirTemp("", "cumulus-upload-")
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		owned := true
		defer func() {
			if owned {
				_ = os.RemoveAll(root)
			}
		}()
		files, err := stageUpload(r, reader, root)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		st, err := ensure.store(r.Context(), namespace)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			writeStoreErr(w, err)
			return
		}
		job := "upload-" + hex.EncodeToString(nonce[:])
		err = queueFileJob(r.Context(), st, job, len(files), func(ctx context.Context) (int, error) {
			c, err := st.IngestUploaded(ctx, root, files, job)
			return c.Stored(), err
		}, func() { _ = os.RemoveAll(root) })
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		owned = false
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "state": "queued", "total": len(files)})
	})
}

type uploadLimitError string

func (e uploadLimitError) Error() string { return string(e) }

func writeUploadError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	var limit uploadLimitError
	var bodyLimit *http.MaxBytesError
	if errors.As(err, &limit) || errors.As(err, &bodyLimit) || errors.Is(err, multipart.ErrMessageTooLarge) {
		code = http.StatusRequestEntityTooLarge
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// Cleaning unsafe names would alias distinct document identities.
func uploadPath(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:") ||
		path.IsAbs(name) || filepath.IsAbs(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid relative filename %q", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") {
			return fmt.Errorf("hidden, empty, or traversal path segment in %q", name)
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".html", ".htm", ".pdf", ".docx":
		return nil
	default:
		return fmt.Errorf("unsupported file type in %q (allowed: md, txt, html, htm, pdf, docx)", name)
	}
}

// Stage the entire batch before ingesting; Part.FileName would flatten relative paths.
func stageUpload(r *http.Request, reader *multipart.Reader, root string) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	var total int64
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("malformed multipart body: %w", err)
		}
		disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil || disposition != "form-data" || params["name"] != "files" {
			return nil, fmt.Errorf("each multipart part must be a files form-data file")
		}
		name := params["filename"]
		if err := uploadPath(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate relative filename %q", name)
		}
		seen[name] = true
		if len(files) >= uploadFileCount {
			return nil, uploadLimitError("upload exceeds 500 files")
		}
		dest := filepath.Join(root, filepath.FromSlash(name))
		rel, err := filepath.Rel(root, dest)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("filename escapes upload directory: %q", name)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return nil, fmt.Errorf("cannot stage %q: %w", name, err)
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, fmt.Errorf("cannot stage %q: %w", name, err)
		}
		limit := int64(uploadFileLimit)
		if remaining := int64(uploadBatchLimit) - total; remaining < limit {
			limit = remaining
		}
		n, copyErr := io.Copy(f, io.LimitReader(part, limit+1))
		closeErr := f.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("reading %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n > uploadFileLimit {
			return nil, uploadLimitError(fmt.Sprintf("file %q exceeds 16 MiB", name))
		}
		total += n
		if total > uploadBatchLimit {
			return nil, uploadLimitError("uploaded files exceed 64 MiB total")
		}
		if n == 0 {
			return nil, fmt.Errorf("empty file %q", name)
		}
		if err := part.Close(); err != nil {
			return nil, fmt.Errorf("closing %q: %w", name, err)
		}
		files = append(files, dest)
	}
	// Enforce the body cap on bytes after the multipart closing boundary too.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, fmt.Errorf("reading multipart body: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("at least one nonempty file is required")
	}
	return files, nil
}
