package live

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CodexURL is the ChatGPT plan's Responses endpoint. Only GPT-Live itself bills the platform account.
const CodexURL = "https://chatgpt.com/backend-api/codex/responses"

// CodexToken hands out the ChatGPT OAuth token Pi keeps for openai-codex. Command prints it; Pi
// refreshes it under its own lock, so this only caches it until five minutes before expiry.
type CodexToken struct {
	Command []string

	mu      sync.Mutex
	token   string
	account string
	expires time.Time
}

func (c *CodexToken) Get(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires.Add(-5*time.Minute)) {
		return c.token, c.account, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", "", fmt.Errorf("no ChatGPT login: %s", truncate(strings.TrimSpace(string(exit.Stderr)), 200))
		}
		return "", "", fmt.Errorf("no ChatGPT login: %v", err)
	}
	token := strings.TrimSpace(string(out))
	account, expires, err := parseCodexToken(token)
	if err != nil {
		return "", "", err
	}
	c.token, c.account, c.expires = token, account, expires
	return token, account, nil
}

// parseCodexToken reads the ChatGPT account id and expiry from the access token's JWT claims.
func parseCodexToken(token string) (string, time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}, fmt.Errorf("the ChatGPT token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the ChatGPT token is not a JWT")
	}
	var claims struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Auth.Account == "" || claims.Exp == 0 {
		return "", time.Time{}, fmt.Errorf("the ChatGPT token has no account id")
	}
	return claims.Auth.Account, time.Unix(claims.Exp, 0), nil
}
