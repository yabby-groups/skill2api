package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func listFiles(dir string) ([]string, error) {
	type fileEntry struct {
		path    string
		modTime time.Time
	}
	var entries []fileEntry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".skill2api-deliveries" {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			rel, e := filepath.Rel(dir, path)
			if e != nil {
				return e
			}
			entries = append(entries, fileEntry{
				path:    filepath.ToSlash(rel),
				modTime: info.ModTime(),
			})
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modTime.Before(entries[j].modTime)
	})
	files := make([]string, len(entries))
	for i, entry := range entries {
		files[i] = entry.path
	}
	return files, err
}

// captureExistingFiles preserves artifacts produced before an interrupted run.
func captureExistingFiles(v *taskStatus) {
	files, err := listFiles(v.OutputDir)
	if err != nil {
		log.Printf("event=skill2api_files request_id=%s result=list_failed error=%q", v.RequestID, err)
		return
	}
	v.Files = files
}

func readTaskFile(outputDir, rawPath string, maxBytes int) ([]byte, error) {
	filePath := strings.TrimSpace(rawPath)
	if filePath == "" || filepath.IsAbs(filePath) {
		return nil, errors.New("file_path must be a non-empty relative path")
	}
	clean := filepath.Clean(filePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("file_path must stay inside the task output directory")
	}
	if maxBytes < 1 {
		return nil, errors.New("maximum file size must be positive")
	}
	base, err := filepath.EvalSymlinks(outputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory: %w", err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(outputDir, clean))
	if err != nil {
		return nil, fmt.Errorf("resolve file: %w", err)
	}
	if !within(base, target) {
		return nil, errors.New("file_path must stay inside the task output directory")
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file_path must reference a regular file")
	}
	if info.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("file exceeds maximum size of %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("file exceeds maximum size of %d bytes", maxBytes)
	}
	return data, nil
}

type temporaryUploadResponse struct {
	File json.RawMessage `json:"file"`
	Err  string          `json:"err"`
}

type uploadedFile struct {
	FileKey string `json:"file_key"`
	FileExt string `json:"file_ext"`
}

type uploadRequestError struct {
	err       error
	retryable bool
}

func (e *uploadRequestError) Error() string { return e.err.Error() }
func (e *uploadRequestError) Unwrap() error { return e.err }

func fileKeyForData(data []byte) string {
	sum := sha256.Sum256(data)
	return strings.ReplaceAll(base64.RawURLEncoding.EncodeToString(sum[:]), "-", "")
}

func isRetryableUploadError(err error) bool {
	var requestErr *uploadRequestError
	if errors.As(err, &requestErr) {
		return requestErr.retryable
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func resolveTemporaryFile(ctx context.Context, c config, apiKey, fileKey string) (json.RawMessage, string, bool, error) {
	body, err := json.Marshal(map[string]string{"file_key": fileKey})
	if err != nil {
		return nil, "", false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UploadBaseURL+"/api/file/temporary/resolve/", bytes.NewReader(body))
	if err != nil {
		return nil, "", false, fmt.Errorf("create temporary file resolve request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "", false, &uploadRequestError{err: fmt.Errorf("resolve temporary file: %w", err), retryable: true}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, int64(maxOutput)+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("read temporary file resolve response: %w", err)
	}
	if len(responseBody) > maxOutput {
		return nil, "", false, errors.New("temporary file resolve response exceeds maximum size")
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, "", false, &uploadRequestError{
			err:       fmt.Errorf("resolve temporary file returned HTTP %d", response.StatusCode),
			retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError,
		}
	}
	var payload struct {
		File json.RawMessage `json:"file"`
		URL  string          `json:"url"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return nil, "", false, fmt.Errorf("decode temporary file resolve response: %w", err)
	}
	var file uploadedFile
	if len(payload.File) == 0 || json.Unmarshal(payload.File, &file) != nil || file.FileKey != fileKey || len(file.FileKey) < 4 || strings.TrimSpace(file.FileExt) == "" || strings.TrimSpace(payload.URL) == "" {
		return nil, "", false, errors.New("temporary file resolve response did not contain the requested file")
	}
	return payload.File, payload.URL, true, nil
}

func uploadTemporaryFile(ctx context.Context, c config, req fileRequest, data []byte) (json.RawMessage, string, error) {
	if err := validateEnvironment(req.Environment); err != nil {
		return nil, "", err
	}
	apiKey := strings.TrimSpace(req.Environment["SANDBOX_AI_KEY"])
	if apiKey == "" {
		return nil, "", errors.New("SANDBOX_AI_KEY is required for file upload")
	}
	fileKey := fileKeyForData(data)
	if file, uploadURL, found, err := resolveTemporaryFile(ctx, c, apiKey, fileKey); err != nil {
		return nil, "", err
	} else if found {
		return file, uploadURL, nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(req.FilePath))
	if err != nil {
		return nil, "", fmt.Errorf("create upload file part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", fmt.Errorf("write upload file part: %w", err)
	}
	if err := writer.WriteField("temporary", "true"); err != nil {
		return nil, "", fmt.Errorf("write temporary field: %w", err)
	}
	if err := writer.WriteField("skill2api", "true"); err != nil {
		return nil, "", fmt.Errorf("write skill2api field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close upload body: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UploadBaseURL+"/api/file/run/", &body)
	if err != nil {
		return nil, "", fmt.Errorf("create upload request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "", &uploadRequestError{err: fmt.Errorf("upload temporary file: %w", err), retryable: true}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, int64(maxOutput)+1))
	if err != nil {
		return nil, "", fmt.Errorf("read upload response: %w", err)
	}
	if len(responseBody) > maxOutput {
		return nil, "", errors.New("upload response exceeds maximum size")
	}
	var payload temporaryUploadResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return nil, "", fmt.Errorf("decode upload response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if payload.Err != "" {
			return nil, "", &uploadRequestError{
				err:       fmt.Errorf("upload temporary file: %s", payload.Err),
				retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError,
			}
		}
		return nil, "", &uploadRequestError{
			err:       fmt.Errorf("upload temporary file returned HTTP %d", response.StatusCode),
			retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError,
		}
	}
	var uploaded uploadedFile
	if len(payload.File) == 0 || json.Unmarshal(payload.File, &uploaded) != nil || len(uploaded.FileKey) < 4 || strings.TrimSpace(uploaded.FileExt) == "" {
		return nil, "", errors.New("upload response did not contain a valid file")
	}
	if uploaded.FileKey != fileKeyForData(data) {
		return nil, "", errors.New("upload response file_key did not match file content")
	}
	fileKey = url.PathEscape(uploaded.FileKey)
	fileExt := url.PathEscape(strings.TrimPrefix(uploaded.FileExt, "."))
	path := fmt.Sprintf("/upload/%s/%s/%s.%s", fileKey[:2], fileKey[2:4], fileKey, fileExt)
	return payload.File, path, nil
}

func uploadTemporaryFileWithRetry(c config, req fileRequest, data []byte) (json.RawMessage, string, int, error) {
	uploadTimeout := c.UploadTimeout
	if uploadTimeout <= 0 {
		uploadTimeout = fileUploadTimeout
	}
	var lastErr error
	for attempt := 1; attempt <= fileUploadAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
		file, uploadURL, err := uploadTemporaryFile(ctx, c, req, data)
		cancel()
		if err == nil {
			return file, uploadURL, attempt, nil
		}
		lastErr = err
		if !isRetryableUploadError(err) || attempt == fileUploadAttempts {
			break
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return nil, "", fileUploadAttempts, lastErr
}
