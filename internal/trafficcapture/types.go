package trafficcapture

const TmuxSocket = "crux-capture"

type Request struct {
	Executable         string
	Arguments          []string
	PID                int
	WorkingDir         string
	WorkingDirExplicit bool
	CapturePath        string
	ManagedCapture     bool
	UnsetEnv           []string
	Wait               bool
}

type Metadata struct {
	Session     string
	CapturePath string
	StatusPath  string
	PaneLogPath string
	Attach      string
	ViewerURL   string
}

type workerConfig struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	WorkingDir  string            `json:"cwd"`
	Output      string            `json:"output"`
	Host        string            `json:"host"`
	Port        int               `json:"port"`
	ViewerPort  int               `json:"viewer_port"`
	UnsetEnv    []string          `json:"unset_env"`
	RuntimePath string            `json:"runtime_path"`
	StatusPath  string            `json:"status_path"`
	ReadyPath   string            `json:"ready_path"`
	StopPath    string            `json:"stop_path"`
	PaneLogPath string            `json:"pane_log"`
	Session     string            `json:"session"`
}

type workerStatus struct {
	State     string `json:"state"`
	Error     string `json:"error"`
	ExitCode  int    `json:"exit_code"`
	ViewerURL string `json:"viewer_url"`
}
