package main

import (
	"errors"
	"fmt"
	"github.com/Lupino/go-periodic/protocol"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func newConfig() (config, error) {
	addr := strings.TrimRight(strings.TrimSpace(os.Getenv("PERIODIC_PORT")), "/")
	if addr == "" {
		return config{}, errors.New("PERIODIC_PORT is required")
	}
	nodeID := strings.TrimSpace(os.Getenv("SKILL2API_NODE_ID"))
	if !validID(nodeID) {
		return config{}, errors.New("SKILL2API_NODE_ID must be a safe non-empty identifier")
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
	uploadTimeout := envDuration("SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS", fileUploadTimeout)
	if uploadTimeout <= 0 {
		return config{}, errors.New("SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS must be positive")
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
	return config{PeriodicAddr: addr, TaskPrefix: strings.TrimSpace(os.Getenv("TASK_PREFIX")), NodeID: nodeID, RSA: protocol.RSAConnParam{Mode: mode, PrivateKeyPath: priv, ServerPublicKeyPath: pub}, OutputRoot: root, SkillsDir: skills, CodexBin: firstEnvDefault("SKILL2API_CODEX_BIN", "codex"), CodexDocker: dockerEnabled, CodexDockerBin: firstEnvDefault("SKILL2API_CODEX_DOCKER_BIN", "docker"), CodexDockerImage: firstEnvDefault("SKILL2API_CODEX_DOCKER_IMAGE", "lupino/sandbox-runner:latest"), CodexDockerOptDir: optDir, CodexNoProxy: noProxy, Debug: debug, UploadBaseURL: uploadBaseURL, UploadTimeout: uploadTimeout, Timeout: timeout, MaxOutput: limit, MaxFileBytes: fileLimit}, nil
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

func nodeFunctionName(c config, fn string) string {
	return withPrefix(c.TaskPrefix, fn) + ":" + c.NodeID
}
