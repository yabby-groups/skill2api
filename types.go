package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Lupino/go-periodic/protocol"
	"path/filepath"
	"sync"
	"time"
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
	dockerSkillsDir        = "/skill2api/skills"
	dockerCodexConfig      = `sandbox_mode = "danger-full-access"
model_provider = "huabot"
model = "gpt-6-luna"

[model_providers.huabot]
name = "Huabot API"
base_url = "https://huabot.com/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
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

type generateResponse struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	NodeID    string `json:"node_id"`
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
	retryAt   time.Time
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
	NodeID            string
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
	UploadTimeout     time.Duration
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
