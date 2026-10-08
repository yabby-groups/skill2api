package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

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
