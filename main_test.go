package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	return config{OutputRoot: root, SkillsDir: skills, CodexBin: "codex", NodeID: "node-a", Timeout: time.Second, MaxOutput: 16, MaxFileBytes: 1024}, root
}

func TestReadTaskFileReturnsBinaryAndAllowsInternalFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.bin"), []byte{0, 1, 2, 255}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "status.json"), []byte(`{"status":"running"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		"image.bin":   {0, 1, 2, 255},
		"status.json": []byte(`{"status":"running"}`),
	} {
		got, err := readTaskFile(root, path, 1024)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("readTaskFile(%q) = %v, %v; want %v, nil", path, got, err, want)
		}
	}
}

func TestReadTaskFileRejectsUnsafeAndOversizedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ".", "..", "../escape", filepath.Join(string(filepath.Separator), "tmp", "escape")} {
		if _, err := readTaskFile(root, path, 1024); err == nil {
			t.Fatalf("unsafe path %q was accepted", path)
		}
	}
	if _, err := readTaskFile(root, "large.bin", 4); err == nil {
		t.Fatal("oversized file was accepted")
	}
	if err := os.Symlink(os.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskFile(root, "outside", 1024); err == nil {
		t.Fatal("symlink outside the output directory was accepted")
	}
}

func TestListFilesOrdersByModificationTime(t *testing.T) {
	root := t.TempDir()
	oldFile := filepath.Join(root, "assets", "source.mp4")
	newFile := filepath.Join(root, "deliverable", "final.mp4")
	for _, path := range []string{oldFile, newFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("video"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Date(2026, time.September, 30, 8, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Minute)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newFile, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	files, err := listFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"assets/source.mp4", "deliverable/final.mp4"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("listFiles() = %#v, want %#v", files, want)
	}
}

func TestUploadTemporaryFileUsesTokenCredentialAndReturnsRelativeURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/file/temporary/resolve/" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"err":"temporary file not found"}`))
			return
		}
		if r.URL.Path != "/api/file/run/" || r.Method != http.MethodPost {
			t.Fatalf("unexpected upload request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer private-key" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("temporary") != "true" || r.FormValue("skill2api") != "true" {
			t.Fatalf("unexpected upload fields: %#v", r.MultipartForm.Value)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || header.Filename != "result.mp4" || !bytes.Equal(data, []byte("video")) {
			t.Fatalf("unexpected upload file: name=%q data=%q err=%v", header.Filename, data, err)
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"file":{"file_key":%q,"file_ext":"mp4"}}`, fileKeyForData([]byte("video")))))
	}))
	defer server.Close()

	file, uploadURL, err := uploadTemporaryFile(context.Background(), config{UploadBaseURL: server.URL}, fileRequest{
		FilePath:    "outputs/result.mp4",
		Environment: map[string]string{"SANDBOX_AI_KEY": "private-key"},
	}, []byte("video"))
	if err != nil {
		t.Fatal(err)
	}
	key := fileKeyForData([]byte("video"))
	if string(file) != fmt.Sprintf(`{"file_key":%q,"file_ext":"mp4"}`, key) || uploadURL != fmt.Sprintf("/upload/%s/%s/%s.mp4", key[:2], key[2:4], key) {
		t.Fatalf("unexpected upload result: file=%s url=%q", file, uploadURL)
	}
}

func TestUploadTemporaryFileReusesResolvedContent(t *testing.T) {
	key := fileKeyForData([]byte("video"))
	uploaded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/file/temporary/resolve/":
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected resolver method: %s", r.Method)
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"file":{"file_key":%q,"file_ext":"mp4","expires_at":9999},"url":"/upload/%s/%s/%s.mp4"}`, key, key[:2], key[2:4], key)))
		case "/api/file/run/":
			uploaded = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	file, uploadURL, err := uploadTemporaryFile(context.Background(), config{UploadBaseURL: server.URL}, fileRequest{
		FilePath:    "outputs/result.mp4",
		Environment: map[string]string{"SANDBOX_AI_KEY": "private-key"},
	}, []byte("video"))
	if err != nil {
		t.Fatal(err)
	}
	if uploaded || string(file) == "" || uploadURL != fmt.Sprintf("/upload/%s/%s/%s.mp4", key[:2], key[2:4], key) {
		t.Fatalf("resolved content was not reused: uploaded=%t file=%s url=%q", uploaded, file, uploadURL)
	}
}

