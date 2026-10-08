package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Lupino/go-periodic"
	"log"
	"os"
	"strings"
	"time"
)

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

func queuedGenerateResponse(requestID, nodeID string) generateResponse {
	return generateResponse{RequestID: requestID, Status: "queued", NodeID: nodeID}
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
	names, err := skillNames(c, req.SkillName)
	if err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	req.SkillName = strings.Join(names, ",")
	outputDir, err := resolveOutputDir(req.OutputDir, c.OutputRoot)
	if err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=validation_failed skill_name=%s output_dir=%s error=%q", req.RequestID, req.SkillName, req.OutputDir, err)
		_ = job.Fail()
		return
	}
	req.OutputDir = outputDir
	now := time.Now().UTC().Format(time.RFC3339Nano)
	v := taskStatus{RequestID: req.RequestID, Status: "queued", CreatedAt: now, SkillName: req.SkillName, Model: req.Model, OutputDir: req.OutputDir}
	if err := store.create(v, req.Force); err != nil {
		log.Printf("event=skill2api_generate request_id=%s result=duplicate error=%q", req.RequestID, err)
		_ = job.Fail()
		return
	}
	log.Printf("event=skill2api_state request_id=%s status=queued skill_name=%s output_dir=%s force=%t", req.RequestID, req.SkillName, req.OutputDir, req.Force)
	scheduleCleanup(c, req.RequestID)
	doneJSON(job, queuedGenerateResponse(req.RequestID, c.NodeID))
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
	if files, listErr := listFiles(v.OutputDir); listErr != nil {
		log.Printf("event=skill2api_status request_id=%s result=file_list_failed error=%q", req.RequestID, listErr)
	} else {
		v.Files = files
	}
	response, outputErr := statusWithLogOutput(v, c.MaxOutput)
	if outputErr != nil {
		log.Printf("event=skill2api_status request_id=%s result=log_read_failed error=%q", req.RequestID, outputErr)
	}
	doneJSON(job, response)
}

func handleFile(job periodic.Job, store *statusStore, c config) {
	var req fileRequest
	if err := parseArgs(job, &req); err != nil {
		log.Printf("event=skill2api_file request_id=%s result=invalid_args", job.Name)
		doneJSON(job, map[string]any{"request_id": job.Name, "status": "failed", "error": "invalid file request"})
		return
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_file request_id=%s result=request_id_mismatch job_name=%s", req.RequestID, job.Name)
		doneJSON(job, map[string]any{"request_id": job.Name, "status": "failed", "error": "request_id must match job name"})
		return
	}
	if !validID(req.RequestID) {
		log.Printf("event=skill2api_file request_id=%s result=invalid_request_id", req.RequestID)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": "invalid request_id"})
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "not_found", "error": "request not found"})
			return
		}
		log.Printf("event=skill2api_file request_id=%s result=status_read_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	data, err := readTaskFile(v.OutputDir, req.FilePath, c.MaxFileBytes)
	if err != nil {
		log.Printf("event=skill2api_file request_id=%s result=file_read_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	uploadTimeout := c.UploadTimeout
	if uploadTimeout <= 0 {
		uploadTimeout = fileUploadTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	file, uploadURL, err := uploadTemporaryFile(ctx, c, req, data)
	cancel()
	if err != nil {
		log.Printf("event=skill2api_file request_id=%s result=upload_failed error=%q", req.RequestID, err)
		doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "failed", "error": err.Error()})
		return
	}
	log.Printf("event=skill2api_file request_id=%s result=uploaded bytes=%d", req.RequestID, len(data))
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "succeeded", "file": file, "url": uploadURL})
}

func handleFileDelivery(job periodic.Job, store *statusStore, c config) {
	var req fileDeliveryRequest
	if err := parseArgs(job, &req); err != nil {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid file delivery request"})
		return
	}
	if strings.TrimSpace(req.DeliveryID) == "" {
		req.DeliveryID = job.Name
	}
	if req.DeliveryID != job.Name || !validID(req.DeliveryID) || !validID(req.RequestID) {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid delivery identifier"})
		return
	}
	v, err := store.read(req.RequestID)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "not_found", "error": "request not found"})
		return
	}
	data, err := readTaskFile(v.OutputDir, req.FilePath, c.MaxFileBytes)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	delivery := fileDeliveryStatus{
		RequestID: req.RequestID, DeliveryID: req.DeliveryID, FilePath: req.FilePath,
		FileKey: fileKeyForData(data), Status: "running", CreatedAt: now,
	}
	execute, err := store.startDelivery(delivery)
	if err != nil {
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": err.Error()})
		return
	}
	if !execute {
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=reused file_key=%s", req.RequestID, req.DeliveryID, delivery.FileKey)
		doneJSON(job, delivery)
		return
	}
	file, uploadURL, attempts, uploadErr := uploadTemporaryFileWithRetry(c, fileRequest{
		RequestID: req.RequestID, FilePath: req.FilePath, Environment: req.Environment,
	}, data)
	delivery.Attempts = attempts
	delivery.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if uploadErr != nil {
		delivery.Status, delivery.Error = "failed", uploadErr.Error()
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=failed attempts=%d error=%q", req.RequestID, req.DeliveryID, attempts, uploadErr)
	} else {
		delivery.Status, delivery.File, delivery.URL = "succeeded", file, uploadURL
		log.Printf("event=skill2api_file_delivery request_id=%s delivery_id=%s result=succeeded attempts=%d file_key=%s", req.RequestID, req.DeliveryID, attempts, delivery.FileKey)
	}
	if err := store.finishDelivery(delivery); err != nil {
		delivery.Status, delivery.Error = "failed", err.Error()
	}
	doneJSON(job, delivery)
}

