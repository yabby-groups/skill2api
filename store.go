package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Lupino/go-periodic"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *statusStore) path(id string) string { return filepath.Join(s.root, id, "status.json") }
func (s *statusStore) deliveryDir(id string) string {
	return filepath.Join(s.root, id, ".skill2api-deliveries")
}
func (s *statusStore) deliveryPath(requestID, deliveryID string) string {
	return filepath.Join(s.deliveryDir(requestID), deliveryID+".json")
}
func (s *statusStore) deliveryKeyPath(requestID, fileKey string) string {
	return filepath.Join(s.deliveryDir(requestID), "key-"+fileKey+".json")
}
func (s *statusStore) read(id string) (taskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readUnlocked(id)
}
func (s *statusStore) readUnlocked(id string) (taskStatus, error) {
	var out taskStatus
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("invalid status file: %w", err)
	}
	return out, nil
}
func (s *statusStore) write(v taskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeUnlocked(v)
}
func (s *statusStore) writeUnlocked(v taskStatus) error {
	v.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	dir := filepath.Dir(s.path(v.RequestID))
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path(v.RequestID))
}

func readDeliveryFile(path string) (fileDeliveryStatus, error) {
	var out fileDeliveryStatus
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("invalid file delivery status: %w", err)
	}
	return out, nil
}

func writeDeliveryFile(path string, value fileDeliveryStatus) error {
	value.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".delivery-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *statusStore) startDelivery(value fileDeliveryStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyPath := s.deliveryKeyPath(value.RequestID, value.FileKey)
	if current, err := readDeliveryFile(keyPath); err == nil &&
		(current.Status == "queued" || current.Status == "running" || current.Status == "succeeded") {
		value.Status = current.Status
		value.SharedDeliveryID = current.DeliveryID
		value.File = current.File
		value.URL = current.URL
		value.Error = current.Error
		value.Attempts = current.Attempts
		return false, writeDeliveryFile(s.deliveryPath(value.RequestID, value.DeliveryID), value)
	}
	if err := writeDeliveryFile(s.deliveryPath(value.RequestID, value.DeliveryID), value); err != nil {
		return false, err
	}
	return true, writeDeliveryFile(keyPath, value)
}

func (s *statusStore) finishDelivery(value fileDeliveryStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeDeliveryFile(s.deliveryPath(value.RequestID, value.DeliveryID), value); err != nil {
		return err
	}
	return writeDeliveryFile(s.deliveryKeyPath(value.RequestID, value.FileKey), value)
}

func (s *statusStore) readDelivery(requestID, deliveryID string) (fileDeliveryStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := readDeliveryFile(s.deliveryPath(requestID, deliveryID))
	if err != nil || value.SharedDeliveryID == "" {
		return value, err
	}
	shared, err := readDeliveryFile(s.deliveryKeyPath(requestID, value.FileKey))
	if err != nil {
		return value, err
	}
	shared.DeliveryID = deliveryID
	shared.SharedDeliveryID = value.SharedDeliveryID
	shared.FilePath = value.FilePath
	return shared, nil
}

func (s *statusStore) create(v taskStatus, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.readUnlocked(v.RequestID); err == nil {
		if !force || existing.Status == "queued" || existing.Status == "running" || existing.Status == "interrupted" {
			return errors.New("request_id already exists")
		}
	}
	return s.writeUnlocked(v)
}

func (s *statusStore) claimWaiting(id string) (taskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.readUnlocked(id)
	if err != nil {
		return taskStatus{}, err
	}
	if v.Status != "waiting_for_input" {
		return v, errors.New("request is not waiting for input")
	}
	v.Status = "running"
	v.Question, v.Options, v.Phase, v.Error, v.Stdout, v.Stderr = "", nil, "", "", "", ""
	if err := s.writeUnlocked(v); err != nil {
		return taskStatus{}, err
	}
	return v, nil
}

func (s *statusStore) claimInterrupted(id string) (taskStatus, error) {
	return s.claimContinuation(id, "interrupted")
}

func (s *statusStore) claimSucceeded(id string) (taskStatus, error) {
	return s.claimContinuation(id, "succeeded")
}

func (s *statusStore) claimFailed(id string) (taskStatus, error) {
	return s.claimContinuation(id, "failed")
}

func (s *statusStore) claimTerminated(id string) (taskStatus, error) {
	return s.claimContinuation(id, "terminated")
}

func (s *statusStore) claimContinuation(id, expectedStatus string) (taskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.readUnlocked(id)
	if err != nil {
		return taskStatus{}, err
	}
	if v.Status != expectedStatus {
		return v, fmt.Errorf("request is not %s", expectedStatus)
	}
	if v.SessionID == "" {
		return v, errors.New("Codex session ID is missing")
	}
	v.Status = "running"
	v.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	v.FinishedAt, v.Error, v.Question, v.Options, v.Phase, v.Stdout, v.Stderr = "", "", "", nil, "", "", ""
	if err := s.writeUnlocked(v); err != nil {
		return taskStatus{}, err
	}
	return v, nil
}

func (s *statusStore) updateSessionID(id, startedAt, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.readUnlocked(id)
	if err != nil {
		return err
	}
	if v.Status != "running" || v.StartedAt != startedAt {
		return nil
	}
	v.SessionID = sessionID
	return s.writeUnlocked(v)
}

