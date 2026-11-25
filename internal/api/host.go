package api

import (
	"bufio"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/sat0ken/niwakube/internal/backend"
)

// HostInfo はホスト (niwakubed が動いているミニPC) の状態。
type HostInfo struct {
	Hostname  string        `json:"hostname"`
	Version   string        `json:"version"`
	CPUs      int           `json:"cpus"`
	Memory    MemoryInfo    `json:"memory"`
	Disk      DiskInfo      `json:"disk"`
	Allocated AllocatedInfo `json:"allocated"`
}

type MemoryInfo struct {
	TotalMiB     int64 `json:"totalMiB"`
	AvailableMiB int64 `json:"availableMiB"`
}

type DiskInfo struct {
	Path     string  `json:"path"`
	TotalGiB float64 `json:"totalGiB"`
	FreeGiB  float64 `json:"freeGiB"`
}

// AllocatedInfo は VM に割り当てたリソース (動いている VM の合計)。
type AllocatedInfo struct {
	Machines  int `json:"machines"`
	Running   int `json:"running"`
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
}

func readMeminfo() MemoryInfo {
	var m MemoryInfo
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			m.TotalMiB = kb / 1024
		case "MemAvailable:":
			m.AvailableMiB = kb / 1024
		}
	}
	return m
}

func diskInfo(path string) DiskInfo {
	d := DiskInfo{Path: path}
	var st syscall.Statfs_t
	if path == "" || syscall.Statfs(path, &st) != nil {
		return d
	}
	const gib = 1 << 30
	d.TotalGiB = float64(st.Blocks) * float64(st.Bsize) / gib
	d.FreeGiB = float64(st.Bavail) * float64(st.Bsize) / gib
	return d
}

func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	h := HostInfo{Version: s.opts.Version, CPUs: runtime.NumCPU(), Memory: readMeminfo(), Disk: diskInfo(s.opts.DataDir)}
	h.Hostname, _ = os.Hostname()
	vms, err := s.svc.List(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	for _, vm := range vms {
		h.Allocated.Machines++
		if vm.Status.State == backend.StateRunning {
			h.Allocated.Running++
			h.Allocated.CPUs += vm.CPUs
			h.Allocated.MemoryMiB += vm.MemoryMiB
		}
	}
	writeJSON(w, http.StatusOK, h)
}
