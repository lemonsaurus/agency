package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

const maxFileBytes = 5 * 1024 * 1024

func mobileFileRoot(home, path string) string {
	for _, name := range []string{"git", ".agents"} {
		root := filepath.Join(home, name)
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

func fileRead(home, path string, output io.Writer) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || mobileFileRoot(home, path) == "" {
		return fmt.Errorf("file must be an absolute path under ~/git or ~/.agents")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rootPath := mobileFileRoot(home, resolved)
	if rootPath == "" {
		return fmt.Errorf("file resolves outside ~/git and ~/.agents")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return fmt.Errorf("file must be regular and at most 5 MiB")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(strings.TrimPrefix(resolved, rootPath+string(filepath.Separator)))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return fmt.Errorf("file must be regular and at most 5 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxFileBytes {
		return fmt.Errorf("file exceeds 5 MiB")
	}
	mime := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".svg":
		mime = "image/svg+xml"
	case ".md":
		mime = "text/markdown"
	case ".txt":
		mime = "text/plain"
	case ".json":
		mime = "application/json"
	}
	metadata := struct {
		Name string `json:"name"`
		Size int    `json:"size"`
		MIME string `json:"mime"`
	}{filepath.Base(path), len(data), mime}
	if err := json.NewEncoder(output).Encode(metadata); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, base64.StdEncoding.EncodeToString(data))
	return err
}

func runFile(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud file <absolute-path>")
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		err = fileRead(home, args[0], os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

var personaFiles = []string{"IDENTITY.md", "SOUL.md", "SLOP.md"}

func persona(dir string) (string, error) {
	var parts []string
	for _, name := range personaFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("cannot read %s from ~/.agents", name)
		}
		parts = append(parts, strings.TrimSpace(string(data)))
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}

func runPersona(args []string) {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud persona")
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	var text string
	if err == nil {
		text, err = persona(filepath.Join(home, ".agents"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Print(text)
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

// createLiveSession forwards a GPT-Live session request from the phone so the API key never leaves the box.
func createLiveSession(ctx context.Context, client *http.Client, key, body string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not set in the environment")
	}
	var request struct {
		Session   json.RawMessage `json:"session"`
		Transport struct {
			Type string `json:"type"`
			SDP  string `json:"sdp"`
		} `json:"transport"`
	}
	if len(body) > 256*1024 || json.Unmarshal([]byte(body), &request) != nil || len(request.Session) == 0 || request.Transport.Type != "webrtc" || request.Transport.SDP == "" {
		return "", fmt.Errorf("invalid live session request")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/live/sessions", strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("could not create live session request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+key)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("live session request failed")
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if err != nil {
		return "", fmt.Errorf("live session reply unreadable")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(reply, &failure)
		message := strings.ReplaceAll(failure.Error.Message, key, "[key]")
		if len(message) > 300 {
			message = message[:300]
		}
		return "", fmt.Errorf("live session request returned HTTP %d: %s", response.StatusCode, message)
	}
	var answer struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		Transport struct {
			SDP string `json:"sdp"`
		} `json:"transport"`
	}
	if json.Unmarshal(reply, &answer) != nil || answer.Session.ID == "" || answer.Transport.SDP == "" {
		return "", fmt.Errorf("invalid live session reply")
	}
	return string(reply), nil
}

func runLiveSession(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud live-session <json>")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	home, err := os.UserHomeDir()
	var key string
	if err == nil {
		key, err = voiceAPIKey(os.Getenv("OPENAI_API_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	}
	var reply string
	if err == nil {
		reply, err = createLiveSession(ctx, client, key, args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Println(reply)
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

var realtimeVoices = []string{"marin", "cedar", "sage", "coral", "shimmer", "ballad", "verse", "alloy", "ash", "echo"}

var accentPattern = regexp.MustCompile(`^[A-Za-z ]{0,30}$`)

const sampleLine = "Hey Lemon. Build's green, tests pass, and I already fixed the thing you were about to ask about. Ya nerd."

func voiceInstructions(accent string) string {
	text := "Carla: sharp, warm, self-possessed, a little amused. Natural pace, no customer-service brightness."
	if accent != "" {
		text = "Speak " + accent + " English with a clear, authentic accent. " + text
	}
	return text
}

func voiceSample(ctx context.Context, client *http.Client, key, voice, accent string) ([]byte, error) {
	if !slices.Contains(realtimeVoices, voice) {
		return nil, fmt.Errorf("unknown voice")
	}
	if !accentPattern.MatchString(accent) {
		return nil, fmt.Errorf("invalid accent")
	}
	body, _ := json.Marshal(map[string]string{
		"model": "gpt-4o-mini-tts", "voice": voice, "input": sampleLine,
		"instructions": voiceInstructions(strings.TrimSpace(accent)), "response_format": "mp3",
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/audio/speech", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("could not create speech request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("speech request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("speech request returned HTTP %d", response.StatusCode)
	}
	audio, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil || len(audio) == 0 {
		return nil, fmt.Errorf("invalid speech response")
	}
	return audio, nil
}

func runVoiceSample(args []string) {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Usage: agency cloud voice-sample <voice> [accent]")
		os.Exit(1)
	}
	accent := ""
	if len(args) == 2 {
		accent = args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	home, err := os.UserHomeDir()
	var key string
	if err == nil {
		key, err = voiceAPIKey(os.Getenv("OPENAI_API_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	}
	if err == nil && strings.TrimSpace(key) == "" {
		err = fmt.Errorf("OPENAI_API_KEY is not set in the environment")
	}
	var audio []byte
	if err == nil {
		audio, err = voiceSample(ctx, client, key, args[0], accent)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(audio))
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