func TestDeliveryStatusSharesContentKeyState(t *testing.T) {
	store := &statusStore{root: t.TempDir()}
	first := fileDeliveryStatus{
		RequestID: "request-1", DeliveryID: "delivery-1", FilePath: "source.mp4",
		FileKey: "shared-key", Status: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	execute, err := store.startDelivery(first)
	if err != nil || !execute {
		t.Fatalf("first delivery start = (%t, %v), want (true, nil)", execute, err)
	}
	second := fileDeliveryStatus{
		RequestID: "request-1", DeliveryID: "delivery-2", FilePath: "source.mp4",
		FileKey: "shared-key", Status: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	execute, err = store.startDelivery(second)
	if err != nil || execute {
		t.Fatalf("second delivery start = (%t, %v), want (false, nil)", execute, err)
	}
	first.Status = "succeeded"
	first.File = json.RawMessage(`{"file_key":"shared-key","file_ext":"mp4"}`)
	first.URL = "/upload/sh/ar/shared-key.mp4"
	if err := store.finishDelivery(first); err != nil {
		t.Fatal(err)
	}
	got, err := store.readDelivery("request-1", "delivery-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.URL != first.URL || got.DeliveryID != "delivery-2" {
		t.Fatalf("shared delivery = %#v", got)
	}
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

func TestSkillNamesSupportsOrderedCommaSeparatedSelection(t *testing.T) {
	c, _ := testConfig(t)
	if err := os.MkdirAll(filepath.Join(c.SkillsDir, "second", "references"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.SkillsDir, "second", "SKILL.md"), []byte("second skill"), 0600); err != nil {
		t.Fatal(err)
	}
	names, err := skillNames(c, " demo, second ")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"demo", "second"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("skill names = %#v, want %#v", names, want)
	}
	for _, value := range []string{"demo,,second", "demo,demo", "demo,../second", "demo,missing"} {
		if _, err := skillNames(c, value); err == nil {
			t.Fatalf("skill names %q unexpectedly accepted", value)
		}
	}
	if err := validateRequest(generateRequest{RequestID: "request-1", SkillName: "demo, second", OutputDir: "request-1", Prompt: "Generate"}, c); err != nil {
		t.Fatalf("multi-skill request rejected: %v", err)
	}
}

func TestSkillNamesRejectsPackageSymlinkOutsideSkillsRoot(t *testing.T) {
	c, _ := testConfig(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.SkillsDir, "outside")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := skillNames(c, "outside"); err == nil {
		t.Fatal("skill symlink outside configured skills directory was accepted")
	}
}

func TestSkillNamesRejectsResourceSymlinkOutsideSkillPackage(t *testing.T) {
	c, _ := testConfig(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.SkillsDir, "demo", "references")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := skillNames(c, "demo"); err == nil {
		t.Fatal("skill resource symlink outside selected package was accepted")
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
	if _, needed, err := parseInputRequest("SKILL2API_INPUT_REQUIRED\nnot JSON\n"); !needed || err == nil {
		t.Fatalf("malformed input request was accepted: needed=%t err=%v", needed, err)
	}
	if _, needed, err := parseInputRequest("SKILL2API_INPUT_REQUIRED\n{\"question\":\"old\"}\nmore output"); needed || err != nil {
		t.Fatalf("non-terminal input request was accepted: needed=%t err=%v", needed, err)
	}
}

func TestHasTokenUsageSummary(t *testing.T) {
	if !hasTokenUsageSummary("finished\ntokens used\n21,712\n") {
		t.Fatal("token usage summary was not detected")
	}
	if hasTokenUsageSummary("finished\n464 tokens\n") {
		t.Fatal("incomplete token usage summary was accepted")
	}
}

func TestDockerExecutionWithoutTokenUsageIsInterrupted(t *testing.T) {
	c, root := testConfig(t)
	fakeDocker := filepath.Join(root, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf 'session id: docker-session-1\\n' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDocker, c.CodexDockerBin = true, fakeDocker
	req := generateRequest{RequestID: "request-1", SkillName: "demo", OutputDir: filepath.Join(root, "request-1"), Prompt: "Create the requested video."}
	if err := os.MkdirAll(req.OutputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(req.OutputDir, "final.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &statusStore{root: root}
	if err := store.create(taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), SkillName: req.SkillName, OutputDir: req.OutputDir}, false); err != nil {
		t.Fatal(err)
	}
	execute(store, newTaskManager(), c, req)
	got, err := store.read(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.FinishedAt != "" {
		t.Fatalf("Docker execution status = %#v", got)
	}
	if !containsFile(got.Files, "final.mp4") {
		t.Fatalf("Docker execution files = %#v, want final.mp4", got.Files)
	}
}

func TestDockerExecutionFailureWithoutTokenUsagePreservesFiles(t *testing.T) {
	c, root := testConfig(t)
	fakeDocker := filepath.Join(root, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf 'session id: docker-session-1\\n' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDocker, c.CodexDockerBin = true, fakeDocker
	req := generateRequest{RequestID: "request-1", SkillName: "demo", OutputDir: filepath.Join(root, "request-1"), Prompt: "Create the requested video."}
	if err := os.MkdirAll(req.OutputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(req.OutputDir, "final.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &statusStore{root: root}
	if err := store.create(taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), SkillName: req.SkillName, OutputDir: req.OutputDir}, false); err != nil {
		t.Fatal(err)
	}
	execute(store, newTaskManager(), c, req)
	got, err := store.read(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.FinishedAt != "" {
		t.Fatalf("Docker execution failure status = %#v", got)
	}
	if !containsFile(got.Files, "final.mp4") {
		t.Fatalf("Docker execution failure files = %#v, want final.mp4", got.Files)
	}
}

func TestDockerExecutionMarksInputRequiredInsteadOfSucceeded(t *testing.T) {
	c, root := testConfig(t)
	fakeDocker := filepath.Join(root, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf 'SKILL2API_INPUT_REQUIRED\\n{\\\"question\\\":\\\"Provide provider credentials\\\",\\\"options\\\":[\\\"configure credentials\\\"]}\\n'\nprintf 'session id: docker-session-1\\n' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDocker, c.CodexDockerBin = true, fakeDocker
	req := generateRequest{RequestID: "request-1", SkillName: "demo", OutputDir: filepath.Join(root, "request-1"), Prompt: "Create the requested video."}
	store := &statusStore{root: root}
	if err := store.create(taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), SkillName: req.SkillName, OutputDir: req.OutputDir}, false); err != nil {
		t.Fatal(err)
	}
	execute(store, newTaskManager(), c, req)
	got, err := store.read(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_for_input" || got.Question != "Provide provider credentials" || got.SessionID != "docker-session-1" || got.FinishedAt != "" {
		t.Fatalf("Docker execution status = %#v", got)
	}
}

func TestDockerResumeMarksInputRequiredInsteadOfSucceeded(t *testing.T) {
	c, root := testConfig(t)
	fakeDocker := filepath.Join(root, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf 'SKILL2API_INPUT_REQUIRED\\n{\\\"question\\\":\\\"Choose a model\\\",\\\"options\\\":[\\\"model-a\\\",\\\"model-b\\\"]}\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDocker, c.CodexDockerBin = true, fakeDocker
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	store := &statusStore{root: root}
	v := taskStatus{RequestID: "request-1", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), SkillName: "demo", OutputDir: outputDir, SessionID: "docker-session-1"}
	if err := store.write(v); err != nil {
		t.Fatal(err)
	}
	executeResume(store, newTaskManager(), c, resumeRequest{RequestID: v.RequestID, Answer: "continue"}, v)
	got, err := store.read(v.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting_for_input" || got.Question != "Choose a model" || !reflect.DeepEqual(got.Options, []string{"model-a", "model-b"}) || got.FinishedAt != "" {
		t.Fatalf("Docker resume status = %#v", got)
	}
}

func TestDockerResumeIgnoresPriorInputMarkerWhenNewRunIsSilent(t *testing.T) {
	c, root := testConfig(t)
	fakeDocker := filepath.Join(root, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDocker, c.CodexDockerBin = true, fakeDocker
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, stdoutLogName), []byte("SKILL2API_INPUT_REQUIRED\n{\"question\":\"Previous question\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "final.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &statusStore{root: root}
	v := taskStatus{RequestID: "request-1", Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), SkillName: "demo", OutputDir: outputDir, SessionID: "docker-session-1"}
	if err := store.write(v); err != nil {
		t.Fatal(err)
	}
	executeResume(store, newTaskManager(), c, resumeRequest{RequestID: v.RequestID, Answer: "continue"}, v)
	got, err := store.read(v.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.Question != "" || got.FinishedAt != "" {
		t.Fatalf("silent Docker resume status = %#v", got)
	}
	if !containsFile(got.Files, "final.mp4") {
		t.Fatalf("silent Docker resume files = %#v, want final.mp4", got.Files)
	}
}

func containsFile(files []string, want string) bool {
	for _, file := range files {
		if file == want {
			return true
		}
	}
	return false
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
	if got := codexExecArgs(config{}, "gpt-6-sol", "/tmp/request-1", []string{"prompt"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("native args = %#v, want %#v", got, want)
	}
	want = []string{"exec", "--model", "gpt-6-sol", "--sandbox", "danger-full-access", "--cd", "/workspace", "--skip-git-repo-check", "prompt"}
	if got := codexExecArgs(config{CodexDocker: true}, "gpt-6-sol", "/tmp/request-1", []string{"prompt"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("docker args = %#v, want %#v", got, want)
	}
}

func TestCommandExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 127")
	if err := cmd.Run(); err == nil {
		t.Fatal("command unexpectedly succeeded")
	} else if got := commandExitCode(err); got != 127 {
		t.Fatalf("exit code = %d, want 127", got)
	}
	if got := commandExitCode(errors.New("start failed")); got != -1 {
		t.Fatalf("non-exit error code = %d, want -1", got)
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
	cmd, executable, err := newCodexCommand(context.Background(), c, "-request-1", outputDir, map[string]string{"SANDBOX_AI_KEY": "client-key", "OTHER_TOKEN": "other-value"}, codexExecArgs(c, "", outputDir, []string{"prompt"}))
	if err != nil {
		t.Fatal(err)
	}
	if executable != "docker-test" || cmd.Path != "docker-test" {
		t.Fatalf("unexpected executable: %q (%q)", executable, cmd.Path)
	}
	joined := strings.Join(cmd.Args, "\n")
	for _, want := range []string{"run", "--rm", "--name\nskill2api--request-1", "--init", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "dst=/workspace", "dst=/home/ubuntu", "--workdir\n/workspace", "--env\nHOME=/home/ubuntu", "--env\nSANDBOX_AI_KEY", "--env\nOTHER_TOKEN", "example/sandbox:tag\ncodex\nexec", "--sandbox\ndanger-full-access", "--cd\n/workspace"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker command missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "dst=/home/ubuntu/.codex/sessions") {
		t.Fatalf("Docker command must mount the private home, not only sessions: %q", cmd.Args)
	}
	if strings.Contains(joined, "dst=/opt") {
		t.Fatalf("Docker command mounted /opt without configuration: %q", cmd.Args)
	}
	if strings.Contains(joined, "--network\nnone") {
		t.Fatalf("Docker command blocked provider network access: %q", cmd.Args)
	}
	if strings.Contains(joined, "client-key") || strings.Contains(joined, "other-value") {
		t.Fatalf("docker command exposed environment values: %q", joined)
	}
	if !strings.Contains(joined, codexHomeDir(c, "-request-1")) {
		t.Fatalf("docker command did not mount persistent home: %q", joined)
	}
	if _, err := os.Stat(codexHomeDir(c, "-request-1")); err != nil {
		t.Fatalf("persistent home was not created: %v", err)
	}
	configPath := filepath.Join(codexHomeDir(c, "-request-1"), ".codex", "config.toml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read Docker Codex config: %v", err)
	}
	if string(configData) != dockerCodexConfig {
		t.Fatalf("Docker Codex config = %q, want %q", configData, dockerCodexConfig)
	}
	configInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat Docker Codex config: %v", err)
	}
	if configInfo.Mode().Perm() != 0600 {
		t.Fatalf("Docker Codex config permissions = %o, want 600", configInfo.Mode().Perm())
	}
	if strings.Contains(string(configData), "client-key") || strings.Contains(string(configData), "other-value") {
		t.Fatalf("Docker Codex config persisted request environment: %q", configData)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "SANDBOX_AI_KEY=client-key") {
		t.Fatalf("client key was not forwarded to Docker: %#v", cmd.Env)
	}
}

func TestDockerCodexCommandMountsOnlySelectedSkillPackages(t *testing.T) {
	c, root := testConfig(t)
	c.CodexDocker, c.CodexDockerBin, c.CodexDockerImage = true, "docker-test", "example/sandbox:tag"
	for _, name := range []string{"hypit", "imagegen", "unselected"} {
		if err := os.MkdirAll(filepath.Join(c.SkillsDir, name, "references"), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.SkillsDir, name, "SKILL.md"), []byte(name+" skill"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := newCodexCommandSkills(context.Background(), c, "request-1", outputDir, []string{"hypit", "imagegen"}, nil, []string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, "\n")
	for _, name := range []string{"hypit", "imagegen"} {
		dir, err := skillDir(c, name)
		if err != nil {
			t.Fatal(err)
		}
		want := "src=" + dir + ",dst=/workspace/skills/" + name + ",readonly"
		if !strings.Contains(joined, want) {
			t.Fatalf("Docker command missing selected skill mount %q: %q", want, cmd.Args)
		}
	}
	if strings.Contains(joined, "unselected") || strings.Contains(joined, "src="+c.SkillsDir+",dst=/workspace/skills") {
		t.Fatalf("Docker command exposed unselected skills: %q", cmd.Args)
	}
}

func TestCodexPromptSkillsPreservesContentAndAddsDockerResourceMap(t *testing.T) {
	prompt := "Make the deliverable."
	skills := [][]byte{[]byte("first skill"), []byte("second skill")}
	got := codexPromptSkills(prompt, skills, []string{"hypit", "imagegen"}, true)
	want := prompt + "\n\nfirst skill\n\nsecond skill\n\nSelected skill packages are mounted read-only. Resolve package-relative resources using:\n- hypit: /workspace/skills/hypit\n- imagegen: /workspace/skills/imagegen\n"
	if got != want {
		t.Fatalf("multi-skill Docker prompt = %q, want %q", got, want)
	}
}

func TestRemoveDockerContainer(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "args")
	success := filepath.Join(root, "docker-success")
	if err := os.WriteFile(success, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE", capture)
	c := config{CodexDocker: true, CodexDockerBin: success}
	if err := removeDockerContainer(context.Background(), c, "request-1"); err != nil {
		t.Fatalf("remove Docker container: %v", err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "rm\n--force\nskill2api-request-1\n" {
		t.Fatalf("docker removal args = %q", args)
	}

	missing := filepath.Join(root, "docker-missing")
	if err := os.WriteFile(missing, []byte("#!/bin/sh\necho 'Error response from daemon: No such container: skill2api-request-1' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDockerBin = missing
	if err := removeDockerContainer(context.Background(), c, "request-1"); err != nil {
		t.Fatalf("missing Docker container should be ignored: %v", err)
	}

	failure := filepath.Join(root, "docker-failure")
	if err := os.WriteFile(failure, []byte("#!/bin/sh\necho daemon unavailable >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexDockerBin = failure
	if err := removeDockerContainer(context.Background(), c, "request-1"); err == nil || !strings.Contains(err.Error(), "daemon unavailable") {
		t.Fatalf("unexpected Docker removal error: %v", err)
	}

	c.CodexDocker = false
	if err := removeDockerContainer(context.Background(), c, "request-1"); err != nil {
		t.Fatalf("native execution should skip Docker removal: %v", err)
	}
}

func TestDockerCodexOptionalOptMountAndPath(t *testing.T) {
	c, root := testConfig(t)
	c.CodexDocker = true
	c.CodexDockerOptDir = filepath.Join(root, "external-tools")
	if err := os.MkdirAll(c.CodexDockerOptDir, 0750); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := newCodexCommand(context.Background(), c, "request-1", outputDir, map[string]string{"PATH": "/request/bin"}, []string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, "\n")
	if !strings.Contains(joined, "src="+c.CodexDockerOptDir+",dst=/opt,readonly") {
		t.Fatalf("Docker command did not mount external tools read-only: %q", cmd.Args)
	}
	if strings.Contains(joined, "/request/bin") {
		t.Fatalf("Docker command exposed PATH value: %q", cmd.Args)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "PATH=/opt/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Fatalf("Docker command did not set the /opt PATH: %#v", cmd.Env)
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
		t.Fatalf("Docker requests shared Codex home: first=%q second=%q", first.Args, second.Args)
	}
	firstConfig := filepath.Join(firstHome, ".codex", "config.toml")
	secondConfig := filepath.Join(secondHome, ".codex", "config.toml")
	if firstConfig == secondConfig {
		t.Fatalf("Docker requests shared Codex config path: first=%q second=%q", firstConfig, secondConfig)
	}
	if _, err := os.Stat(firstConfig); err != nil {
		t.Fatalf("first Docker config was not created: %v", err)
	}
	if _, err := os.Stat(secondConfig); err != nil {
		t.Fatalf("second Docker config was not created: %v", err)
	}
}

func TestNativeCodexHomeIsPrivatePerRequest(t *testing.T) {
	c, root := testConfig(t)
	outputDir := filepath.Join(root, "request-1")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := newCodexCommand(context.Background(), c, "request-1", outputDir, map[string]string{"HOME": "ignored"}, []string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "HOME="+codexHomeDir(c, "request-1")) {
		t.Fatalf("native command did not use private home: %#v", cmd.Env)
	}
	if _, err := os.Stat(codexHomeDir(c, "request-1")); err != nil {
		t.Fatalf("native private home was not created: %v", err)
	}
}

func TestCleanupRemovesOnlyNamedExpiredDedicatedTaskDataAndSessions(t *testing.T) {
	c, root := testConfig(t)
	store := &statusStore{root: root}
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	expired := filepath.Join(root, "expired")
	if err := os.MkdirAll(expired, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(expired, "artifact.txt"), []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexHomeDir(c, "expired"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.write(taskStatus{RequestID: "expired", Status: "succeeded", FinishedAt: now.Add(-retentionAge).Format(time.RFC3339Nano), OutputDir: expired}); err != nil {
		t.Fatal(err)
	}
	otherExpired := filepath.Join(root, "other-expired")
	if err := os.MkdirAll(otherExpired, 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.write(taskStatus{RequestID: "other-expired", Status: "failed", FinishedAt: now.Add(-retentionAge).Format(time.RFC3339Nano), OutputDir: otherExpired}); err != nil {
		t.Fatal(err)
	}

	resumed := filepath.Join(root, "resumed")
	if err := store.write(taskStatus{RequestID: "resumed", Status: "succeeded", FinishedAt: now.Add(-retentionAge).Format(time.RFC3339Nano), OutputDir: resumed, SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resumed, 0750); err != nil {
		t.Fatal(err)
	}
	if resumedStatus, err := store.claimSucceeded("resumed"); err != nil || resumedStatus.Status != "running" || resumedStatus.FinishedAt != "" {
		t.Fatalf("resumed task did not reset retention window: %#v err=%v", resumedStatus, err)
	}
	custom := filepath.Join(root, "custom-output")
	if err := os.MkdirAll(custom, 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.write(taskStatus{RequestID: "custom", Status: "failed", FinishedAt: now.Add(-retentionAge).Format(time.RFC3339Nano), OutputDir: custom}); err != nil {
		t.Fatal(err)
	}

	got := store.cleanup(c, "expired", now)
	if got.Status != "deleted" || got.RequestID != "expired" {
		t.Fatalf("cleanup result = %#v", got)
	}
	for _, path := range []string{expired, filepath.Dir(codexHomeDir(c, "expired"))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired task data remains at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(resumed); err != nil {
		t.Fatalf("resumed task was removed: %v", err)
	}
	if _, err := os.Stat(otherExpired); err != nil {
		t.Fatalf("unrelated expired task was removed: %v", err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("custom output was removed: %v", err)
	}
	if got := store.cleanup(c, "resumed", now); got.Status != "deferred" {
		t.Fatalf("resumed task cleanup result = %#v", got)
	}
	if got := store.cleanup(c, "custom", now); got.Status != "skipped" {
		t.Fatalf("custom task cleanup result = %#v", got)
	}
}

func TestRemoveTaskDataKeepsStatusForRetryWhenArtifactRemovalFails(t *testing.T) {
	taskDir := t.TempDir()
	statusPath := filepath.Join(taskDir, "status.json")
	artifactPath := filepath.Join(taskDir, "artifact.txt")
	if err := os.WriteFile(statusPath, []byte(`{"request_id":"request-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("busy artifact")
	err := removeTaskData(taskDir, statusPath, func(path string) error {
		if path == artifactPath {
			return wantErr
		}
		return os.RemoveAll(path)
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("removeTaskData error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(statusPath); err != nil {
		t.Fatalf("status file was removed after failed cleanup: %v", err)
	}
}

func TestCleanupRescheduleDelayUsesRetentionDeadline(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	if delay, ok := cleanupRescheduleDelay(cleanupResponse{Status: "deferred"}, now); !ok || delay != int(retentionAge.Seconds()) {
		t.Fatalf("deferred cleanup delay = %d, %t", delay, ok)
	}
	dueAt := now.Add(90*time.Minute + 500*time.Millisecond)
	if delay, ok := cleanupRescheduleDelay(cleanupResponse{Status: "not_due", retryAt: dueAt}, now); !ok || delay != 5401 {
		t.Fatalf("not-due cleanup delay = %d, %t", delay, ok)
	}
	if _, ok := cleanupRescheduleDelay(cleanupResponse{Status: "not_due"}, now); ok {
		t.Fatal("cleanup without a due time should not be rescheduled")
	}
}

func TestCleanupUsesFinishedAtForRetentionDeadline(t *testing.T) {
	c, root := testConfig(t)
	store := &statusStore{root: root}
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Hour)
	outputDir := filepath.Join(root, "recent")
	if err := os.MkdirAll(outputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.write(taskStatus{RequestID: "recent", Status: "succeeded", FinishedAt: finishedAt.Format(time.RFC3339Nano), OutputDir: outputDir}); err != nil {
		t.Fatal(err)
	}
	got := store.cleanup(c, "recent", now)
	if got.Status != "not_due" || !got.retryAt.Equal(finishedAt.Add(retentionAge)) {
		t.Fatalf("cleanup result = %#v, want not_due at %s", got, finishedAt.Add(retentionAge))
	}
}

func TestSubmitCleanupAtUsesRequestIDAndTerminalRetention(t *testing.T) {
	c, _ := testConfig(t)
	c.TaskPrefix = "generation-"
	finishedAt := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	var gotFunc, gotName string
	var gotOptions map[string]interface{}
	err := submitCleanupAt(func(funcName, name string, options map[string]interface{}) error {
		gotFunc, gotName, gotOptions = funcName, name, options
		return nil
	}, c, "request-1", finishedAt)
	if err != nil {
		t.Fatal(err)
	}
	if gotFunc != "generation-skill2api_cleanup:node-a" || gotName != "request-1" || gotOptions["schedat"] != finishedAt.Add(retentionAge).Unix() {
		t.Fatalf("cleanup schedule = func=%q name=%q options=%#v", gotFunc, gotName, gotOptions)
	}
}

func TestNodeFunctionNamesKeepGenerateShared(t *testing.T) {
	c := config{TaskPrefix: "generation-", NodeID: "node-a"}
	want := []string{
		"generation-skill2api_generate",
		"generation-skill2api_status:node-a",
		"generation-skill2api_file:node-a",
		"generation-skill2api_file_delivery:node-a",
		"generation-skill2api_file_delivery_status:node-a",
		"generation-skill2api_resume:node-a",
		"generation-skill2api_terminate:node-a",
		"generation-skill2api_cleanup:node-a",
	}
	if got := registeredFunctionNames(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered function names = %#v, want %#v", got, want)
	}
}

func TestQueuedGenerateResponseIncludesNodeID(t *testing.T) {
	got := queuedGenerateResponse("request-1", "node-a")
	if got != (generateResponse{RequestID: "request-1", Status: "queued", NodeID: "node-a"}) {
		t.Fatalf("generate response = %#v", got)
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

func TestRunCodexDebugPreservesSensitiveLogs(t *testing.T) {
	c, root := testConfig(t)
	c.Debug = true
	c.MaxOutput = 4096
	bin := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'secret prompt value\\n' >&2\nprintf 'session id: private-session-1\\n' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.CodexBin = bin
	out := filepath.Join(root, "request-1")
	if err := os.MkdirAll(out, 0750); err != nil {
		t.Fatal(err)
	}
	_, stderr, sessionID, err := runCodexWithSession(context.Background(), c, generateRequest{OutputDir: out, Prompt: "secret prompt value"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "private-session-1" || !strings.Contains(stderr, "secret prompt value") || !strings.Contains(stderr, "private-session-1") {
		t.Fatalf("debug output did not preserve sensitive data: session=%q stderr=%q", sessionID, stderr)
	}
	logData, err := os.ReadFile(filepath.Join(out, stderrLogName))
	if err != nil || !strings.Contains(string(logData), "private-session-1") {
		t.Fatalf("debug stderr log missing session ID: %q err=%v", logData, err)
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
