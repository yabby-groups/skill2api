package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	generateFunc  = "skill2api_generate"
	statusFunc    = "skill2api_status"
	resumeFunc    = "skill2api_resume"
	terminateFunc = "skill2api_terminate"
	maxOutput     = 64 * 1024
	maxSkillName  = 128
	stdoutLogName = "stdout.log"
	stderrLogName = "stderr.log"
)

type generateRequest struct {
	RequestID string `json:"request_id"`
	SkillName string `json:"skill_name"`
	OutputDir string `json:"output_dir"`
	Prompt    string `json:"prompt"`
	Force     bool   `json:"force"`
}

type statusRequest struct {
	RequestID string `json:"request_id"`
}

type resumeRequest struct {
	RequestID   string `json:"request_id"`
	Answer      string `json:"answer"`
	Instruction string `json:"instruction"`
}

type terminateRequest struct {
	RequestID string `json:"request_id"`
}

type taskStatus struct {
	RequestID  string   `json:"request_id"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	UpdatedAt  string   `json:"updated_at"`
	SkillName  string   `json:"skill_name"`
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
		StartedAt: v.StartedAt, FinishedAt: v.FinishedAt, SkillName: v.SkillName,
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
	PeriodicAddr string
	TaskPrefix   string
	RSA          protocol.RSAConnParam
	OutputRoot   string
	SkillsDir    string
	CodexBin     string
	Timeout      time.Duration
	MaxOutput    int
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
	timeout := envDuration("SKILL2API_CODEX_TIMEOUT_SECONDS", 15*time.Minute)
	if timeout <= 0 {
		return config{}, errors.New("SKILL2API_CODEX_TIMEOUT_SECONDS must be positive")
	}
	limit := envInt("SKILL2API_MAX_OUTPUT_BYTES", maxOutput)
	if limit < 1024 {
		limit = 1024
	}
	return config{PeriodicAddr: addr, TaskPrefix: strings.TrimSpace(os.Getenv("TASK_PREFIX")), RSA: protocol.RSAConnParam{Mode: mode, PrivateKeyPath: priv, ServerPublicKeyPath: pub}, OutputRoot: root, SkillsDir: skills, CodexBin: firstEnvDefault("SKILL2API_CODEX_BIN", "codex"), Timeout: timeout, MaxOutput: limit}, nil
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

func validateRequest(req generateRequest, c config) error {
	if !validID(req.RequestID) {
		return errors.New("request_id must contain only letters, digits, dot, underscore, or hyphen")
	}
	if strings.TrimSpace(req.SkillName) == "" || len(req.SkillName) > maxSkillName || filepath.Base(req.SkillName) != req.SkillName || strings.Contains(req.SkillName, "..") {
		return errors.New("invalid skill_name")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("prompt is required")
	}
	if _, err := resolveOutputDir(req.OutputDir, c.OutputRoot); err != nil {
		return err
	}
	skillRoot, err := filepath.Abs(c.SkillsDir)
	if err != nil {
		return err
	}
	if !within(skillRoot, filepath.Join(skillRoot, req.SkillName)) {
		return errors.New("invalid skill_name")
	}
	if _, err = os.Stat(filepath.Join(skillRoot, req.SkillName, "SKILL.md")); err != nil {
		return fmt.Errorf("skill not found: %w", err)
	}
	return nil
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
	lines := strings.Split(stdout, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "SKILL2API_INPUT_REQUIRED" {
			continue
		}
		if i+1 >= len(lines) {
			return inputRequest{}, true, errors.New("input request JSON is missing")
		}
		var request inputRequest
		if err := json.Unmarshal([]byte(strings.TrimSpace(lines[i+1])), &request); err != nil {
			return inputRequest{}, true, fmt.Errorf("invalid input request: %w", err)
		}
		if strings.TrimSpace(request.Question) == "" {
			return inputRequest{}, true, errors.New("input request question is empty")
		}
		return request, true, nil
	}
	return inputRequest{}, false, nil
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

func runCodexWithSessionCallback(ctx context.Context, c config, req generateRequest, skill []byte, onSessionID func(string)) (string, string, string, error) {
	prompt := fmt.Sprintf("%s\n\nUse the skill instructions below to implement the request as a reviewable Myna Sanic blueprint package. Write the blueprint, route entry, dependency notes, and README into the current working directory (%s). Do not modify files outside that directory.\n\nIf you need user information before continuing, do not guess. End your response with exactly two lines: SKILL2API_INPUT_REQUIRED and then a JSON object containing question and optional options.\n\nSkill instructions:\n%s", req.Prompt, req.OutputDir, string(skill))
	return runCodexCommand(ctx, c, req.OutputDir, false, redactionLines(req.Prompt), onSessionID, []string{"exec", "--sandbox", "workspace-write", "--cd", req.OutputDir, "--skip-git-repo-check", prompt})
}

func runCodexResume(ctx context.Context, c config, req taskStatus, answer, instruction string) (string, string, error) {
	prompt := "Continue the original task."
	if strings.TrimSpace(answer) != "" {
		prompt = fmt.Sprintf("User answer to your pending question: %s\n\nContinue the original task.", answer)
	}
	if strings.TrimSpace(instruction) != "" {
		prompt += fmt.Sprintf("\n\nAdditional user instruction: %s", instruction)
	}
	prompt += "\n\nIf another clarification is required, end your response with exactly two lines: SKILL2API_INPUT_REQUIRED and then a JSON object containing question and optional options."
	redactions := append(redactionLines(answer), redactionLines(instruction)...)
	stdout, stderr, _, err := runCodexCommand(ctx, c, req.OutputDir, true, redactions, nil, []string{"exec", "--sandbox", "workspace-write", "--cd", req.OutputDir, "--skip-git-repo-check", "resume", req.SessionID, prompt})
	return stdout, stderr, err
}

func runCodexCommand(ctx context.Context, c config, outputDir string, appendLogs bool, redactions []string, onSessionID func(string), args []string) (string, string, string, error) {
	stdoutOffset, stderrOffset, err := outputLogOffsets(outputDir)
	if err != nil {
		return "", "", "", err
	}
	if !appendLogs {
		stdoutOffset, stderrOffset = 0, 0
	}
	stdoutLog, stderrLog, err := openOutputLogs(outputDir, appendLogs)
	if err != nil {
		return "", "", "", err
	}
	cmd := exec.CommandContext(ctx, c.CodexBin, args...)
	log.Printf("event=skill2api_codex_start output_dir=%s codex_bin=%s timeout=%s", outputDir, c.CodexBin, c.Timeout)
	stdoutWriter := &redactingLogWriter{file: stdoutLog, redactions: redactions}
	stderrWriter := &redactingLogWriter{file: stderrLog, redactions: redactions, onSessionID: onSessionID}
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
		log.Printf("event=skill2api_codex_finish result=failed error=%q stdout_bytes=%d stderr_bytes=%d", err, len(stdout), len(stderr))
		return stdout, stderr, stderrWriter.sessionID, fmt.Errorf("codex exited: %w", err)
	}
	log.Printf("event=skill2api_codex_finish result=succeeded stdout_bytes=%d stderr_bytes=%d", len(stdout), len(stderr))
	return stdout, stderr, stderrWriter.sessionID, nil
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
	for _, redaction := range w.redactions {
		text = strings.ReplaceAll(text, redaction, "[redacted]")
	}
	text = redactSessionID(text)
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
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, e := filepath.Rel(dir, path)
			if e != nil {
				return e
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
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
	var stdout string
	skill, err := os.ReadFile(filepath.Join(c.SkillsDir, req.SkillName, "SKILL.md"))
	if err == nil {
		var sessionID string
		stdout, _, sessionID, err = runCodexWithSessionCallback(ctx, c, req, skill, func(sessionID string) {
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
		v.Status = "failed"
		v.Error = err.Error()
		v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = store.write(v)
		log.Printf("event=skill2api_state request_id=%s status=failed error=%q", req.RequestID, v.Error)
		return
	}
	if input, needed, parseErr := parseInputRequest(stdout); needed {
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
	var err error
	stdout, _, err := runCodexResume(ctx, c, v, req.Answer, req.Instruction)
	cancel()
	if current, readErr := store.read(req.RequestID); readErr == nil && current.Status == "terminated" {
		return
	}
	v.Question, v.Options, v.Phase = "", nil, ""
	if err != nil {
		v.Status = "failed"
		v.Error = err.Error()
	} else if input, needed, parseErr := parseInputRequest(stdout); needed {
		if parseErr != nil {
			v.Status, v.Error = "failed", parseErr.Error()
		} else {
			v.Status, v.Phase = "waiting_for_input", "clarification"
			v.Question, v.Options = input.Question, input.Options
			v.Error = ""
		}
	} else {
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
	outputDir, err := resolveOutputDir(req.OutputDir, c.OutputRoot)
	if err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	req.OutputDir = outputDir
	now := time.Now().UTC().Format(time.RFC3339Nano)
	v := taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: now, SkillName: req.SkillName, OutputDir: req.OutputDir}
	if err := store.create(v, req.Force); err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=duplicate error=%q", req.RequestID, err)
		_ = job.Fail()
		return
	}
	log.Printf("event=skill2api_state request_id=%s status=queued skill_name=%s output_dir=%s force=%t", req.RequestID, req.SkillName, req.OutputDir, req.Force)
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
	response, outputErr := statusWithLogOutput(v, c.MaxOutput)
	if outputErr != nil {
		log.Printf("event=skill2api_status request_id=%s result=log_read_failed error=%q", req.RequestID, outputErr)
	}
	doneJSON(job, response)
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
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "running"})
	go executeResume(store, manager, c, req, v)
}

func handleTerminate(job periodic.Job, store *statusStore, manager *taskManager) {
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
	}
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": v.Error})
}

func registerWorkerFuncs(worker *periodic.Worker, prefix string, store *statusStore, manager *taskManager, c config) error {
	if err := worker.AddFunc(withPrefix(prefix, generateFunc), func(job periodic.Job) { handleGenerate(job, store, manager, c) }); err != nil {
		return err
	}
	if err := worker.AddFunc(withPrefix(prefix, statusFunc), func(job periodic.Job) { handleStatus(job, store, c) }); err != nil {
		return err
	}
	if err := worker.AddFunc(withPrefix(prefix, resumeFunc), func(job periodic.Job) { handleResume(job, store, manager, c) }); err != nil {
		return err
	}
	if err := worker.AddFunc(withPrefix(prefix, terminateFunc), func(job periodic.Job) { handleTerminate(job, store, manager) }); err != nil {
		return err
	}
	return nil
}
func connectPeriodic(client *periodic.Client, addr string, rsa protocol.RSAConnParam) error {
	return client.Connect(addr, rsa)
}

func main() {
	c, err := newConfig()
	if err != nil {
		panic(err)
	}
	store := &statusStore{root: c.OutputRoot}
	manager := newTaskManager()
	if err := os.MkdirAll(c.OutputRoot, 0750); err != nil {
		panic(err)
	}
	if err := store.recoverRunning(); err != nil {
		panic(err)
	}
	for {
		worker := periodic.NewWorker(8)
		worker.SetIOTimeout(30*time.Second, 10*time.Second)
		if err := connectPeriodic(&worker.Client, c.PeriodicAddr, c.RSA); err != nil {
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		if err := registerWorkerFuncs(worker, c.TaskPrefix, store, manager, c); err != nil {
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		worker.Work()
		worker.Close()
		time.Sleep(time.Second)
	}
}