func handleFileDeliveryStatus(job periodic.Job, store *statusStore) {
	var req fileDeliveryStatusRequest
	if err := parseArgs(job, &req); err != nil || req.DeliveryID != job.Name || !validID(req.RequestID) || !validID(req.DeliveryID) {
		doneJSON(job, map[string]any{"delivery_id": job.Name, "status": "failed", "error": "invalid file delivery status request"})
		return
	}
	delivery, err := store.readDelivery(req.RequestID, req.DeliveryID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "queued"})
			return
		}
		doneJSON(job, map[string]any{"request_id": req.RequestID, "delivery_id": req.DeliveryID, "status": "failed", "error": "read file delivery status"})
		return
	}
	doneJSON(job, delivery)
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
	if err := validateEnvironment(req.Environment); err != nil {
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
	scheduleCleanup(c, req.RequestID)
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": "running"})
	go executeResume(store, manager, c, req, v)
}

func handleTerminate(job periodic.Job, store *statusStore, manager *taskManager, c config) {
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := removeDockerContainer(ctx, c, req.RequestID); err != nil {
			log.Printf("event=skill2api_terminate request_id=%s result=container_cleanup_failed error=%q", req.RequestID, err)
			doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": err.Error()})
			return
		}
	}
	doneJSON(job, map[string]any{"request_id": req.RequestID, "status": v.Status, "error": v.Error})
}

func handleCleanup(job periodic.Job, store *statusStore, c config) {
	var req statusRequest
	if strings.TrimSpace(job.Args) != "" {
		if err := parseArgs(job, &req); err != nil {
			log.Printf("event=skill2api_cleanup result=invalid_args")
			_ = job.Fail()
			return
		}
	}
	if strings.TrimSpace(req.RequestID) == "" {
		req.RequestID = job.Name
	} else if req.RequestID != job.Name {
		log.Printf("event=skill2api_cleanup result=invalid_args")
		_ = job.Fail()
		return
	}
	if !validID(req.RequestID) {
		log.Printf("event=skill2api_cleanup request_id=%s result=invalid_args", req.RequestID)
		_ = job.Fail()
		return
	}
	now := time.Now().UTC()
	result := store.cleanup(c, req.RequestID, now)
	if delay, reschedule := cleanupRescheduleDelay(result, now); reschedule {
		if err := job.SchedLater(delay); err != nil {
			log.Printf("event=skill2api_cleanup request_id=%s result=reschedule_failed error=%q", req.RequestID, err)
			_ = job.Fail()
		}
		return
	}
	log.Printf("event=skill2api_cleanup request_id=%s result=%s", req.RequestID, result.Status)
	doneJSON(job, result)
}

func addWorkerFunc(worker *periodic.Worker, function string, handler func(periodic.Job)) error {
	if err := worker.AddFunc(function, handler); err != nil {
		log.Printf("event=skill2api_register function=%s result=failed error=%q", function, err)
		return err
	}
	log.Printf("event=skill2api_register function=%s result=registered", function)
	return nil
}

func registeredFunctionNames(c config) []string {
	return []string{
		withPrefix(c.TaskPrefix, generateFunc),
		nodeFunctionName(c, statusFunc),
		nodeFunctionName(c, fileFunc),
		nodeFunctionName(c, fileDeliveryFunc),
		nodeFunctionName(c, fileDeliveryStatusFunc),
		nodeFunctionName(c, resumeFunc),
		nodeFunctionName(c, terminateFunc),
		nodeFunctionName(c, cleanupFunc),
	}
}

func registerWorkerFuncs(worker *periodic.Worker, store *statusStore, manager *taskManager, c config) error {
	functions := registeredFunctionNames(c)
	if err := addWorkerFunc(worker, functions[0], func(job periodic.Job) { handleGenerate(job, store, manager, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[1], func(job periodic.Job) { handleStatus(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[2], func(job periodic.Job) { handleFile(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[3], func(job periodic.Job) { handleFileDelivery(job, store, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[4], func(job periodic.Job) { handleFileDeliveryStatus(job, store) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[5], func(job periodic.Job) { handleResume(job, store, manager, c) }); err != nil {
		return err
	}
	if err := addWorkerFunc(worker, functions[6], func(job periodic.Job) { handleTerminate(job, store, manager, c) }); err != nil {
		return err
	}
	return addWorkerFunc(worker, functions[7], func(job periodic.Job) { handleCleanup(job, store, c) })
}
