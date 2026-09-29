package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lupino/go-periodic/protocol"
)

func testConfig(t *testing.T) (config, string) {
	t.Helper()
	root := t.TempDir()
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(filepath.Join(skills, "demo"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "demo", "SKILL.md"), []byte("make an API"), 0600); err != nil {
		t.Fatal(err)
	}
	return config{OutputRoot: root, SkillsDir: skills, CodexBin: "codex", Timeout: time.Second, MaxOutput: 16}, root
}

func TestValidateRequestRejectsTraversalAndOutsideOutput(t *testing.T) {
	c, root := testConfig(t)
	base := generateRequest{RequestID: "request-1", SkillName: "demo", OutputDir: filepath.Join(root, "request-1"), Prompt: "Generate a test API"}
	if err := validateRequest(base, c); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for _, req := range []generateRequest{
		{RequestID: "../escape", SkillName: "demo", OutputDir: base.OutputDir, Prompt: base.Prompt},
		{RequestID: "ok", SkillName: "../demo", OutputDir: base.OutputDir, Prompt: base.Prompt},
		{RequestID: "ok", SkillName: "demo", OutputDir: filepath.Join(root, "..", "outside"), Prompt: base.Prompt},
	} {
		if err := validateRequest(req, c); err == nil {
			t.Fatalf("request %#v was accepted", req)
		}
	}
}

func TestStatusStoreAtomicWriteAndRecovery(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	v := taskStatus{RequestID: "r1", Status: "running", CreatedAt: "created", SkillName: "demo", OutputDir: filepath.Join(store.root, "r1")}
	if err := store.write(v); err != nil {
		t.Fatal(err)
	}
	if err := store.recoverRunning(); err != nil {
		t.Fatal(err)
	}
	got, err := store.read("r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "worker interrupted" || got.FinishedAt == "" {
		t.Fatalf("unexpected recovered status: %#v", got)
	}
	if err := store.create(taskStatus{RequestID: "r1", Status: "queued"}, false); err == nil {
		t.Fatal("duplicate request was accepted")
	}
	if err := store.create(taskStatus{RequestID: "r1", Status: "queued"}, true); err != nil {
		t.Fatalf("force replacement failed: %v", err)
	}
}

func TestParseInputRequest(t *testing.T) {
	request, needed, err := parseInputRequest("progress\nSKILL2API_INPUT_REQUIRED\n{\"question\":\"Pick one\",\"options\":[\"a\",\"b\"]}\n")
	if err != nil || !needed || request.Question != "Pick one" || len(request.Options) != 2 {
		t.Fatalf("unexpected input request: %#v needed=%t err=%v", request, needed, err)
	}
	if _, needed, err := parseInputRequest("no interaction"); err != nil || needed {
		t.Fatalf("ordinary output was treated as input request: needed=%t err=%v", needed, err)
	}
}

func TestClaimWaitingIsAtomic(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	if err := store.write(taskStatus{RequestID: "r1", Status: "waiting_for_input", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.claimWaiting("r1")
	if err != nil || got.Status != "running" {
		t.Fatalf("claim waiting failed: %#v err=%v", got, err)
	}
	if _, err := store.claimWaiting("r1"); err == nil {
		t.Fatal("waiting request was claimed twice")
	}
}

func TestPublicStatusDoesNotExposeSession(t *testing.T) {
	data, err := json.Marshal(publicStatus(taskStatus{RequestID: "r1", Status: "waiting_for_input", SessionID: "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "session_id") {
		t.Fatalf("session leaked in public status: %s", data)
	}
}

func TestRunCodexSuccessAndOutputTruncation(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '1234567890abcdefghij'\nprintf 'err-output' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin, c.MaxOutput = bin, 10
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCodex(context.Background(), c, generateRequest{OutputDir: out}, []byte("skill"))
	if err != nil {
		t.Fatalf("runCodex() error = %v", err)
	}
	if !strings.Contains(stdout, "[output truncated]") || !strings.Contains(stderr, "err-output") {
		t.Fatalf("unexpected summaries: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestRunCodexExitAndTimeout(t *testing.T) {
	c, root := testConfig(t)
	fail := filepath.Join(root, "fail")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = fail
	if _, _, err := runCodex(context.Background(), c, generateRequest{}, nil); err == nil {
		t.Fatal("nonzero codex exit was accepted")
	}
	sleep := filepath.Join(root, "sleep")
	if err := os.WriteFile(sleep, []byte("#!/bin/sh\nsleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = sleep
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := runCodex(ctx, c, generateRequest{}, nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestConfigPlainModeUsesGenerationSettings(t *testing.T) {
	t.Setenv("GENERATION_PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("GENERATION_TASK_PREFIX", "generation-")
	t.Setenv("GENERATION_RSA_MODE", "0")
	t.Setenv("GENERATION_PRIVATE_KEY", "")
	t.Setenv("GENERATION_SERVER_PUBLIC_KEY", "")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.RSA.Mode != protocol.ModePlain || c.TaskPrefix != "generation-" || c.PeriodicAddr == "" {
		t.Fatalf("unexpected config: %#v", c)
	}
	if withPrefix(c.TaskPrefix, generateFunc) != "generation-skill2api_generate" || statusFunc != "skill2api_status" {
		t.Fatal("periodic function naming contract changed")
	}
}
