// Package notify shows a desktop notification that stays until dismissed.
package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"unicode/utf16"
)

// Show posts a critical notification with notify-send on Linux, or a reminder toast through
// powershell.exe on WSL.
func Show(ctx context.Context, title, text string) error {
	if isWSL() {
		return toast(ctx, title, text)
	}
	if out, err := exec.CommandContext(ctx, "notify-send", "-u", "critical", "-a", "Agency", "--", title, text).CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	release, err := os.ReadFile("/proc/version")
	return err == nil && bytes.Contains(bytes.ToLower(release), []byte("microsoft"))
}

// toastScript reads title and text from the environment so no quoting reaches PowerShell. The
// reminder scenario with a Dismiss button keeps the toast up until it is dismissed. The app id is
// PowerShell's, which Windows lets post toasts without registering one.
const toastScript = `$ErrorActionPreference = 'Stop'
[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
$title = [System.Security.SecurityElement]::Escape($env:AGENCY_TOAST_TITLE)
$text = [System.Security.SecurityElement]::Escape($env:AGENCY_TOAST_TEXT)
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml("<toast scenario='reminder'><visual><binding template='ToastGeneric'><text>$title</text><text>$text</text></binding></visual><actions><action content='Dismiss' arguments='dismiss' activationType='system'/></actions><audio src='ms-winsoundevent:Notification.Reminder'/></toast>")
$toast = New-Object Windows.UI.Notifications.ToastNotification $xml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show($toast)`

func toast(ctx context.Context, title, text string) error {
	units := utf16.Encode([]rune(toastScript))
	encoded := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		encoded = append(encoded, byte(unit), byte(unit>>8))
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	wslenv := os.Getenv("WSLENV")
	if wslenv != "" {
		wslenv += ":"
	}
	cmd.Env = append(os.Environ(), "AGENCY_TOAST_TITLE="+title, "AGENCY_TOAST_TEXT="+text, "WSLENV="+wslenv+"AGENCY_TOAST_TITLE:AGENCY_TOAST_TEXT")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("powershell toast: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
