//go:build !windows

package notify

import (
	"context"
	"os/exec"
	"time"
)

func probeNotifyBackend() bool {
	_, err := exec.LookPath("notify-send")
	return err == nil
}

func sendNotification(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "notify-send",
		"--app-name=telegrim",
		"--icon=chat",
		"--expire-time=5000",
		title, body).Run()
}
