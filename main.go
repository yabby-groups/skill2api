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
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lupino/go-periodic"
	"github.com/Lupino/go-periodic/protocol"
)

const (
	generateFunc           = "skill2api_generate"
	statusFunc             = "skill2api_status"
	fileFunc               = "skill2api_file"
	fileDeliveryFunc       = "skill2api_file_delivery"
	fileDeliveryStatusFunc = "skill2api_file_delivery_status"
	resumeFunc             = "skill2api_resume"
	terminateFunc          = "skill2api_terminate"
	cleanupFunc            = "skill2api_cleanup"
	maxOutput              = 64 * 1024
	maxFileBytes           = 64 * 1024 * 1024
	maxSkillName           = 128
	retentionAge           = 24 * time.Hour
	stdoutLogName          = "stdout.log"
	stderrLogName          = "stderr.log"
	maxInputTailBytes      = 16 * 1024
	fileUploadTimeout      = 45 * time.Second
	fileUploadAttempts     = 3
	dockerCodexConfig      = `sandbox_mode = "danger-full-access"
model_provider = "sandbox_runner"
model = "gpt-5.6-luna"

[model_providers.sandbox_runner]
name = "Sandbox Runner"
base_url = "https://huabot.com/v1"
wire_api = "responses"
env_key = "SANDBOX_AI_KEY"
supports_websockets = false

[projects."/workspace"]
trust_level = "trusted"
`
)

type generateRequest struct {
	RequestID   string            `json:"request_id"`
	SkillName   string            `json:"skill_name"`
	OutputDir   string            `json:"output_dir"`
	Prompt      string            `json:"prompt"`
	Environment map[string]string `json:"environment,omitempty"`
	Model       string            `json:"model,omitempty"`
	Force       bool              `json:"force"`
}

type statusRequest struct {
	RequestID string `json:"request_id"`
}

type fileRequest struct {
	RequestID   string            `json:"request_id"`
	FilePath    string            `json:"file_path"`
	Environment map[string]string `json:"environment"`
}

type fileDeliveryRequest struct {
	RequestID   string            `json:"request_id"`
	DeliveryID  string            `json:"delivery_id"`
	FilePath    string            `json:"file_path"`
	Environment map[string]string `json:"environment"`
}

type fileDeliveryStatusRequest struct {
	RequestID  string `json:"request_id"`
	DeliveryID string `json:"delivery_id"`
}

