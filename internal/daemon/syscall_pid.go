package daemon

import "os"

func syscallPid() int { return os.Getpid() }
