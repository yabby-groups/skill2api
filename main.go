package main

import (
	"log"
	"os"
	"time"

	"github.com/Lupino/go-periodic"
	"github.com/Lupino/go-periodic/protocol"
)

func connectPeriodic(client *periodic.Client, addr string, rsa protocol.RSAConnParam) error {
	return client.Connect(addr, rsa)
}

func main() {
	c, err := newConfig()
	if err != nil {
		panic(err)
	}
	log.Printf("event=skill2api_starting output_root=%s skills_dir=%s task_prefix=%s node_id=%s timeout_seconds=%d", c.OutputRoot, c.SkillsDir, c.TaskPrefix, c.NodeID, int(c.Timeout.Seconds()))
	store := &statusStore{root: c.OutputRoot}
	manager := newTaskManager()
	if err := os.MkdirAll(c.OutputRoot, 0750); err != nil {
		panic(err)
	}
	if err := store.recoverRunning(); err != nil {
		panic(err)
	}
	log.Printf("event=skill2api_startup result=recovered_running_requests")
	for {
		worker := periodic.NewWorker(8)
		worker.SetIOTimeout(30*time.Second, 10*time.Second)
		log.Printf("event=skill2api_connect result=starting")
		if err := connectPeriodic(&worker.Client, c.PeriodicAddr, c.RSA); err != nil {
			log.Printf("event=skill2api_connect result=failed error=%q", err)
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		log.Printf("event=skill2api_connect result=connected")
		if err := registerWorkerFuncs(worker, store, manager, c); err != nil {
			log.Printf("event=skill2api_startup result=registration_failed error=%q", err)
			worker.Close()
			time.Sleep(time.Second)
			continue
		}
		log.Printf("event=skill2api_worker result=started")
		worker.Work()
		log.Printf("event=skill2api_worker result=stopped")
		worker.Close()
		time.Sleep(time.Second)
	}
}