type fileDeliveryStatus struct {
	RequestID        string          `json:"request_id"`
	DeliveryID       string          `json:"delivery_id"`
	SharedDeliveryID string          `json:"shared_delivery_id,omitempty"`
	FilePath         string          `json:"file_path"`
	FileKey          string          `json:"file_key,omitempty"`
	Status           string          `json:"status"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	FinishedAt       string          `json:"finished_at,omitempty"`
	Attempts         int             `json:"attempts"`
	File             json.RawMessage `json:"file,omitempty"`
	URL              string          `json:"url,omitempty"`
	Error            string          `json:"error,omitempty"`
}

type resumeRequest struct {
	RequestID   string            `json:"request_id"`
	Answer      string            `json:"answer"`
	Instruction string            `json:"instruction"`
	Environment map[string]string `json:"environment,omitempty"`
}

type terminateRequest struct {
	RequestID string `json:"request_id"`
}

type cleanupResponse struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type taskStatus struct {
	RequestID  string   `json:"request_id"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	UpdatedAt  string   `json:"updated_at"`
	SkillName  string   `json:"skill_name"`
	Model      string   `json:"model,omitempty"`
	OutputDir  string   `json:"output_dir"`
	Files      []string `json:"files"`
	Error      string   `json:"error"`
	Stdout     string   `json:"stdout,omitempty"`
	Stderr     string   `json:"stderr,omitempty"`
	Phase      string   `json:"phase,omitempty"`
	Question   string   `json:"question,omitempty"`
	Options    []string `json:"options,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
}

type statusResponse struct {
	RequestID  string   `json:"request_id"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	UpdatedAt  string   `json:"updated_at"`
	SkillName  string   `json:"skill_name"`
	Model      string   `json:"model,omitempty"`
	OutputDir  string   `json:"output_dir"`
	Files      []string `json:"files"`
	Error      string   `json:"error"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Phase      string   `json:"phase,omitempty"`
	Question   string   `json:"question,omitempty"`
	Options    []string `json:"options,omitempty"`
}

func publicStatus(v taskStatus) statusResponse {
	return statusResponse{
		RequestID: v.RequestID, Status: v.Status, CreatedAt: v.CreatedAt,
		StartedAt: v.StartedAt, FinishedAt: v.FinishedAt, SkillName: v.SkillName, Model: v.Model,
		UpdatedAt: v.UpdatedAt,
		OutputDir: v.OutputDir, Files: v.Files, Error: v.Error, Stdout: v.Stdout,
		Stderr: v.Stderr, Phase: v.Phase, Question: v.Question, Options: v.Options,
	}
}

func statusWithLogOutput(v taskStatus, limit int) (statusResponse, error) {
	response := publicStatus(v)
	stdout, err := readOutputTail(filepath.Join(v.OutputDir, stdoutLogName), limit)
	if err != nil {
		return response, fmt.Errorf("read %s: %w", stdoutLogName, err)
	}
	stderr, err := readOutputTail(filepath.Join(v.OutputDir, stderrLogName), limit)
	if err != nil {
		return response, fmt.Errorf("read %s: %w", stderrLogName, err)
	}
	response.Stdout, response.Stderr = stdout, stderr
	return response, nil
}

type config struct {
	PeriodicAddr      string
	TaskPrefix        string
	RSA               protocol.RSAConnParam
	OutputRoot        string
	SkillsDir         string
	CodexBin          string
	CodexDocker       bool
	CodexDockerBin    string
	CodexDockerImage  string
	CodexDockerOptDir string
	CodexNoProxy      bool
	Debug             bool
	UploadBaseURL     string
	Timeout           time.Duration
	MaxOutput         int
	MaxFileBytes      int
}

type statusStore struct {
	root string
	mu   sync.Mutex
}

type taskManager struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newTaskManager() *taskManager {
	return &taskManager{cancels: make(map[string]context.CancelFunc)}
}

func (m *taskManager) register(id string, cancel context.CancelFunc) {
	m.mu.Lock()
	m.cancels[id] = cancel
	m.mu.Unlock()
}

func (m *taskManager) unregister(id string) {
	m.mu.Lock()
	delete(m.cancels, id)
	m.mu.Unlock()
}

func (m *taskManager) cancel(id string) {
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func newConfig() (config, error) {
	addr := strings.TrimRight(strings.TrimSpace(os.Getenv("PERIODIC_PORT")), "/")
	if addr == "" {
		return config{}, errors.New("PERIODIC_PORT is required")
	}
	root, err := filepath.Abs(strings.TrimSpace(os.Getenv("SKILL2API_OUTPUT_ROOT")))
	if err != nil || root == "." || strings.TrimSpace(os.Getenv("SKILL2API_OUTPUT_ROOT")) == "" {
		return config{}, errors.New("SKILL2API_OUTPUT_ROOT is required")
	}
	skills := strings.TrimSpace(os.Getenv("SKILL2API_SKILLS_DIR"))
	if skills == "" {
		skills = "skills"
	}
	mode, err := parseRSAMode(os.Getenv("PERIODIC_RSA_MODE"))
	if err != nil {
		return config{}, err
	}
	priv := strings.TrimSpace(os.Getenv("PERIODIC_RSA_PRIVATE_KEY_PATH"))
	pub := strings.TrimSpace(os.Getenv("PERIODIC_RSA_PUBLIC_KEY_PATH"))
	if mode != protocol.ModePlain && (priv == "" || pub == "") {
		return config{}, errors.New("generation RSA key paths are required unless plain mode is selected")
	}
	// Video-generation skills can legitimately spend hours waiting on remote providers.
	timeout := envDuration("SKILL2API_CODEX_TIMEOUT_SECONDS", 6*time.Hour)
	if timeout <= 0 {
		return config{}, errors.New("SKILL2API_CODEX_TIMEOUT_SECONDS must be positive")
	}
	limit := envInt("SKILL2API_MAX_OUTPUT_BYTES", maxOutput)
	if limit < 1024 {
		limit = 1024
	}
	fileLimit := envInt("SKILL2API_MAX_FILE_BYTES", maxFileBytes)
	if fileLimit < 1 {
		fileLimit = maxFileBytes
	}
	noProxy, err := strconv.ParseBool(firstEnvDefault("SKILL2API_CODEX_NO_PROXY", "false"))
	if err != nil {
		return config{}, errors.New("SKILL2API_CODEX_NO_PROXY must be true or false")
	}
	dockerEnabled, err := strconv.ParseBool(firstEnvDefault("SKILL2API_CODEX_DOCKER", "false"))
	if err != nil {
		return config{}, errors.New("SKILL2API_CODEX_DOCKER must be true or false")
	}
	debug, err := strconv.ParseBool(firstEnvDefault("SKILL2API_DEBUG", "false"))
	if err != nil {
		return config{}, errors.New("SKILL2API_DEBUG must be true or false")
	}
	optDir := strings.TrimSpace(os.Getenv("SKILL2API_CODEX_DOCKER_OPT_DIR"))
	if dockerEnabled && optDir != "" {
		optDir, err = filepath.Abs(optDir)
		if err != nil {
			return config{}, fmt.Errorf("resolve SKILL2API_CODEX_DOCKER_OPT_DIR: %w", err)
		}
		info, statErr := os.Stat(optDir)
		if statErr != nil || !info.IsDir() {
			if statErr != nil {
				return config{}, fmt.Errorf("SKILL2API_CODEX_DOCKER_OPT_DIR must be an existing directory: %w", statErr)
			}
			return config{}, errors.New("SKILL2API_CODEX_DOCKER_OPT_DIR must be an existing directory")
		}
	}
	uploadBaseURL := strings.TrimRight(firstEnvDefault("SKILL2API_UPLOAD_BASE_URL", "https://huabot.com"), "/")
	uploadURL, err := url.ParseRequestURI(uploadBaseURL)
	if err != nil || uploadURL.Scheme == "" || uploadURL.Host == "" {
		return config{}, errors.New("SKILL2API_UPLOAD_BASE_URL must be an absolute URL")
	}
	return config{PeriodicAddr: addr, TaskPrefix: strings.TrimSpace(os.Getenv("TASK_PREFIX")), RSA: protocol.RSAConnParam{Mode: mode, PrivateKeyPath: priv, ServerPublicKeyPath: pub}, OutputRoot: root, SkillsDir: skills, CodexBin: firstEnvDefault("SKILL2API_CODEX_BIN", "codex"), CodexDocker: dockerEnabled, CodexDockerBin: firstEnvDefault("SKILL2API_CODEX_DOCKER_BIN", "docker"), CodexDockerImage: firstEnvDefault("SKILL2API_CODEX_DOCKER_IMAGE", "lupino/sandbox-runner:latest"), CodexDockerOptDir: optDir, CodexNoProxy: noProxy, Debug: debug, UploadBaseURL: uploadBaseURL, Timeout: timeout, MaxOutput: limit, MaxFileBytes: fileLimit}, nil
}
func firstEnvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return n
}
func envDuration(key string, fallback time.Duration) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}
func parseRSAMode(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return protocol.ModeAES, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < protocol.ModePlain || n > protocol.ModeAES {
		return 0, fmt.Errorf("invalid RSA mode: %q", raw)
	}
	return n, nil
}
func withPrefix(prefix, fn string) string { return strings.TrimSpace(prefix) + fn }

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
	return submit(withPrefix(c.TaskPrefix, cleanupFunc), requestID, map[string]interface{}{
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
	if err := os.RemoveAll(filepath.Join(s.root, id)); err != nil {
		log.Printf("event=skill2api_cleanup request_id=%s result=data_remove_failed error=%q", id, err)
		result.Status, result.Error = "failed", "remove task data"
		return result
	}
	log.Printf("event=skill2api_cleanup request_id=%s result=deleted", id)
	result.Status = "deleted"
	return result
}

func validateRequest(req generateRequest, c config) error {
	if !validID(req.RequestID) {
		return errors.New("request_id must contain only letters, digits, dot, underscore, or hyphen")
	}
	if _, err := skillNames(c, req.SkillName); err != nil {
		return err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("prompt is required")
	}
	if strings.TrimSpace(req.Model) != req.Model || strings.ContainsRune(req.Model, '\x00') {
		return errors.New("invalid model")
	}
	if err := validateEnvironment(req.Environment); err != nil {
		return err
	}
	if _, err := resolveOutputDir(req.OutputDir, c.OutputRoot); err != nil {
		return err
	}
	return nil
}

// skillNames parses the public comma-separated skill_name field and verifies
// that every selected package remains within the configured skills directory.
func skillNames(c config, raw string) ([]string, error) {
	if len(raw) > maxSkillName || strings.TrimSpace(raw) == "" {
		return nil, errors.New("invalid skill_name")
	}
	skillRoot, err := filepath.Abs(c.SkillsDir)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(skillRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve skills directory: %w", err)
	}
	seen := make(map[string]struct{})
	names := make([]string, 0, strings.Count(raw, ",")+1)
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" || filepath.Base(name) != name || strings.Contains(name, "..") {
			return nil, errors.New("invalid skill_name")
		}
		if _, exists := seen[name]; exists {
			return nil, errors.New("duplicate skill_name")
		}
		skillDir := filepath.Join(skillRoot, name)
		if !within(skillRoot, skillDir) {
			return nil, errors.New("invalid skill_name")
		}
		resolvedDir, err := filepath.EvalSymlinks(skillDir)
		if err != nil || !within(resolvedRoot, resolvedDir) {
			return nil, errors.New("invalid skill_name")
		}
		info, err := os.Stat(resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("skill not found: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("skill not found: not a directory")
		}
		skillFile := filepath.Join(resolvedDir, "SKILL.md")
		resolvedFile, err := filepath.EvalSymlinks(skillFile)
		if err != nil || !within(resolvedDir, resolvedFile) {
			return nil, errors.New("invalid skill_name")
		}
		if _, err := os.Stat(resolvedFile); err != nil {
			return nil, fmt.Errorf("skill not found: %w", err)
		}
		if err := validateSkillResources(resolvedDir); err != nil {
			return nil, err
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func validateSkillResources(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir || info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !withinOrSame(dir, resolved) {
			return errors.New("skill resource symlink escapes skill directory")
		}
		return nil
	})
}

func withinOrSame(root, path string) bool {
	return filepath.Clean(root) == filepath.Clean(path) || within(root, path)
}

func skillDir(c config, name string) (string, error) {
	names, err := skillNames(c, name)
	if err != nil || len(names) != 1 {
		if err == nil {
			err = errors.New("invalid skill_name")
		}
		return "", err
	}
	root, err := filepath.Abs(c.SkillsDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Join(root, names[0]))
}

func readSkills(c config, raw string) ([][]byte, []string, error) {
	names, err := skillNames(c, raw)
	if err != nil {
		return nil, nil, err
	}
	skills := make([][]byte, 0, len(names))
	for _, name := range names {
		dir, err := skillDir(c, name)
		if err != nil {
			return nil, nil, err
		}
		skill, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			return nil, nil, fmt.Errorf("read skill %q: %w", name, err)
		}
		skills = append(skills, skill)
	}
	return skills, names, nil
}

func validateEnvironment(values map[string]string) error {
	for key, value := range values {
		if !validEnvironmentName(key) || strings.ContainsRune(value, '\x00') {
			return errors.New("invalid environment")
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func resolveOutputDir(raw, root string) (string, error) {
	rel := strings.TrimSpace(raw)
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("output_dir must be relative to SKILL2API_OUTPUT_ROOT")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	out, err := filepath.Abs(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	if !within(root, out) {
		return "", errors.New("output_dir must be inside SKILL2API_OUTPUT_ROOT")
	}
	return out, nil
}
func validID(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

func (s *statusStore) recoverRunning() error {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		v, err := s.read(entry.Name())
		if err != nil || v.Status != "running" {
			continue
		}
		v.Error = "worker interrupted"
		if v.SessionID != "" {
			v.Status = "interrupted"
			v.FinishedAt = ""
		} else {
			v.Status = "failed"
			v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		log.Printf("event=skill2api_recover request_id=%s status=%s error=%q", v.RequestID, v.Status, v.Error)
		if err := s.write(v); err != nil {
			return err
		}
	}
	return nil
}

type inputRequest struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func parseInputRequest(stdout string) (inputRequest, bool, error) {
	lines := strings.Split(strings.TrimRight(stdout, "\r\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-2]) != "SKILL2API_INPUT_REQUIRED" {
		return inputRequest{}, false, nil
	}
	var request inputRequest
	if err := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &request); err != nil {
		return inputRequest{}, true, fmt.Errorf("invalid input request: %w", err)
	}
	if strings.TrimSpace(request.Question) == "" {
		return inputRequest{}, true, errors.New("input request question is empty")
	}
	return request, true, nil
}

func hasTokenUsageSummary(output string) bool {
	lines := strings.Split(strings.TrimRight(output, "\r\n"), "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end < 2 || !strings.Contains(strings.ToLower(strings.TrimSpace(lines[end-2])), "tokens used") {
		return false
	}
	for _, field := range strings.Fields(strings.TrimSpace(lines[end-1])) {
		if !numericTokenField(field) {
			return false
		}
	}
	return strings.TrimSpace(lines[end-1]) != ""
}

func numericTokenField(value string) bool {
	value = strings.Trim(value, ",")
	if value == "" {
		return false
	}
	for _, r := range value {
		if r != ',' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func interruptedDockerExecution(c config, sessionID, stderr string) bool {
	return c.CodexDocker && sessionID != "" && !hasTokenUsageSummary(stderr)
}

func inputRequestFromLog(outputDir string) (inputRequest, bool, error) {
	return inputRequestFromLogSince(outputDir, 0)
}

func inputRequestFromLogSince(outputDir string, offset int64) (inputRequest, bool, error) {
	stdout, err := readOutputTailSince(filepath.Join(outputDir, stdoutLogName), offset, maxInputTailBytes)
	if err != nil {
		return inputRequest{}, true, fmt.Errorf("read %s: %w", stdoutLogName, err)
	}
	return parseInputRequest(stdout)
}

func sessionIDFromStderr(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		const marker = "session id:"
		at := strings.Index(strings.ToLower(line), marker)
		if at >= 0 {
			return strings.TrimSpace(line[at+len(marker):])
		}
	}
	return ""
}

func runCodex(ctx context.Context, c config, req generateRequest, skill []byte) (string, string, error) {
	stdout, stderr, _, err := runCodexWithSession(ctx, c, req, skill)
	return stdout, stderr, err
}

func runCodexWithSession(ctx context.Context, c config, req generateRequest, skill []byte) (string, string, string, error) {
	return runCodexWithSessionCallback(ctx, c, req, skill, nil)
}

func codexPrompt(prompt string, skill []byte) string {
	return prompt + "\n\n" + string(skill)
}

func codexPromptSkills(prompt string, skills [][]byte, names []string, docker bool) string {
	if len(skills) == 1 && !docker {
		return codexPrompt(prompt, skills[0])
	}
	var out strings.Builder
	out.WriteString(prompt)
	for _, skill := range skills {
		out.WriteString("\n\n")
		out.Write(skill)
	}
	if docker {
		out.WriteString("\n\nSelected skill packages are mounted read-only. Resolve package-relative resources using:\n")
		for _, name := range names {
			fmt.Fprintf(&out, "- %s: /workspace/skills/%s\n", name, name)
		}
	}
	return out.String()
}

func runCodexWithSessionCallback(ctx context.Context, c config, req generateRequest, skill []byte, onSessionID func(string)) (string, string, string, error) {
	return runCodexWithSessionCallbackSkills(ctx, c, req, [][]byte{skill}, nil, onSessionID)
}

func runCodexWithSessionCallbackSkills(ctx context.Context, c config, req generateRequest, skills [][]byte, names []string, onSessionID func(string)) (string, string, string, error) {
	environment := codexRequestEnvironment(c, req.Environment)
	redactions := append(redactionLines(req.Prompt), environmentRedactions(environment)...)
	return runCodexCommandSkills(ctx, c, req.RequestID, req.OutputDir, names, false, environment, redactions, onSessionID, codexExecArgs(c, req.Model, req.OutputDir, []string{codexPromptSkills(req.Prompt, skills, names, c.CodexDocker)}))
}

func runCodexResume(ctx context.Context, c config, req taskStatus, answer, instruction string, environment map[string]string) (string, string, error) {
	prompt := "Continue the original task."
	if strings.TrimSpace(answer) != "" {
		prompt = fmt.Sprintf("User answer to your pending question: %s\n\nContinue the original task.", answer)
	}
	if strings.TrimSpace(instruction) != "" {
		prompt += fmt.Sprintf("\n\nAdditional user instruction: %s", instruction)
	}
	prompt += "\n\nIf another clarification is required, end your response with exactly two lines: SKILL2API_INPUT_REQUIRED and then a JSON object containing question and optional options."
	var names []string
	if c.CodexDocker {
		var err error
		names, err = skillNames(c, req.SkillName)
		if err != nil {
			return "", "", err
		}
		prompt += "\n\nSelected skill packages remain mounted read-only:\n"
		for _, name := range names {
			prompt += fmt.Sprintf("- %s: /workspace/skills/%s\n", name, name)
		}
	}
	environment = codexRequestEnvironment(c, environment)
	redactions := append(redactionLines(answer), redactionLines(instruction)...)
	redactions = append(redactions, environmentRedactions(environment)...)
	stdout, stderr, _, err := runCodexCommandSkills(ctx, c, req.RequestID, req.OutputDir, names, true, environment, redactions, nil, codexExecArgs(c, req.Model, req.OutputDir, []string{"resume", req.SessionID, prompt}))
	return stdout, stderr, err
}

func runCodexCommand(ctx context.Context, c config, requestID, outputDir string, appendLogs bool, environment map[string]string, redactions []string, onSessionID func(string), args []string) (string, string, string, error) {
	return runCodexCommandSkills(ctx, c, requestID, outputDir, nil, appendLogs, environment, redactions, onSessionID, args)
}

func runCodexCommandSkills(ctx context.Context, c config, requestID, outputDir string, names []string, appendLogs bool, environment map[string]string, redactions []string, onSessionID func(string), args []string) (string, string, string, error) {
	stdoutOffset, stderrOffset, err := outputLogOffsets(outputDir)
	if err != nil {
		return "", "", "", err
	}
	if c.Debug {
		redactions = nil
	}
	if !appendLogs {
		stdoutOffset, stderrOffset = 0, 0
	}
	stdoutLog, stderrLog, err := openOutputLogs(outputDir, appendLogs)
	if err != nil {
		return "", "", "", err
	}
	cmd, executable, err := newCodexCommandSkills(ctx, c, requestID, outputDir, names, environment, args)
	if err != nil {
		_ = stdoutLog.Close()
		_ = stderrLog.Close()
		return "", "", "", err
	}
	log.Printf("event=skill2api_codex_start output_dir=%s codex_bin=%s timeout=%s", outputDir, executable, c.Timeout)
	stdoutWriter := &redactingLogWriter{file: stdoutLog, redactions: redactions, redact: !c.Debug}
	stderrWriter := &redactingLogWriter{file: stderrLog, redactions: redactions, redact: !c.Debug, onSessionID: onSessionID}
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	err = cmd.Run()
	closeErr := closeOutputLogs(stdoutWriter, stderrWriter)
	stdout, stdoutErr := readOutputTailSince(filepath.Join(outputDir, stdoutLogName), stdoutOffset, c.MaxOutput)
	stderr, stderrErr := readOutputTailSince(filepath.Join(outputDir, stderrLogName), stderrOffset, c.MaxOutput)
	if closeErr != nil {
		return stdout, stderr, stderrWriter.sessionID, closeErr
	}
	if stdoutErr != nil {
		return stdout, stderr, stderrWriter.sessionID, fmt.Errorf("read %s: %w", stdoutLogName, stdoutErr)
	}
	if stderrErr != nil {
		return stdout, stderr, stderrWriter.sessionID, fmt.Errorf("read %s: %w", stderrLogName, stderrErr)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.Printf("event=skill2api_codex_finish result=timeout stdout_bytes=%d stderr_bytes=%d", len(stdout), len(stderr))
		return stdout, stderr, stderrWriter.sessionID, fmt.Errorf("codex timed out")
	}
	if err != nil {
		log.Printf("event=skill2api_codex_finish result=failed error=%q exit_code=%d stdout_bytes=%d stderr_bytes=%d", err, commandExitCode(err), len(stdout), len(stderr))
		return stdout, stderr, stderrWriter.sessionID, fmt.Errorf("codex exited: %w", err)
	}
	log.Printf("event=skill2api_codex_finish result=succeeded stdout_bytes=%d stderr_bytes=%d", len(stdout), len(stderr))
	return stdout, stderr, stderrWriter.sessionID, nil
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func codexRequestEnvironment(c config, values map[string]string) map[string]string {
	merged := make(map[string]string, len(values)+1)
	for key, value := range values {
		merged[key] = value
	}
	if c.CodexDocker {
		if _, provided := merged["SANDBOX_AI_KEY"]; !provided {
			if value, ok := os.LookupEnv("SANDBOX_AI_KEY"); ok {
				merged["SANDBOX_AI_KEY"] = value
			}
		}
	}
	return merged
}

func codexExecArgs(c config, model, outputDir string, tail []string) []string {
	workDir := outputDir
	sandbox := "workspace-write"
	if c.CodexDocker {
		workDir = "/workspace"
		sandbox = "danger-full-access"
	}
	args := []string{"exec"}
	if !c.CodexDocker {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--sandbox", sandbox, "--cd", workDir, "--skip-git-repo-check")
	return append(args, tail...)
}

func codexHomeDir(c config, requestID string) string {
	return filepath.Join(c.OutputRoot, ".skill2api-codex", requestID, "home")
}

func prepareDockerCodexHome(home string) error {
	configDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("create Codex config directory: %w", err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(dockerCodexConfig), 0600); err != nil {
		return fmt.Errorf("write Codex config: %w", err)
	}
	if err := os.Chmod(configPath, 0600); err != nil {
		return fmt.Errorf("protect Codex config: %w", err)
	}
	return nil
}

func dockerEnvironment(c config, environment map[string]string) map[string]string {
	merged := make(map[string]string, len(environment)+1)
	for key, value := range environment {
		merged[key] = value
	}
	if c.CodexDockerOptDir != "" {
		merged["PATH"] = "/opt/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return merged
}

func removeDockerContainer(ctx context.Context, c config, requestID string) error {
	if !c.CodexDocker {
		return nil
	}
	containerName := dockerContainerName(requestID)
	cmd := exec.CommandContext(ctx, c.CodexDockerBin, "rm", "--force", containerName)
	output, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(output), "No such container") {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("remove Docker container %q: %w", containerName, err)
	}
	return fmt.Errorf("remove Docker container %q: %w: %s", containerName, err, message)
}

func dockerContainerName(requestID string) string {
	// Docker requires names to start with an alphanumeric character, unlike valid request IDs.
	return "skill2api-" + requestID
}

func newCodexCommand(ctx context.Context, c config, requestID, outputDir string, environment map[string]string, args []string) (*exec.Cmd, string, error) {
	return newCodexCommandSkills(ctx, c, requestID, outputDir, nil, environment, args)
}

func newCodexCommandSkills(ctx context.Context, c config, requestID, outputDir string, names []string, environment map[string]string, args []string) (*exec.Cmd, string, error) {
	if !c.CodexDocker {
		home := codexHomeDir(c, requestID)
		if err := os.MkdirAll(home, 0700); err != nil {
			return nil, c.CodexBin, fmt.Errorf("create Codex home: %w", err)
		}
		cmd := exec.CommandContext(ctx, c.CodexBin, args...)
		nativeEnvironment := make(map[string]string, len(environment)+1)
		for key, value := range environment {
			nativeEnvironment[key] = value
		}
		nativeEnvironment["HOME"] = home
		cmd.Env = codexEnvironment(os.Environ(), nativeEnvironment, c.CodexNoProxy)
		return cmd, c.CodexBin, nil
	}
	home := codexHomeDir(c, requestID)
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, c.CodexDockerBin, fmt.Errorf("create Codex home: %w", err)
	}
	if err := prepareDockerCodexHome(home); err != nil {
		return nil, c.CodexDockerBin, err
	}
	dockerArgs := []string{"run", "--rm", "--name", dockerContainerName(requestID), "--init", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
	dockerArgs = append(dockerArgs,
		"--mount", "type=bind,src="+outputDir+",dst=/workspace",
		"--mount", "type=bind,src="+home+",dst=/home/ubuntu",
		"--workdir", "/workspace",
		"--env", "HOME=/home/ubuntu",
	)
	for _, name := range names {
		dir, err := skillDir(c, name)
		if err != nil {
			return nil, c.CodexDockerBin, err
		}
		dockerArgs = append(dockerArgs, "--mount", "type=bind,src="+dir+",dst=/workspace/skills/"+name+",readonly")
	}
	dockerEnv := dockerEnvironment(c, environment)
	if c.CodexDockerOptDir != "" {
		dockerArgs = append(dockerArgs, "--mount", "type=bind,src="+c.CodexDockerOptDir+",dst=/opt,readonly")
	}
	keys := make([]string, 0, len(dockerEnv))
	for key := range dockerEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		dockerArgs = append(dockerArgs, "--env", key)
	}
	dockerArgs = append(dockerArgs, c.CodexDockerImage)
	// The sandbox image entrypoint executes its arguments directly, so Docker
	// runs must include the executable that native exec.Command supplies.
	dockerArgs = append(dockerArgs, c.CodexBin)
	dockerArgs = append(dockerArgs, args...)
	log.Printf("event=skill2api_codex_command mode=docker docker_bin=%s image=%s network_access=true opt_dir_mounted=%t skill_count=%d output_dir=%s container_workdir=/workspace container_home=/home/ubuntu codex_subcommand=%s", c.CodexDockerBin, c.CodexDockerImage, c.CodexDockerOptDir != "", len(names), outputDir, codexSubcommand(args))
	cmd := exec.CommandContext(ctx, c.CodexDockerBin, dockerArgs...)
	cmd.Env = codexEnvironment(os.Environ(), dockerEnv, c.CodexNoProxy)
	return cmd, c.CodexDockerBin, nil
}

func codexSubcommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func withoutProxyEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, value := range environ {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func codexEnvironment(environ []string, overrides map[string]string, noProxy bool) []string {
	merged := append([]string(nil), environ...)
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered := merged[:0]
		for _, value := range merged {
			current, _, _ := strings.Cut(value, "=")
			if current != key {
				filtered = append(filtered, value)
			}
		}
		merged = append(filtered, key+"="+overrides[key])
	}
	if noProxy {
		return withoutProxyEnv(merged)
	}
	return merged
}

func environmentRedactions(values map[string]string) []string {
	var redactions []string
	for _, value := range values {
		redactions = append(redactions, redactionLines(value)...)
	}
	sort.Slice(redactions, func(i, j int) bool {
		if len(redactions[i]) == len(redactions[j]) {
			return redactions[i] < redactions[j]
		}
		return len(redactions[i]) > len(redactions[j])
	})
	return redactions
}

func openOutputLogs(outputDir string, appendLogs bool) (*os.File, *os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY
	if appendLogs {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	stdout, err := os.OpenFile(filepath.Join(outputDir, stdoutLogName), flags, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", stdoutLogName, err)
	}
	stderr, err := os.OpenFile(filepath.Join(outputDir, stderrLogName), flags, 0600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("open %s: %w", stderrLogName, err)
	}
	return stdout, stderr, nil
}

type redactingLogWriter struct {
	file        *os.File
	redactions  []string
	redact      bool
	pending     []byte
	sessionID   string
	onSessionID func(string)
}

func redactionLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (w *redactingLogWriter) Write(p []byte) (int, error) {
	originalLen := len(p)
	w.pending = append(w.pending, p...)
	for {
		at := bytes.IndexByte(w.pending, '\n')
		if at < 0 {
			break
		}
		if err := w.writeLine(w.pending[:at+1]); err != nil {
			return originalLen, err
		}
		w.pending = w.pending[at+1:]
	}
	return originalLen, nil
}

func (w *redactingLogWriter) writeLine(line []byte) error {
	text := string(line)
	if sessionID := sessionIDFromStderr(text); sessionID != "" {
		w.sessionID = sessionID
		if w.onSessionID != nil {
			w.onSessionID(sessionID)
		}
	}
	if w.redact {
		for _, redaction := range w.redactions {
			text = strings.ReplaceAll(text, redaction, "[redacted]")
		}
		text = redactSessionID(text)
	}
	_, err := w.file.WriteString(text)
	return err
}

func redactSessionID(text string) string {
	const marker = "session id:"
	lower := strings.ToLower(text)
	at := strings.Index(lower, marker)
	if at < 0 {
		return text
	}
	suffix := ""
	if strings.HasSuffix(text, "\n") {
		suffix = "\n"
	}
	return text[:at+len(marker)] + " [redacted]" + suffix
}

func (w *redactingLogWriter) Close() error {
	if len(w.pending) > 0 {
		if err := w.writeLine(w.pending); err != nil {
			return err
		}
		w.pending = nil
	}
	return w.file.Close()
}

func closeOutputLogs(stdout, stderr *redactingLogWriter) error {
	stdoutErr := stdout.Close()
	stderrErr := stderr.Close()
	if stdoutErr != nil {
		return fmt.Errorf("close %s: %w", stdoutLogName, stdoutErr)
	}
	if stderrErr != nil {
		return fmt.Errorf("close %s: %w", stderrLogName, stderrErr)
	}
	return nil
}

func readOutputTail(path string, limit int) (string, error) {
	return readOutputTailSince(path, 0, limit)
}

func outputLogOffsets(outputDir string) (int64, int64, error) {
	stdout, err := outputLogSize(filepath.Join(outputDir, stdoutLogName))
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", stdoutLogName, err)
	}
	stderr, err := outputLogSize(filepath.Join(outputDir, stderrLogName))
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", stderrLogName, err)
	}
	return stdout, stderr, nil
}

func outputLogSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func readOutputTailSince(path string, offset int64, limit int) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	start := info.Size() - int64(limit)
	if start < offset {
		start = offset
	}
	truncated := start > offset
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if err != nil {
		return "", err
	}
	if truncated {
		return "[earlier output truncated]\n" + string(data), nil
	}
	return string(data), nil
}

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
	var lastErr error
	for attempt := 1; attempt <= fileUploadAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), fileUploadTimeout)
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

func execute(store *statusStore, manager *taskManager, c config, req generateRequest) {
	v, err := store.claimQueued(req.RequestID)
	if err != nil {
		if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
			return
		}
		log.Printf("event=skill2api_execute request_id=%s result=not_started error=%q", req.RequestID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	manager.register(req.RequestID, cancel)
	defer manager.unregister(req.RequestID)
	if current, readErr := store.read(req.RequestID); readErr != nil || current.Status == "terminated" {
		cancel()
		return
	}
	log.Printf("event=skill2api_state request_id=%s status=running skill_name=%s output_dir=%s", req.RequestID, req.SkillName, req.OutputDir)
	if err := os.MkdirAll(req.OutputDir, 0750); err != nil {
		cancel()
		if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
			return
		}
		v.Status = "failed"
		v.Error = fmt.Sprintf("create output directory: %v", err)
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = store.writeFromRunning(v)
		log.Printf("event=skill2api_state request_id=%s status=failed error=%q", req.RequestID, v.Error)
		return
	}
	var stderr string
	skills, names, err := readSkills(c, req.SkillName)
	if err == nil {
		var sessionID string
		_, stderr, sessionID, err = runCodexWithSessionCallbackSkills(ctx, c, req, skills, names, func(sessionID string) {
			if updateErr := store.updateSessionID(req.RequestID, v.StartedAt, sessionID); updateErr != nil {
				log.Printf("event=skill2api_session request_id=%s result=persist_failed error=%q", req.RequestID, updateErr)
			}
		})
		cancel()
		v.SessionID = sessionID
	}
	if err != nil {
		if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
			return
		}
		if interruptedDockerExecution(c, v.SessionID, stderr) {
			v.Status = "interrupted"
			v.Error = "Docker execution interrupted before token usage was recorded"
			captureExistingFiles(&v)
		} else {
			v.Status = "failed"
			v.Error = err.Error()
		}
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if v.Status == "interrupted" {
			v.FinishedAt = ""
		}
		_ = store.write(v)
		log.Printf("event=skill2api_state request_id=%s status=%s error=%q", req.RequestID, v.Status, v.Error)
		return
	}
	if input, needed, parseErr := inputRequestFromLog(req.OutputDir); needed {
		if parseErr != nil || v.SessionID == "" {
			if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
				return
			}
			v.Status = "failed"
			if parseErr != nil {
				v.Error = parseErr.Error()
			} else {
				v.Error = "Codex session ID was not found; cannot resume interactive task"
			}
			v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_, _ = store.writeFromRunning(v)
			return
		}
		if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
			return
		}
		v.Status = "waiting_for_input"
		v.Phase = "clarification"
		v.Question = input.Question
		v.Options = input.Options
		v.Error = ""
		_, _ = store.writeFromRunning(v)
		log.Printf("event=skill2api_state request_id=%s status=waiting_for_input", req.RequestID)
		return
	}
	if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
		return
	}
	stderr, readErr := readOutputTail(filepath.Join(req.OutputDir, stderrLogName), c.MaxOutput)
	if readErr != nil {
		v.Status = "failed"
		v.Error = fmt.Errorf("read %s: %w", stderrLogName, readErr).Error()
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = store.writeFromRunning(v)
		return
	}
	if interruptedDockerExecution(c, v.SessionID, stderr) {
		v.Status = "interrupted"
		v.Error = "Docker execution ended without token usage summary"
		captureExistingFiles(&v)
		v.FinishedAt = ""
		_, _ = store.writeFromRunning(v)
		log.Printf("event=skill2api_state request_id=%s status=interrupted reason=missing_token_usage", req.RequestID)
		return
	}
	v.Status = "succeeded"
	v.Files, err = listFiles(req.OutputDir)
	if err != nil {
		v.Status = "failed"
		v.Error = err.Error()
	}
	v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.writeFromRunning(v)
	log.Printf("event=skill2api_state request_id=%s status=%s files=%d error=%q", req.RequestID, v.Status, len(v.Files), v.Error)
}

func executeResume(store *statusStore, manager *taskManager, c config, req resumeRequest, v taskStatus) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	manager.register(req.RequestID, cancel)
	defer manager.unregister(req.RequestID)
	if current, readErr := store.read(req.RequestID); readErr != nil || current.Status == "terminated" {
		cancel()
		return
	}
	stdoutOffset, err := outputLogSize(filepath.Join(v.OutputDir, stdoutLogName))
	if err == nil {
		_, _, err = runCodexResume(ctx, c, v, req.Answer, req.Instruction, req.Environment)
	}
	cancel()
	if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
		return
	}
	v.Question, v.Options, v.Phase = "", nil, ""
	if err != nil {
		v.Status = "failed"
		v.Error = err.Error()
	} else if input, needed, parseErr := inputRequestFromLogSince(v.OutputDir, stdoutOffset); needed {
		if parseErr != nil {
			v.Status, v.Error = "failed", parseErr.Error()
		} else {
			v.Status, v.Phase = "waiting_for_input", "clarification"
			v.Question, v.Options = input.Question, input.Options
			v.Error = ""
		}
	} else {
		stderr, readErr := readOutputTail(filepath.Join(v.OutputDir, stderrLogName), c.MaxOutput)
		if readErr != nil {
			v.Status, v.Error = "failed", fmt.Sprintf("read %s: %v", stderrLogName, readErr)
		} else if c.CodexDocker && v.SessionID != "" && !hasTokenUsageSummary(stderr) {
			v.Status, v.Error = "interrupted", "Docker execution ended without token usage summary"
		}
		if v.Status == "interrupted" {
			captureExistingFiles(&v)
			v.FinishedAt = ""
			_, _ = store.writeFromRunning(v)
			return
		}
		v.Status = "succeeded"
		v.Files, err = listFiles(v.OutputDir)
		if err != nil {
			v.Status, v.Error = "failed", err.Error()
		}
	}
	v.FinishedAt = ""
	if v.Status == "succeeded" || v.Status == "failed" {
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, _ = store.writeFromRunning(v)
}

func parseArgs[T any](job periodic.Job, out *T) error {
	if strings.TrimSpace(job.Args) == "" {
		return errors.New("job args is empty")
	}
	return json.Unmarshal([]byte(job.Args), out)
}
func doneJSON(job periodic.Job, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		_ = job.Fail()
		return
	}
	_ = job.Done(data)
}
func handleGenerate(job periodic.Job, store *statusStore, manager *taskManager, c config) {
	var req generateRequest
	if err := parseArgs(job, &req); err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=invalid_args error=%q", req.RequestID, err)
		_ = job.Fail()
		return
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_generate request_id=%s result=request_id_mismatch job_name=%s", req.RequestID, job.Name)
		_ = job.Fail()
		return
	}
	if err := validateRequest(req, c); err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	names, err := skillNames(c, req.SkillName)
	if err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	req.SkillName = strings.Join(names, ",")
	outputDir, err := resolveOutputDir(req.OutputDir, c.OutputRoot)
	if err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	req.OutputDir = outputDir
	now := time.Now().UTC().Format(time.RFC3339Nano)
	v := taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: now, SkillName: req.SkillName, Model: req.Model, OutputDir: req.OutputDir}
	if err := store.create(v, req.Force); err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=duplicate error=%q", req.RequestID, err)
		_ = job.Fail()
		return
	}
	log.Printf("event=skill2api_state request_id=%s status=queued skill_name=%s output_dir=%s force=%t", req.RequestID, req.SkillName, req.OutputDir, req.Force)
	scheduleCleanup(c, req.RequestID)
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "queued"})
	go execute(store, manager, c, req)
}
func handleStatus(job periodic.Job, store *statusStore, c config) {
	var req statusRequest
	if strings.TrimSpace(job.Args) != "" {
		if err := parseArgs(job, &req); err != nil {
			log.Printf("event=skill2api_status request_id=%s result=invalid_args", req.RequestID)
			_ = job.Fail()
			return
		}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_status request_id=%s result=request_id_mismatch job_name=%s", req.RequestID, job.Name)
		_ = job.Fail()
		return
	}
	if !validID(req.RequestID) {
		log.Printf("event=skill2api_status request_id=%s result=invalid_args", req.RequestID)
		_ = job.Fail()
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("event=skill2api_status request_id=%s result=not_found", req.RequestID)
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "not_found", "error": "request not found"})
			return
		}
		log.Printf("event=skill2api_status request_id=%s result=corrupt error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	if files, listErr := listFiles(v.OutputDir); listErr != nil {
		log.Printf("event=skill2api_status request_id=%s result=file_list_failed error=%q", req.RequestID, listErr)
	} else {
		v.Files = files
	}
	response, outputErr := statusWithLogOutput(v, c.MaxOutput)
	if outputErr != nil {
		log.Printf("event=skill2api_status request_id=%s result=log_read_failed error=%q", req.RequestID, outputErr)
	}
	doneJSON(job, response)
}

func handleFile(job periodic.Job, store *statusStore, c config) {
	var req fileRequest
	if err := parseArgs(job, &req); err != nil {
		log.Printf("event=skill2api_file request_id=%s result=invalid_args", job.Name)
		doneJSON(job, map[string]any{"request_id": job.Name, "status": "failed", "error": "invalid file request"})
		return
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_file request_id=%s result=request_id_mismatch job_name=%s", req.RequestID, job.Name)
		doneJSON(job, map[string]any{"request_id": job.Name, "status": "failed", "error": "request_id must match job name"})
		return
	}
	if !validID(req.RequestID) {
		log.Printf("event=skill2api_file request_id=%s result=invalid_request_id", req.RequestID)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": "invalid request_id"})
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "not_found", "error": "request not found"})
			return
		}
		log.Printf("event=skill2api_file request_id=%s result=status_read_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	data, err := readTaskFile(v.OutputDir, req.FilePath, c.MaxFileBytes)
	if err != nil {
		log.Printf("event=skill2api_file request_id=%s result=file_read_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileUploadTimeout)
	file, uploadURL, err := uploadTemporaryFile(ctx, c, req, data)
	cancel()
	if err != nil {
		log.Printf("event=skill2api_file request_id=%s result=upload_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	log.Printf("event=skill2api_file request_id=%s result=uploaded bytes=%d", req.RequestID, len(data))
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "succeeded", "file": file, "url": uploadURL})
}

func handleFileDelivery(job periodic.Job, store *statusStore, c config) {
	var req fileDeliveryRequest
	if err := parseArgs(job, &req); err != nil {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid file delivery request"})
		return
	}
	if strings.TrimSpace(req.DeliveryID) == "" {
		req.DeliveryID = job.Name
	}
	if req.DeliveryID != job.Name || !validID(req.DeliveryID) || !validID(req.RequestID) {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid delivery identifier"})
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "not_found", "error": "request not found"})
		return
	}
	data, err := readTaskFile(v.OutputDir, req.FilePath, c.MaxFileBytes)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	delivery := fileDeliveryStatus{
		RequestID: req.RequestID, DeliveryID: req.DeliveryID, FilePath: req.FilePath,
		FileKey: fileKeyForData(data), Status: "running", CreatedAt: now,
	}
	execute, err := store.startDelivery(delivery)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": err.Error()})
		return
	}
	if !execute {
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=reused file_key=%s", req.RequestID, req.DeliveryID, delivery.FileKey)
		doneJSON(job, delivery)
		return
	}
	file, uploadURL, attempts, uploadErr := uploadTemporaryFileWithRetry(c, fileRequest{
		RequestID: req.RequestID, FilePath: req.FilePath, Environment: req.Environment,
	}, data)
	delivery.Attempts = attempts
	delivery.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if uploadErr != nil {
		delivery.Status, delivery.Error = "failed", uploadErr.Error()
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=failed attempts=%d error=%q", req.RequestID, req.DeliveryID, attempts, uploadErr)
	} else {
		delivery.Status, delivery.File, delivery.URL = "succeeded", file, uploadURL
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=succeeded attempts=%d file_key=%s", req.RequestID, req.DeliveryID, attempts, delivery.FileKey)
	}
	if err := store.finishDelivery(delivery); err != nil {
		delivery.Status, delivery.Error = "failed", err.Error()
	}
	doneJSON(job, delivery)
}

func handleFileDeliveryStatus(job periodic.Job, store *statusStore) {
	var req fileDeliveryStatusRequest
	if err := parseArgs(job, &req); err != nil || req.DeliveryID != job.Name || !validID(req.RequestID) || !validID(req.DeliveryID) {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid file delivery status request"})
		return
	}
	delivery, err := store.readDelivery(req.RequestID, req.DeliveryID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "queued"})
			return
		}
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": "read file delivery status"})
		return
	}
	doneJSON(job, delivery)
}

func handleResume(job periodic.Job, store *statusStore, manager *taskManager, c config) {
	var req resumeRequest
	if strings.TrimSpace(job.Args) != "" {
		if err := parseArgs(job, &req); err != nil {
			_ = job.Fail()
			return
		}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		_ = job.Fail()
		return
	}
	if !validID(req.RequestID) || (strings.TrimSpace(req.Answer) != "" && strings.TrimSpace(req.Instruction) != "") {
		_ = job.Fail()
		return
	}
	if err := validateEnvironment(req.Environment); err != nil {
		_ = job.Fail()
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "not_found", "error": "request not found"})
		return
	}
	if v.Status == "waiting_for_input" {
		if strings.TrimSpace(req.Answer) == "" {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "answer is required while waiting for input"})
			return
		}
		v, err = store.claimWaiting(req.RequestID)
	} else if v.Status == "interrupted" {
		if strings.TrimSpace(req.Answer) != "" {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "answer is only valid while waiting for input"})
			return
		}
		v, err = store.claimInterrupted(req.RequestID)
	} else if v.Status == "succeeded" {
		if strings.TrimSpace(req.Instruction) == "" {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "instruction is required for a completed request"})
			return
		}
		v, err = store.claimSucceeded(req.RequestID)
	} else if v.Status == "failed" {
		if strings.TrimSpace(req.Answer) != "" {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "answer is only valid while waiting for input"})
			return
		}
		v, err = store.claimFailed(req.RequestID)
	} else if v.Status == "terminated" {
		if strings.TrimSpace(req.Answer) != "" {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "answer is only valid while waiting for input"})
			return
		}
		v, err = store.claimTerminated(req.RequestID)
	} else {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": "request is not resumable"})
		return
	}
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "conflict", "error": err.Error()})
		return
	}
	if v.SessionID == "" {
		v.Status = "failed"
		v.Error = "Codex session ID is missing"
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = store.write(v)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": v.Error})
		return
	}
	scheduleCleanup(c, req.RequestID)
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "running"})
	go executeResume(store, manager, c, req, v)
}

func handleTerminate(job periodic.Job, store *statusStore, manager *taskManager, c config) {
	var req terminateRequest
	if strings.TrimSpace(job.Args) != "" {
		if err := parseArgs(job, &req); err != nil {
			_ = job.Fail()
			return
		}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		_ = job.Fail()
		return
	}
	if !validID(req.RequestID) {
		_ = job.Fail()
		return
	}
	v, err := store.terminate(req.RequestID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "not_found", "error": "request not found"})
			return
		}
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	if v.Status == "terminated" {
		manager.cancel(req.RequestID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := removeDockerContainer(ctx, c, req.RequestID); err != nil {
			log.Printf("event=skill2api_terminate request_id=%s result=container_cleanup_failed error=%q", req.RequestID, err)
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": err.Error()})
			return
		}
	}
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": v.Error})
}

func handleCleanup(job periodic.Job, store *statusStore, c config) {
	var req statusRequest
	if strings.TrimSpace(job.Args) != "" {
		if err := parseArgs(job, &req); err != nil {
			log.Printf("event=skill2api_cleanup result=invalid_args")
			_ = job.Fail()
			return
		}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_cleanup result=invalid_args")
		_ = job.Fail()
		return
	}
	if !validID(req.RequestID) {
		log.Printf("event=skill2api_cleanup request_id=%s result=invalid_args", req.RequestID)
		_ = job.Fail()
		return
	}
	result := store.cleanup(c, req.RequestID, time.Now().UTC())
	if result.Status == "deferred" {
		if err := job.SchedLater(int(retentionAge.Seconds())); err != nil {
			log.Printf("event=skill2api_cleanup request_id=%s result=defer_failed error=%q", req.RequestID, err)
			_ = job.Fail()
		}
		return
	}
	log.Printf("event=skill2api_cleanup request_id=%s result=%s", req.RequestID, result.Status)
	doneJSON(job, result)
}

func addWorkerFunc(worker *periodic.Worker, prefix, name string, handler func(periodic.Job)) error {
	function := withPrefix(prefix, name)
	if err := worker.AddFunc(function, handler); err != nil {
		log.Printf("event=skill2api_register function=%s result=failed error=%q", function, err)
		return err
	}
	log.Printf("event=skill2api_register function=%s result=registered", function)
	return nil
}

func registerWorkerFuncs(worker *periodic.Worker, prefix string, store *statusStore, manager *taskManager, c config) error {
	if err := addWorkerFunc(worker, prefix, generateFunc, func(job periodic.Job) { handleGenerate(job, store, manager, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, statusFunc, func(job periodic.Job) { handleStatus(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, fileFunc, func(job periodic.Job) { handleFile(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, fileDeliveryFunc, func(job periodic.Job) { handleFileDelivery(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, fileDeliveryStatusFunc, func(job periodic.Job) { handleFileDeliveryStatus(job, store) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, resumeFunc, func(job periodic.Job) { handleResume(job, store, manager, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, prefix, terminateFunc, func(job periodic.Job) { handleTerminate(job, store, manager, c) }); err != nil {
		return err
	}
	return addWorkerFunc(worker, prefix, cleanupFunc, func(job periodic.Job) { handleCleanup(job, store, c) })
}
func connectPeriodic(client *periodic.Client, addr string, rsa protocol.RSAConnParam) error {
	return client.Connect(addr, rsa)
}

func main() {
	c, err := newConfig()
	if err != nil {
		panic(err)
	}
	log.Printf("event=skill2api_starting output_root=%s skills_dir=%s task_prefix=%s timeout_seconds=%d", c.OutputRoot, c.SkillsDir, c.TaskPrefix, int(c.Timeout.Seconds()))
	store := &statusStore{root: c.OutputRoot}
	manager := newTaskManager()
	if err := os.MkdirAll(c.OutputRoot, 0750); err != nil {
		panic(err)
	}
	if err := store.recoverRunning(); err != nil {
		panic(err)
	}
	log.Printf("event=skill2api_startup result=recovered_running_requests")
	for {
		worker := periodic.NewWorker(8)
		worker.SetIOTimeout(30*time.Second, 10*time.Second)
		log.Printf("event=skill2api_connect result=starting")
		if err := connectPeriodic(&worker.Client, c.PeriodicAddr, c.RSA); err != nil {
			log.Printf("event=skill2api_connect result=failed error=%q", err)
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		log.Printf("event=skill2api_connect result=connected")
		if err := registerWorkerFuncs(worker, c.TaskPrefix, store, manager, c); err != nil {
			log.Printf("event=skill2api_startup result=registration_failed error=%q", err)
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		log.Printf("event=skill2api_worker result=started")
		worker.Work()
		log.Printf("event=skill2api_worker result=stopped")
		worker.Close()
		time.Sleep(time.Second)
	}
}