func (s *statusStore) claimQueued(id string) (taskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.readUnlocked(id)
	if err != nil {
		return taskStatus{}, err
	}
	if v.Status != "queued" {
		return v, errors.New("request is not queued")
	}
	v.Status = "running"
	v.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.writeUnlocked(v); err != nil {
		return taskStatus{}, err
	}
	return v, nil
}

// writeFromRunning applies an execution result only while this attempt owns the request.
func (s *statusStore) writeFromRunning(v taskStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readUnlocked(v.RequestID)
	if err != nil {
		return false, err
	}
	if current.Status != "running" || current.StartedAt != v.StartedAt {
		return false, nil
	}
	return true, s.writeUnlocked(v)
}

func (s *statusStore) terminate(id string) (taskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.readUnlocked(id)
	if err != nil {
		return taskStatus{}, err
	}
	if v.Status == "succeeded" || v.Status == "failed" || v.Status == "terminated" {
		return v, nil
	}
	v.Status = "terminated"
	v.Error = "terminated by user"
	v.Question, v.Options, v.Phase = "", nil, ""
	v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.writeUnlocked(v); err != nil {
		return taskStatus{}, err
	}
	return v, nil
}

func terminalStatus(status string) bool {
	return status == "succeeded" || status == "failed" || status == "terminated"
}

type cleanupSubmitter func(string, string, map[string]interface{}) error

func submitCleanupAt(submit cleanupSubmitter, c config, requestID string, startedAt time.Time) error {
	return submit(nodeFunctionName(c, cleanupFunc), requestID, map[string]interface{}{
		"schedat": startedAt.Add(retentionAge).Unix(),
	})
}

func scheduleCleanup(c config, requestID string) {
	client := periodic.NewClient()
	if err := connectPeriodic(client, c.PeriodicAddr, c.RSA); err != nil {
		log.Printf("event=skill2api_cleanup request_id=%s result=schedule_connect_failed error=%q", requestID, err)
		return
	}
	defer client.Close()
	if err := submitCleanupAt(client.SubmitJob, c, requestID, time.Now().UTC()); err != nil {
		log.Printf("event=skill2api_cleanup request_id=%s result=schedule_failed error=%q", requestID, err)
		return
	}
	log.Printf("event=skill2api_cleanup request_id=%s result=scheduled", requestID)
}

// removeTaskData keeps statusPath until every other task artifact is gone, so a
// failed cleanup can be retried with the same request ID.
func removeTaskData(taskDir, statusPath string, removeAll func(string) error) error {
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(taskDir, entry.Name())
		if filepath.Clean(path) == filepath.Clean(statusPath) {
			continue
		}
		if err := removeAll(path); err != nil {
			return err
		}
	}
	if err := os.Remove(statusPath); err != nil {
		return err
	}
	return os.Remove(taskDir)
}

func (s *statusStore) cleanup(c config, id string, now time.Time) cleanupResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := cleanupResponse{RequestID: id}
	v, err := s.readUnlocked(id)
	if errors.Is(err, os.ErrNotExist) {
		result.Status, result.Error = "not_found", "request not found"
		return result
	}
	if err != nil || v.RequestID != id {
		result.Status, result.Error = "failed", "invalid task status"
		return result
	}
	if !terminalStatus(v.Status) {
		result.Status, result.Error = "deferred", "request is not terminal"
		return result
	}
	finishedAt, err := time.Parse(time.RFC3339Nano, v.FinishedAt)
	if err != nil {
		result.Status, result.Error = "not_due", "request has no valid completion time"
		return result
	}
	if finishedAt.Add(retentionAge).After(now) {
		result.Status, result.Error = "not_due", "retention period has not elapsed"
		result.retryAt = finishedAt.Add(retentionAge)
		return result
	}
	if filepath.Clean(v.OutputDir) != filepath.Join(s.root, id) {
		result.Status, result.Error = "skipped", "request output directory is not dedicated"
		return result
	}
	if err := os.RemoveAll(filepath.Dir(codexHomeDir(c, id))); err != nil {
		log.Printf("event=skill2api_cleanup request_id=%s result=session_remove_failed error=%q", id, err)
		result.Status, result.Error = "failed", "remove private Codex session"
		return result
	}
	if err := removeTaskData(filepath.Join(s.root, id), s.path(id), os.RemoveAll); err != nil {
		log.Printf("event=skill2api_cleanup request_id=%s result=data_remove_failed error=%q", id, err)
		result.Status, result.Error = "failed", "remove task data"
		return result
	}
	log.Printf("event=skill2api_cleanup request_id=%s result=deleted", id)
	result.Status = "deleted"
	return result
}

func cleanupRescheduleDelay(result cleanupResponse, now time.Time) (int, bool) {
	if result.Status == "deferred" {
		return int(retentionAge.Seconds()), true
	}
	if result.Status != "not_due" || result.retryAt.IsZero() {
		return 0, false
	}
	delay := result.retryAt.Sub(now)
	if delay <= 0 {
		return 1, true
	}
	return int((delay + time.Second - 1) / time.Second), true
}
