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

type pathIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type Metadata struct {
	Session        string
	CapturePath    string
	StatusPath     string
	PaneLogPath    string
	TargetLogPath  string
	ControlSocket  string
	TargetExitCode *int
	Attach         string
	ViewerURL      string
}

type workerConfig struct {
	Command            []string          `json:"command"`
	OriginalCommand    []string          `json:"original_command"`
	ControlSocket      string            `json:"control_socket"`
	Managed            bool              `json:"managed"`
	DetachedTerminal   bool              `json:"detached_terminal"`
	WireGuardPort      int               `json:"wireguard_port"`
	Environment        map[string]string `json:"environment"`
	WorkingDir         string            `json:"cwd"`
	WorkingDirIdentity pathIdentity      `json:"cwd_identity"`
	Output             string            `json:"output"`
	OutputIdentity     pathIdentity      `json:"output_identity"`
	CapturePath        string            `json:"capture_path"`
	ExecutableIdentity pathIdentity      `json:"executable_identity"`
	PassFDs            []int             `json:"pass_fds,omitempty"`
	Host               string            `json:"host"`
	Port               int               `json:"port"`
	ViewerPort         int               `json:"viewer_port"`
	UnsetEnv           []string          `json:"unset_env"`
	RuntimePath        string            `json:"runtime_path"`
	StatusPath         string            `json:"status_path"`
	ReadyPath          string            `json:"ready_path"`
	StopPath           string            `json:"stop_path"`
	PaneLogPath        string            `json:"pane_log"`
	Session            string            `json:"session"`
}

type workerStatus struct {
	State          string `json:"state"`
	Error          string `json:"error"`
	ExitCode       int    `json:"exit_code"`
	ViewerURL      string `json:"viewer_url"`
	ControlSocket  string `json:"control_socket"`
	TargetExitCode *int   `json:"target_exit_code,omitempty"`
}
