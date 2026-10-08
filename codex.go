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
	"strings"
)

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
