package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lupino/go-periodic/protocol"
)

func TestConfigPlainModeUsesPeriodicSettings(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("TASK_PREFIX", "generation-")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("PERIODIC_RSA_PRIVATE_KEY_PATH", "")
	t.Setenv("PERIODIC_RSA_PUBLIC_KEY_PATH", "")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.RSA.Mode != protocol.ModePlain || c.TaskPrefix != "generation-" || c.NodeID != "node-a" || c.PeriodicAddr == "" {
		t.Fatalf("unexpected config: %#v", c)
	}
	if c.Timeout != 6*time.Hour {
		t.Fatalf("default Codex timeout = %s, want %s", c.Timeout, 6*time.Hour)
	}
	if c.UploadTimeout != fileUploadTimeout {
		t.Fatalf("default upload timeout = %s, want %s", c.UploadTimeout, fileUploadTimeout)
	}
	if c.CodexNoProxy || c.CodexDocker || c.Debug || c.CodexDockerBin != "docker" || c.CodexDockerImage != "lupino/sandbox-runner:latest" {
		t.Fatalf("direct provider settings should default to false: %#v", c)
	}
	if withPrefix(c.TaskPrefix, generateFunc) != "generation-skill2api_generate" || nodeFunctionName(c, statusFunc) != "generation-skill2api_status:node-a" || nodeFunctionName(c, cleanupFunc) != "generation-skill2api_cleanup:node-a" {
		t.Fatal("periodic function naming contract changed")
	}
}

func TestConfigRequiresSafeNodeID(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_NODE_ID") {
		t.Fatalf("missing node ID error = %v", err)
	}
	t.Setenv("SKILL2API_NODE_ID", "node:a")
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_NODE_ID") {
		t.Fatalf("unsafe node ID error = %v", err)
	}
}

func TestConfigDebugOverride(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	t.Setenv("SKILL2API_DEBUG", "true")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Debug {
		t.Fatal("debug setting was not enabled")
	}
	t.Setenv("SKILL2API_DEBUG", "invalid")
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_DEBUG") {
		t.Fatalf("invalid debug setting error = %v", err)
	}
}

func TestConfigCodexTimeoutOverride(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	t.Setenv("SKILL2API_CODEX_TIMEOUT_SECONDS", "28800")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 8*time.Hour {
		t.Fatalf("Codex timeout override = %s, want %s", c.Timeout, 8*time.Hour)
	}
}

func TestConfigUploadTimeoutOverride(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	t.Setenv("SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS", "180")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.UploadTimeout != 3*time.Minute {
		t.Fatalf("upload timeout override = %s, want %s", c.UploadTimeout, 3*time.Minute)
	}
	t.Setenv("SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS", "0")
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS") {
		t.Fatalf("invalid upload timeout error = %v", err)
	}
}

func TestConfigCodexDocker(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	t.Setenv("SKILL2API_CODEX_DOCKER", "true")
	t.Setenv("SKILL2API_CODEX_DOCKER_BIN", "podman")
	t.Setenv("SKILL2API_CODEX_DOCKER_IMAGE", "registry.example/sandbox:1")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CodexDocker || c.CodexDockerBin != "podman" || c.CodexDockerImage != "registry.example/sandbox:1" {
		t.Fatalf("unexpected Docker config: %#v", c)
	}
	t.Setenv("SKILL2API_CODEX_DOCKER", "invalid")
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_CODEX_DOCKER") {
		t.Fatalf("invalid Docker setting error = %v", err)
	}
}

func TestConfigCodexDockerOptDir(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_NODE_ID", "node-a")
	t.Setenv("SKILL2API_CODEX_DOCKER", "true")
	optDir := t.TempDir()
	t.Setenv("SKILL2API_CODEX_DOCKER_OPT_DIR", optDir)
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.CodexDockerOptDir != optDir {
		t.Fatalf("Docker opt directory = %q, want %q", c.CodexDockerOptDir, optDir)
	}
	t.Setenv("SKILL2API_CODEX_DOCKER_OPT_DIR", filepath.Join(optDir, "missing"))
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_CODEX_DOCKER_OPT_DIR") {
		t.Fatalf("invalid Docker opt directory error = %v", err)
	}
}
