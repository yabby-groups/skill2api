package main

import (
	"bytes"
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
	base := generateRequest{RequestID: "request-1", SkillName: "demo", OutputDir: "request-1", Prompt: "Generate a test API"}
	if err := validateRequest(base, c); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for _, req := range []generateRequest{
		{RequestID: "../escape", SkillName: "demo", OutputDir: base.OutputDir, Prompt: base.Prompt},
		{RequestID: "ok", SkillName: "../demo", OutputDir: base.OutputDir, Prompt: base.Prompt},
		{RequestID: "ok", SkillName: "demo", OutputDir: "../outside", Prompt: base.Prompt},
		{RequestID: "ok", SkillName: "demo", OutputDir: filepath.Join(root, "request-1"), Prompt: base.Prompt},
	} {
		if err := validateRequest(req, c); err == nil {
			t.Fatalf("request %#v was accepted", req)
		}
	}
	resolved, err := resolveOutputDir(base.OutputDir, c.OutputRoot)
	if err != nil || resolved != filepath.Join(root, "request-1") {
		t.Fatalf("relative output directory was not resolved against root: %q err=%v", resolved, err)
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

func TestTerminateTransitionsActiveStates(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	for _, state := range []string{"queued", "running", "waiting_for_input"} {
		id := "request-" + state
		if err := store.write(taskStatus{RequestID: id, Status: state, Question: "question", Phase: "clarification"}); err != nil {
			t.Fatal(err)
		}
		got, err := store.terminate(id)
		if err != nil || got.Status != "terminated" || got.Error != "terminated by user" || got.FinishedAt == "" {
			t.Fatalf("terminate %s: %#v err=%v", state, got, err)
		}
		if got.Question != "" || got.Phase != "" {
			t.Fatalf("interactive fields were retained: %#v", got)
		}
		again, err := store.terminate(id)
		if err != nil || again.Status != "terminated" {
			t.Fatalf("repeated terminate: %#v err=%v", again, err)
		}
	}
}

func TestWriteFromRunningDoesNotOverrideTermination(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	running := taskStatus{RequestID: "r1", Status: "running", StartedAt: "started"}
	if err := store.write(running); err != nil {
		t.Fatal(err)
	}
	if _, err := store.terminate(running.RequestID); err != nil {
		t.Fatal(err)
	}
	running.Status = "succeeded"
	written, err := store.writeFromRunning(running)
	if err != nil || written {
		t.Fatalf("terminated request was overwritten: written=%t err=%v", written, err)
	}
	got, err := store.read(running.RequestID)
	if err != nil || got.Status != "terminated" {
		t.Fatalf("unexpected stored status: %#v err=%v", got, err)
	}
}

func TestTaskManagerCancel(t *testing.T) {
	manager := newTaskManager()
	done := make(chan struct{})
	manager.register("r1", func() { close(done) })
	manager.cancel("r1")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel function was not called")
	}
	manager.unregister("r1")
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

func TestRunCodexWritesOutputLogsAndReadsTail(t *testing.T) {
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
	if stdout != "[earlier output truncated]\nabcdefghij" || stderr != "err-output" {
		t.Fatalf("unexpected summaries: stdout=%q stderr=%q", stdout, stderr)
	}
	stdoutLog, err := os.ReadFile(filepath.Join(out, stdoutLogName))
	if err != nil || string(stdoutLog) != "1234567890abcdefghij" {
		t.Fatalf("unexpected stdout log: %q err=%v", stdoutLog, err)
	}
	stderrLog, err := os.ReadFile(filepath.Join(out, stderrLogName))
	if err != nil || string(stderrLog) != "err-output" {
		t.Fatalf("unexpected stderr log: %q err=%v", stderrLog, err)
	}
}

func TestRunCodexExitAndTimeout(t *testing.T) {
	c, root := testConfig(t)
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(root, "fail")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = fail
	if _, _, err := runCodex(context.Background(), c, generateRequest{OutputDir: out}, nil); err == nil {
		t.Fatal("nonzero codex exit was accepted")
	}
	sleep := filepath.Join(root, "sleep")
	if err := os.WriteFile(sleep, []byte("#!/bin/sh\nsleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = sleep
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := runCodex(ctx, c, generateRequest{OutputDir: out}, nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestRunCodexResumeAppendsLogs(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'out'\nprintf 'err' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCodex(context.Background(), c, generateRequest{OutputDir: out}, nil); err != nil {
		t.Fatal(err)
	}
	stdoutTail, stderrTail, err := runCodexResume(context.Background(), c, taskStatus{OutputDir: out, SessionID: "session-1"}, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if stdoutTail != "out" || stderrTail != "err" {
		t.Fatalf("resume output included a prior execution: stdout=%q stderr=%q", stdoutTail, stderrTail)
	}
	stdout, err := os.ReadFile(filepath.Join(out, stdoutLogName))
	if err != nil || string(stdout) != "outout" {
		t.Fatalf("unexpected appended stdout log: %q err=%v", stdout, err)
	}
	stderr, err := os.ReadFile(filepath.Join(out, stderrLogName))
	if err != nil || string(stderr) != "errerr" {
		t.Fatalf("unexpected appended stderr log: %q err=%v", stderr, err)
	}
}

func TestRunCodexWritesLogsBeforeCompletion(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'started\\n'\nsleep 1\nprintf 'finished\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := runCodex(context.Background(), c, generateRequest{OutputDir: out}, nil)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		output, err := os.ReadFile(filepath.Join(out, stdoutLogName))
		if err == nil && string(output) == "started\n" {
			select {
			case err := <-done:
				t.Fatalf("Codex finished before running-log assertion: %v", err)
			default:
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdout log did not become available while Codex ran: output=%q err=%v", output, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunCodexRedactsPromptAndResumeAnswerFromLogs(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'keep this output'\ncase \"$*\" in *resume*) printf 'secret answer value' >&2 ;; *) printf 'secret prompt value' >&2 ;; esac\nprintf '\\nsession id: private-session-1\\n' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	if _, _, sessionID, err := runCodexWithSession(context.Background(), c, generateRequest{OutputDir: out, Prompt: "secret prompt value"}, nil); err != nil {
		t.Fatal(err)
	} else if sessionID != "private-session-1" {
		t.Fatalf("unexpected private session ID: %q", sessionID)
	}
	if _, _, err := runCodexResume(context.Background(), c, taskStatus{OutputDir: out, SessionID: "session-1"}, "secret answer value"); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.ReadFile(filepath.Join(out, stdoutLogName))
	if err != nil || !strings.Contains(string(stdout), "keep this output") {
		t.Fatalf("unexpected stdout log: %q err=%v", stdout, err)
	}
	stderr, err := os.ReadFile(filepath.Join(out, stderrLogName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stderr), "secret prompt value") || strings.Contains(string(stderr), "secret answer value") || strings.Contains(string(stderr), "private-session-1") {
		t.Fatalf("sensitive input was written to stderr log: %q", stderr)
	}
	if strings.Count(string(stderr), "[redacted]") != 4 {
		t.Fatalf("expected inputs and session IDs to be redacted: %q", stderr)
	}
}

func TestStatusWithLogOutputReadsLogsWithoutWritingStatus(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, stdoutLogName), []byte("1234567890abcdefghij"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, stderrLogName), []byte("stderr"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &statusStore{root: root}
	v := taskStatus{RequestID: "request-1", Status: "running", OutputDir: outputDir}
	if err := store.write(v); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.path(v.RequestID))
	if err != nil {
		t.Fatal(err)
	}
	response, err := statusWithLogOutput(v, 10)
	if err != nil {
		t.Fatal(err)
	}
	if response.Stdout != "[earlier output truncated]\nabcdefghij" || response.Stderr != "stderr" {
		t.Fatalf("unexpected status output: %#v", response)
	}
	after, err := os.ReadFile(store.path(v.RequestID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("status query unexpectedly rewrote status.json")
	}
}

func TestConfigPlainModeUsesPeriodicSettings(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("TASK_PREFIX", "generation-")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("PERIODIC_RSA_PRIVATE_KEY_PATH", "")
	t.Setenv("PERIODIC_RSA_PUBLIC_KEY_PATH", "")
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
