package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func projects(root string) ([]project, error) {
	result := []project{}
	owners, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, owner := range owners {
		if !owner.IsDir() || strings.HasPrefix(owner.Name(), ".") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, owner.Name()))
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			if repo.IsDir() && !strings.HasPrefix(repo.Name(), ".") {
				result = append(result, project{Name: owner.Name() + "/" + repo.Name(), Path: filepath.Join(root, owner.Name(), repo.Name())})
			}
		}
	}
	return result, nil
}

func runProjects(args []string) {
	if len(args) != 1 || args[0] != "--json" {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud projects --json")
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		var listed []project
		listed, err = projects(filepath.Join(home, "git"))
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(listed)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

type voiceToken struct {
	Value     string `json:"value"`
	ExpiresAt int64  `json:"expires_at"`
}

func mintVoiceToken(ctx context.Context, client *http.Client, key string) (voiceToken, error) {
	if strings.TrimSpace(key) == "" {
		return voiceToken{}, fmt.Errorf("OPENAI_API_KEY is not set in the environment")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/realtime/client_secrets", strings.NewReader(`{"session":{"type":"realtime","model":"gpt-realtime"}}`))
	if err != nil {
		return voiceToken{}, fmt.Errorf("could not create Realtime token request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return voiceToken{}, fmt.Errorf("Realtime token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return voiceToken{}, fmt.Errorf("Realtime token request returned HTTP %d", response.StatusCode)
	}
	var token voiceToken
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&token); err != nil || token.Value == "" || token.Value == key || token.ExpiresAt <= time.Now().Unix() {
		return voiceToken{}, fmt.Errorf("invalid Realtime token response")
	}
	return token, nil
}

func voiceAPIKey(env, path string) (string, error) {
	if strings.TrimSpace(env) != "" {
		return env, nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read OPENAI_API_KEY from private.env")
	}
	defer file.Close()
	var key string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "export ")
		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != "OPENAI_API_KEY" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return "", fmt.Errorf("invalid OPENAI_API_KEY quoting in private.env")
			}
			value = value[1 : len(value)-1]
		}
		if strings.ContainsAny(value, " \t\r\n\"'`$") {
			return "", fmt.Errorf("OPENAI_API_KEY in private.env must be a literal value")
		}
		key = value
	}
	if scanner.Err() != nil {
		return "", fmt.Errorf("cannot read OPENAI_API_KEY from private.env")
	}
	return key, nil
}

func runVoiceToken(args []string) {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud voice-token")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	home, err := os.UserHomeDir()
	var key string
	if err == nil {
		key, err = voiceAPIKey(os.Getenv("OPENAI_API_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	}
	var token voiceToken
	if err == nil {
		token, err = mintVoiceToken(ctx, client, key)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(token)
}
