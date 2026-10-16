package wsl

type ProbeRequest struct{ Root, Outside, WindowsExecutable string }
type ProbeResult struct {
	Kernel     string `json:"kernel"`
	ABI        int    `json:"abi"`
	Filesystem int64  `json:"filesystem"`
}
