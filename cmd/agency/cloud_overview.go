package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lemonsaurus/agency/internal/cloud"
	"github.com/lemonsaurus/agency/internal/config"
	"github.com/lemonsaurus/agency/internal/ipc"
	"github.com/lemonsaurus/agency/internal/live"
	"github.com/lemonsaurus/agency/internal/tmux"
)

// overview is the phone's one-call picture of the box: sessions and what they are doing, other
// processes such as dev servers, parked work from private-memory tasks, and machine load.
type overview struct {
	At         int64             `json:"at"`
	Box        overviewBox       `json:"box"`
	Sessions   []overviewSession `json:"sessions"`
	Runtimes   []overviewRuntime `json:"runtimes"`
	Tasks      json.RawMessage   `json:"tasks"`
	TasksError string            `json:"tasksError,omitempty"`
}

type overviewBox struct {
	Load      float64 `json:"load"`
	CPUs      int     `json:"cpus"`
	MemUsed   float64 `json:"memUsed"`
	DiskFree  uint64  `json:"diskFree"`
	DiskTotal uint64  `json:"diskTotal"`
}

// State is live.Glance's working or idle, reload for a Pi without a bridge, or unknown when the
// bridge did not answer.
type overviewSession struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
	Dir   string `json:"cwd"`
	Role  string `json:"role"`
	live.Glance
}

type overviewRuntime struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Group   string `json:"group"`
	Command string `json:"command"`
}

func runOverview(cfg *config.Config) {
	panes, err := tmux.NewClient(cfg.Session.Name, "").ListPanes(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	home, _ := os.UserHomeDir()
	result := overview{At: time.Now().UnixMilli(), Box: machine(home), Sessions: []overviewSession{}, Runtimes: []overviewRuntime{}}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		result.Tasks, result.TasksError = openTasks(home)
	}()
	var mu sync.Mutex
	daemon := socketPath(cfg.Session.Name)
	for _, pane := range panes {
		if pane.Session != cfg.Session.Name {
			continue
		}
		bridge := bridgeListening(daemon, pane.ID)
		if !bridge && pane.Command != "pi" {
			result.Runtimes = append(result.Runtimes, overviewRuntime{pane.ID, pane.TaskLabel, cloud.Group(pane), pane.Command})
			continue
		}
		session := overviewSession{ID: pane.ID, Label: pane.TaskLabel, Group: cloud.Group(pane), Dir: pane.CWD, Role: pane.Role, Glance: live.Glance{State: "reload"}}
		if !bridge {
			result.Sessions = append(result.Sessions, session)
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			session.Glance = glance(daemon, session.ID)
			mu.Lock()
			result.Sessions = append(result.Sessions, session)
			mu.Unlock()
		}()
	}
	wait.Wait()
	sort.SliceStable(result.Sessions, func(i, j int) bool { return result.Sessions[i].At > result.Sessions[j].At })
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func glance(daemon, paneID string) live.Glance {
	path, err := ipc.BridgePath(daemon, paneID)
	if err != nil {
		return live.Glance{State: "unknown"}
	}
	id := make([]byte, 16)
	rand.Read(id)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	reply, err := ipc.Transcript(ctx, path, ipc.TranscriptRequest{ID: hex.EncodeToString(id), Limit: 12})
	if err != nil || reply.Error != nil {
		return live.Glance{State: "unknown"}
	}
	return live.GlanceAt(live.ParseTranscript(reply.Entries), 400)
}

// openTasks reads parked work through dotagents' private-memory script; Go has no SQLite driver here.
func openTasks(home string) (json.RawMessage, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", filepath.Join(home, ".pi", "agent", "extensions", "private-memory", "open-tasks.mjs")).Output()
	if err != nil || !json.Valid(out) {
		return json.RawMessage("[]"), "Parked work is unavailable: private memory did not answer."
	}
	return out, ""
}

func machine(home string) overviewBox {
	box := overviewBox{CPUs: runtime.NumCPU()}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		box.Load, _ = strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		fields := map[string]float64{}
		for _, line := range strings.Split(string(data), "\n") {
			if parts := strings.Fields(line); len(parts) >= 2 {
				fields[strings.TrimSuffix(parts[0], ":")], _ = strconv.ParseFloat(parts[1], 64)
			}
		}
		if fields["MemTotal"] > 0 {
			box.MemUsed = 1 - fields["MemAvailable"]/fields["MemTotal"]
		}
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(home, &disk) == nil {
		box.DiskFree, box.DiskTotal = disk.Bavail*uint64(disk.Bsize), disk.Blocks*uint64(disk.Bsize)
	}
	return box
}
