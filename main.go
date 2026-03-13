package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	mainCtx, mainCancel = context.WithCancel(context.Background())
	jobs                = make(map[string]*Job)
	jobsMu              sync.RWMutex
	shutdownWg          sync.WaitGroup
	jobCounter          int32
)

type Job struct {
	ID            string
	Command       string
	Args          []string
	UseShell      bool
	Workers       int
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	activeCancels []context.CancelFunc
}

type StartRequest struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Workers int      `json:"workers"`
}

type UpdateRequest struct {
	Workers int `json:"workers"`
}

type JobInfo struct {
	ID      string   `json:"id"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Workers int      `json:"workers"`
}

func startJob(req StartRequest) (string, error) {
	if req.Workers < 1 || (req.Command == "" && len(req.Args) == 0) {
		return "", fmt.Errorf("invalid parameters")
	}

	id := generateID()

	jobCtx, jobCancel := context.WithCancel(mainCtx)
	j := &Job{
		ID:       id,
		Workers:  req.Workers,
		ctx:      jobCtx,
		cancel:   jobCancel,
		UseShell: len(req.Args) == 0,
	}
	if j.UseShell {
		j.Command = req.Command
	} else {
		j.Args = make([]string, len(req.Args))
		copy(j.Args, req.Args)
	}

	jobsMu.Lock()
	jobs[id] = j
	jobsMu.Unlock()

	j.mu.Lock()
	for i := 1; i <= req.Workers; i++ {
		wCtx, wCancel := context.WithCancel(jobCtx)
		j.activeCancels = append(j.activeCancels, wCancel)
		shutdownWg.Add(1)
		go jobWorker(wCtx, id, i, j)
	}
	j.mu.Unlock()

	fmt.Printf("✅ Job %s запущен | workers=%d | %s\n",
		id, req.Workers,
		func() string {
			if j.UseShell {
				return "command=\"" + j.Command + "\""
			}
			return fmt.Sprintf("args=%v", j.Args)
		}())

	return id, nil
}

func jobsHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/jobs":
		if r.Method == http.MethodGet {
			listJobs(w)
			return
		}
		if r.Method == http.MethodPost {
			createJobHTTP(w, r)
			return
		}

	case strings.HasPrefix(r.URL.Path, "/jobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/jobs/")

		if r.Method == http.MethodPost {
			updateJob(w, r, id)
			return
		}
		if r.Method == http.MethodDelete {
			stopJob(w, id)
			return
		}
	}
	http.NotFound(w, r)
}

func createJobHTTP(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "Неверный JSON"}`, http.StatusBadRequest)
		return
	}

	id, err := startJob(req)
	if err != nil {
		http.Error(w, `{"error": "`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	resp := map[string]interface{}{
		"id":      id,
		"workers": req.Workers,
		"status":  "started",
	}
	if len(req.Args) == 0 {
		resp["command"] = req.Command
	} else {
		resp["args"] = req.Args
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func listJobs(w http.ResponseWriter) {
	jobsMu.RLock()
	defer jobsMu.RUnlock()

	var list []JobInfo
	for _, j := range jobs {
		info := JobInfo{ID: j.ID, Workers: j.Workers}
		if j.UseShell {
			info.Command = j.Command
		} else {
			info.Args = j.Args
		}
		list = append(list, info)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func updateJob(w http.ResponseWriter, r *http.Request, id string) {
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Workers < 0 {
		http.Error(w, `{"error": "workers >= 0"}`, http.StatusBadRequest)
		return
	}

	jobsMu.RLock()
	j, exists := jobs[id]
	jobsMu.RUnlock()
	if !exists {
		http.Error(w, `{"error": "Job not found"}`, http.StatusNotFound)
		return
	}

	j.mu.Lock()
	current := len(j.activeCancels)
	j.Workers = req.Workers

	if req.Workers > current {
		for i := current + 1; i <= req.Workers; i++ {
			wCtx, wCancel := context.WithCancel(j.ctx)
			j.activeCancels = append(j.activeCancels, wCancel)
			shutdownWg.Add(1)
			go jobWorker(wCtx, id, i, j)
		}
	} else if req.Workers < current {
		toStop := current - req.Workers
		for i := 0; i < toStop; i++ {
			last := len(j.activeCancels) - 1
			j.activeCancels[last]()
			j.activeCancels = j.activeCancels[:last]
		}
	}
	j.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "workers": req.Workers, "status": "updated"})
}

func stopJob(w http.ResponseWriter, id string) {
	jobsMu.Lock()
	j, exists := jobs[id]
	if !exists {
		jobsMu.Unlock()
		http.Error(w, `{"error": "Job not found"}`, http.StatusNotFound)
		return
	}
	delete(jobs, id)
	j.cancel()
	jobsMu.Unlock()

	fmt.Printf("🛑 Job %s stopped and removed\n", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped", "id": id})
}

func generateID() string {
	return fmt.Sprintf("job-%03d", atomic.AddInt32(&jobCounter, 1))
}

func loadInitialJobs() {
	env := os.Getenv("INITIAL_JOBS")
	if env == "" {
		return
	}

	var reqs []StartRequest
	if err := json.Unmarshal([]byte(env), &reqs); err != nil {
		fmt.Printf("❌ INITIAL_JOBS: JSON error: %v\n", err)
		return
	}

	fmt.Printf("🔄 Loading %d jobs from INITIAL_JOBS...\n", len(reqs))
	for i, req := range reqs {
		if _, err := startJob(req); err != nil {
			fmt.Printf("⚠️ Job #%d skipped: %v\n", i+1, err)
		}
	}
}

func jobWorker(ctx context.Context, jobID string, wid int, job *Job) {
	defer shutdownWg.Done()

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[Job %s W%2d] ⛔ Stopped by signal\n", jobID, wid)
			return
		default:
		}

		fmt.Printf("[Job %s W%2d] Starting command...\n", jobID, wid)

		var cmd *exec.Cmd
		if job.UseShell {
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/c", job.Command)
			} else {
				cmd = exec.Command("sh", "-c", job.Command)
			}
		} else {
			cmd = exec.Command(job.Args[0], job.Args[1:]...)
		}

		cmd.Stdout = &prefixWriter{jobID: jobID, wid: wid}
		cmd.Stderr = cmd.Stdout

		if err := cmd.Start(); err != nil {
			fmt.Printf("[Job %s W%2d] ❌ Start error: %v → restarting\n", jobID, wid, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			// === FIX: ALWAYS RESTART ===
			if err == nil {
				fmt.Printf("[Job %s W%2d] ✅ Finished successfully (code 0) → restarting\n", jobID, wid)
			} else if exitErr, ok := err.(*exec.ExitError); ok {
				fmt.Printf("[Job %s W%2d] ❌ Crashed with code %d → restarting\n", jobID, wid, exitErr.ExitCode())
			} else {
				fmt.Printf("[Job %s W%2d] ❌ Error: %v → restarting\n", jobID, wid, err)
			}

		case <-ctx.Done():
			if cmd.Process != nil {
				err := cmd.Process.Signal(os.Signal(syscall.SIGQUIT))
				if err != nil {
					fmt.Printf("[Job %s W%2d] force stop failed\nErr: %s", jobID, wid, err.Error())
				}
			}
			<-done
			fmt.Printf("[Job %s W%2d] ⛔ Force killed\n", jobID, wid)
			return
		}

		time.Sleep(100 * time.Millisecond) // anti-spam protection
	}
}

type prefixWriter struct {
	jobID string
	wid   int
}

func (w *prefixWriter) Write(p []byte) (n int, err error) {
	fmt.Printf("[Job %s W%2d] %s", w.jobID, w.wid, p)
	return len(p), nil
}

func main() {
	loadInitialJobs()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/jobs", jobsHandler)

	fmt.Printf("🚀 Server started at http://localhost:%s\n", port)
	fmt.Println("Mode: WORKERS ARE KEPT RUNNING CONSTANTLY (always restart)")

	go func() {
		if err := http.ListenAndServe(":"+port, nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("❌ Server crashed: %v\n", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\n⛔ Stopping server...")
	mainCancel()

	jobsMu.Lock()
	for _, j := range jobs {
		j.cancel()
	}
	jobsMu.Unlock()

	shutdownWg.Wait()
	fmt.Println("✅ All workers stopped.")
}
