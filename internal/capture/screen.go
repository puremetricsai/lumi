package capture

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type ScreenCapturer struct {
	Binary  string
	Display int
}

func (c ScreenCapturer) Capture(ctx context.Context, destination string) error {
	binary := c.Binary
	if binary == "" {
		binary = "/usr/sbin/screencapture"
	}
	display := c.Display
	if display <= 0 {
		display = 1
	}
	output, err := exec.CommandContext(ctx, binary, "-x", "-t", "jpg", "-D", fmt.Sprint(display), destination).CombinedOutput()
	if err != nil {
		return commandError("capture screen", err, output)
	}
	return nil
}

type OCR struct {
	Binary   string
	Language string
}

func (o OCR) Extract(ctx context.Context, imagePath string) (string, error) {
	binary := o.Binary
	if binary == "" {
		binary = "tesseract"
	}
	args := []string{imagePath, "stdout", "--psm", "3"}
	if o.Language != "" {
		args = append(args, "-l", o.Language)
	}
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return "", commandError("run OCR", err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func FrontmostWindow(ctx context.Context) (app, window string) {
	const script = `tell application "System Events"
set frontProcess to first application process whose frontmost is true
set appName to name of frontProcess
try
set windowName to name of front window of frontProcess
on error
set windowName to ""
end try
return appName & linefeed & windowName
end tell`
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script).Output()
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)
	app = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		window = strings.TrimSpace(parts[1])
	}
	return app, window
}

func commandError(action string, err error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
