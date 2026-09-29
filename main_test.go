package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
		{RequestID: "ok", SkillName: "demo", OutputDir: base.OutputDir, Prompt: base.Prompt, Model: " model"},
		{RequestID: "ok", SkillName: "demo", OutputDir: base.OutputDir, Prompt: base.Prompt, Environment: map[string]string{"NOT-VALID": "value"}},
		{RequestID: "ok", SkillName: "demo", OutputDir: base.OutputDir, Prompt: base.Prompt, Environment: map[string]string{"VALID": "value\x00"}},
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
	if err := store.write(taskStatus{RequestID: "r2", Status: "running", StartedAt: "started", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.recoverRunning(); err != nil {
		t.Fatal(err)
	}
	resumable, err := store.read("r2")
	if err != nil || resumable.Status != "interrupted" || resumable.Error != "worker interrupted" || resumable.FinishedAt != "" {
		t.Fatalf("unexpected resumable status: %#v err=%v", resumable, err)
	}
	if err := store.create(taskStatus{RequestID: "r2", Status: "queued"}, true); err == nil {
		t.Fatal("force replaced an interrupted request")
	}
	claimed, err := store.claimInterrupted("r2")
	if err != nil || claimed.Status != "running" || claimed.StartedAt == "started" {
		t.Fatalf("interrupted request was not claimed: %#v err=%v", claimed, err)
	}
	if err := store.write(taskStatus{RequestID: "r3", Status: "succeeded", StartedAt: "completed-attempt", FinishedAt: "finished", SessionID: "session-3"}); err != nil {
		t.Fatal(err)
	}
	continued, err := store.claimSucceeded("r3")
	if err != nil || continued.Status != "running" || continued.StartedAt == "completed-attempt" || continued.FinishedAt != "" || continued.SessionID != "session-3" {
		t.Fatalf("completed request was not claimed for continuation: %#v err=%v", continued, err)
	}
	for _, state := range []string{"failed", "terminated"} {
		id := "r-" + state
		if err := store.write(taskStatus{RequestID: id, Status: state, StartedAt: "previous", FinishedAt: "finished", SessionID: "session-" + state}); err != nil {
			t.Fatal(err)
		}
		var resumed taskStatus
		if state == "failed" {
			resumed, err = store.claimFailed(id)
		} else {
			resumed, err = store.claimTerminated(id)
		}
		if err != nil || resumed.Status != "running" || resumed.FinishedAt != "" || resumed.SessionID == "" {
			t.Fatalf("%s request was not claimed for continuation: %#v err=%v", state, resumed, err)
		}
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

func TestCodexPromptPreservesCallerPromptAndSkillVerbatim(t *testing.T) {
	prompt := "Replace the product in this video."
	skill := []byte("---\nname: hypit\n---\nUse the supplied references.")
	got := codexPrompt(prompt, skill)
	want := prompt + "\n\n" + string(skill)
	if got != want {
		t.Fatalf("Codex prompt was rewritten:\nwant %q\n got %q", want, got)
	}
	if strings.Contains(got, "Myna Sanic") || strings.Contains(got, "blueprint package") {
		t.Fatalf("Codex prompt contains an unrelated task wrapper: %q", got)
	}
}

func TestWithoutProxyEnvRemovesProxyVariablesOnly(t *testing.T) {
	got := withoutProxyEnv([]string{"PATH=/bin", "http_proxy=http://proxy", "HTTPS_PROXY=http://proxy", "ALL_PROXY=socks5://proxy", "NO_PROXY=localhost", "KEEP=value"})
	if strings.Join(got, "|") != "PATH=/bin|KEEP=value" {
		t.Fatalf("unexpected direct environment: %#v", got)
	}
}

func TestCodexExecArgsSelectsSandboxAndNetworkMode(t *testing.T) {
	want := []string{"exec", "-c", "sandbox_workspace_write.network_access=true", "--model", "gpt-6-sol", "--sandbox", "workspace-write", "--cd", "/tmp/request-1", "--skip-git-repo-check", "prompt"}
	if got := codexExecArgs(config{CodexNetworkAccess: true}, "gpt-6-sol", "/tmp/request-1", []string{"prompt"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("native args = %#v, want %#v", got, want)
	}
	want = []string{"exec", "--model", "gpt-6-sol", "--sandbox", "danger-full-access", "--cd", "/workspace", "--skip-git-repo-check", "prompt"}
	if got := codexExecArgs(config{CodexDocker: true, CodexNetworkAccess: true}, "gpt-6-sol", "/tmp/request-1", []string{"prompt"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("docker args = %#v, want %#v", got, want)
	}
}

func TestCodexEnvironmentOverlaysAndFiltersProxy(t *testing.T) {
	got := codexEnvironment([]string{"PATH=/bin", "KEEP=old", "HTTP_PROXY=old-proxy"}, map[string]string{"KEEP": "new", "TOKEN": "secret", "http_proxy": "request-proxy"}, true)
	want := []string{"PATH=/bin", "KEEP=new", "TOKEN=secret"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged environment = %#v, want %#v", got, want)
	}
}

func TestDockerCodexCommandIsolatedAndReceivesExplicitEnvironment(t *testing.T) {
	c, root := testConfig(t)
	c.CodexDocker, c.CodexDockerBin, c.CodexDockerImage = true, "docker-test", "example/sandbox:tag"
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	cmd, executable, err := newCodexCommand(context.Background(), c, "request-1", outputDir, map[string]string{"SANDBOX_AI_KEY": "client-key", "OTHER_TOKEN": "other-value"}, codexExecArgs(c, "", outputDir, []string{"prompt"}))
	if err != nil {
		t.Fatal(err)
	}
	if executable != "docker-test" || cmd.Path != "docker-test" {
		t.Fatalf("unexpected executable: %q (%q)", executable, cmd.Path)
	}
	joined := strings.Join(cmd.Args, "\n")
	for _, want := range []string{"run", "--rm", "--init", "--network\nnone", "dst=/workspace", "dst=/home/ubuntu", "--workdir\n/workspace", "--env\nSANDBOX_AI_KEY", "--env\nOTHER_TOKEN", "example/sandbox:tag", "--sandbox\ndanger-full-access", "--cd\n/workspace"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker command missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "client-key") || strings.Contains(joined, "other-value") {
		t.Fatalf("docker command exposed environment values: %q", joined)
	}
	if !strings.Contains(joined, codexHomeDir(c, "request-1")) {
		t.Fatalf("docker command did not mount the persistent home: %q", joined)
	}
	if _, err := os.Stat(codexHomeDir(c, "request-1")); err != nil {
		t.Fatalf("persistent home was not created: %v", err)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "SANDBOX_AI_KEY=client-key") {
		t.Fatalf("client key was not forwarded to Docker: %#v", cmd.Env)
	}
	c.CodexNetworkAccess = true
	cmd, _, err = newCodexCommand(context.Background(), c, "request-1", outputDir, nil, codexExecArgs(c, "", outputDir, []string{"prompt"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Args, "\n"), "--network\nnone") {
		t.Fatalf("network-enabled Docker command remained isolated: %#v", cmd.Args)
	}
}

func TestDockerCodexEnvironmentFallsBackToWorkerKey(t *testing.T) {
	t.Setenv("SANDBOX_AI_KEY", "worker-key")
	values := codexRequestEnvironment(config{CodexDocker: true}, map[string]string{"OTHER": "value"})
	if values["SANDBOX_AI_KEY"] != "worker-key" {
		t.Fatalf("worker fallback was not used: %#v", values)
	}
	values = codexRequestEnvironment(config{CodexDocker: true}, map[string]string{"SANDBOX_AI_KEY": "client-key"})
	if values["SANDBOX_AI_KEY"] != "client-key" {
		t.Fatalf("client key did not take precedence: %#v", values)
	}
}

func TestDockerCodexHomesAreIsolatedPerRequest(t *testing.T) {
	c, root := testConfig(t)
	c.CodexDocker = true
	outputDir := filepath.Join(root, "shared-output")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	first, _, err := newCodexCommand(context.Background(), c, "request-1", outputDir, nil, codexExecArgs(c, "", outputDir, []string{"prompt"}))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := newCodexCommand(context.Background(), c, "request-2", outputDir, nil, codexExecArgs(c, "", outputDir, []string{"prompt"}))
	if err != nil {
		t.Fatal(err)
	}
	firstHome, secondHome := codexHomeDir(c, "request-1"), codexHomeDir(c, "request-2")
	if firstHome == secondHome || !strings.Contains(strings.Join(first.Args, "\n"), firstHome) || !strings.Contains(strings.Join(second.Args, "\n"), secondHome) {
		t.Fatalf("Docker requests shared a Codex home: first=%q second=%q", first.Args, second.Args)
	}
}

func TestEnvironmentValuesAreRedacted(t *testing.T) {
	got := environmentRedactions(map[string]string{"TOKEN": "first\nsecond", "EMPTY": "", "PREFIX": "first-value"})
	if !reflect.DeepEqual(got, []string{"first-value", "second", "first"}) {
		t.Fatalf("environment redactions = %#v", got)
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

func TestUpdateSessionIDOnlyUpdatesCurrentRun(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	if err := store.write(taskStatus{RequestID: "r1", Status: "running", StartedAt: "attempt-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.updateSessionID("r1", "attempt-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	got, err := store.read("r1")
	if err != nil || got.SessionID != "session-1" {
		t.Fatalf("session ID was not persisted: %#v err=%v", got, err)
	}
	if err := store.updateSessionID("r1", "another-attempt", "session-2"); err != nil {
		t.Fatal(err)
	}
	got, err = store.read("r1")
	if err != nil || got.SessionID != "session-1" {
		t.Fatalf("stale execution overwrote session: %#v err=%v", got, err)
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

func TestPublicStatusExposesModelButNotSession(t *testing.T) {
	data, err := json.Marshal(publicStatus(taskStatus{RequestID: "r1", Status: "waiting_for_input", Model: "gpt-6-sol", SessionID: "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "session_id") {
		t.Fatalf("session leaked in public status: %s", data)
	}
	if !strings.Contains(string(data), `"model":"gpt-6-sol"`) {
		t.Fatalf("model missing from public status: %s", data)
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
	stdoutTail, stderrTail, err := runCodexResume(context.Background(), c, taskStatus{OutputDir: out, SessionID: "session-1"}, "answer", "", nil)
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

func TestRunCodexPersistsSessionBeforeCompletion(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'session id: session-1\\n' >&2\nsleep 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	sessions := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := runCodexWithSessionCallback(context.Background(), c, generateRequest{OutputDir: out}, nil, func(id string) { sessions <- id })
		done <- err
	}()
	select {
	case id := <-sessions:
		if id != "session-1" {
			t.Fatalf("unexpected session ID: %q", id)
		}
	case err := <-done:
		t.Fatalf("Codex completed before session persistence: %v", err)
	case <-time.After(time.Second):
		t.Fatal("session ID was not reported while Codex ran")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunCodexRedactsPromptAndResumeAnswerFromLogs(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'keep this output'\ncase \"$*\" in *resume*) printf 'secret answer value\\nsecret instruction value' >&2 ;; *) printf 'secret prompt value' >&2 ;; esac\nprintf '\\nsession id: private-session-1\\n' >&2\n"), 0700); err != nil {
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
	if _, _, err := runCodexResume(context.Background(), c, taskStatus{OutputDir: out, SessionID: "session-1"}, "secret answer value", "secret instruction value", nil); err != nil {
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
	if strings.Contains(string(stderr), "secret prompt value") || strings.Contains(string(stderr), "secret answer value") || strings.Contains(string(stderr), "secret instruction value") || strings.Contains(string(stderr), "private-session-1") {
		t.Fatalf("sensitive input was written to stderr log: %q", stderr)
	}
	if strings.Count(string(stderr), "[redacted]") != 5 {
		t.Fatalf("expected inputs and session IDs to be redacted: %q", stderr)
	}
}

func TestRunCodexPassesEnvironmentAndModelWithoutLeakingValue(t *testing.T) {
	c, root := testConfig(t)
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s|%s\\n' \"$SKILL2API_TEST_SECRET\" \"$*\"\nprintf '%s\\n' \"$SKILL2API_TEST_SECRET\" >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	c.MaxOutput = 4096
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	req := generateRequest{OutputDir: out, Prompt: "ordinary prompt", Model: "gpt-6-sol", Environment: map[string]string{"SKILL2API_TEST_SECRET": "private-value"}}
	stdout, stderr, err := runCodex(context.Background(), c, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "private-value") || strings.Contains(stderr, "private-value") {
		t.Fatalf("environment value leaked in returned output: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout, "--model gpt-6-sol") {
		t.Fatalf("model argument missing from initial run: %q", stdout)
	}
	stdout, stderr, err = runCodexResume(context.Background(), c, taskStatus{OutputDir: out, SessionID: "session-1", Model: "gpt-6-sol"}, "answer", "", map[string]string{"SKILL2API_TEST_SECRET": "private-value"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "private-value") || strings.Contains(stderr, "private-value") || !strings.Contains(stdout, "--model gpt-6-sol") {
		t.Fatalf("resume did not preserve model or redact environment: stdout=%q stderr=%q", stdout, stderr)
	}
	stderrLog, err := os.ReadFile(filepath.Join(out, stderrLogName))
	if err != nil || strings.Contains(string(stderrLog), "private-value") {
		t.Fatalf("environment value leaked in stderr log: %q err=%v", stderrLog, err)
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
	if c.Timeout != 6*time.Hour {
		t.Fatalf("default Codex timeout = %s, want %s", c.Timeout, 6*time.Hour)
	}
	if c.CodexNoProxy || c.CodexNetworkAccess || c.CodexDocker || c.CodexDockerBin != "docker" || c.CodexDockerImage != "lupino/sandbox-runner:latest" {
		t.Fatalf("direct provider settings should default to false: %#v", c)
	}
	if withPrefix(c.TaskPrefix, generateFunc) != "generation-skill2api_generate" || statusFunc != "skill2api_status" {
		t.Fatal("periodic function naming contract changed")
	}
}

func TestConfigCodexTimeoutOverride(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_CODEX_TIMEOUT_SECONDS", "28800")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 8*time.Hour {
		t.Fatalf("Codex timeout override = %s, want %s", c.Timeout, 8*time.Hour)
	}
}

func TestConfigCodexNetworkAccess(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
	t.Setenv("SKILL2API_CODEX_NETWORK_ACCESS", "true")
	c, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CodexNetworkAccess {
		t.Fatal("network access was not enabled")
	}

	t.Setenv("SKILL2API_CODEX_NETWORK_ACCESS", "invalid")
	if _, err := newConfig(); err == nil || !strings.Contains(err.Error(), "SKILL2API_CODEX_NETWORK_ACCESS") {
		t.Fatalf("invalid network-access setting error = %v", err)
	}
}

func TestConfigCodexDocker(t *testing.T) {
	t.Setenv("PERIODIC_PORT", "tcp://periodic:5000")
	t.Setenv("PERIODIC_RSA_MODE", "0")
	t.Setenv("SKILL2API_OUTPUT_ROOT", t.TempDir())
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
