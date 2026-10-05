package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

var personaFiles = []string{"IDENTITY.md", "SOUL.md", "LANGUAGE.md"}

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

// privateKey reads one literal KEY=value from private.env; the environment wins.
func privateKey(variable, env, path string) (string, error) {
	if strings.TrimSpace(env) != "" {
		return env, nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read %s from private.env", variable)
	}
	defer file.Close()
	var key string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "export ")
		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != variable {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return "", fmt.Errorf("invalid %s quoting in private.env", variable)
			}
			value = value[1 : len(value)-1]
		}
		if strings.ContainsAny(value, " \t\r\n\"'`$") {
			return "", fmt.Errorf("%s in private.env must be a literal value", variable)
		}
		key = value
	}
	if scanner.Err() != nil {
		return "", fmt.Errorf("cannot read %s from private.env", variable)
	}
	return key, nil
}

var realtimeVoices = []string{"marin", "cedar", "sage", "coral", "shimmer", "ballad", "verse", "alloy", "ash", "echo"}

var accentPattern = regexp.MustCompile(`^[A-Za-z ]{0,30}$`)

const sampleLine = "Hey Lemon. Build's green, tests pass, and I already fixed the thing you were about to ask about. Ya nerd."

// runSpend prints GPT-Live spend in dollars for the current billing month from the OpenAI costs
// API, which needs an admin key.
func runSpend() {
	home, _ := os.UserHomeDir()
	key, err := privateKey("OPENAI_ADMIN_KEY", os.Getenv("OPENAI_ADMIN_KEY"), filepath.Join(home, ".pi", "agent", "private.env"))
	if err == nil && key == "" {
		err = fmt.Errorf("add an OpenAI admin key as OPENAI_ADMIN_KEY to ~/.pi/agent/private.env")
	}
	now := time.Now().UTC()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var usd float64
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		usd, err = liveSpend(ctx, http.DefaultClient, "https://api.openai.com", key, since)
	}
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(map[string]any{"usd": usd, "since": since.Format("2006-01-02")})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// liveSpend sums the costs API's gpt-live line items since the given time, following pages.
func liveSpend(ctx context.Context, client *http.Client, base, key string, since time.Time) (float64, error) {
	total := 0.0
	page := ""
	for {
		query := url.Values{"start_time": {strconv.FormatInt(since.Unix(), 10)}, "group_by": {"line_item"}, "limit": {"31"}}
		if page != "" {
			query.Set("page", page)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/organization/costs?"+query.Encode(), nil)
		if err != nil {
			return 0, err
		}
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(request)
		if err != nil {
			return 0, err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
		response.Body.Close()
		if err != nil {
			return 0, err
		}
		if response.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("OpenAI costs API returned %s: %.300s", response.Status, data)
		}
		var body struct {
			Data []struct {
				Results []struct {
					LineItem string `json:"line_item"`
					Amount   struct {
						Value json.Number `json:"value"`
					} `json:"amount"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool   `json:"has_more"`
			NextPage string `json:"next_page"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return 0, fmt.Errorf("invalid OpenAI costs reply")
		}
		for _, bucket := range body.Data {
			for _, result := range bucket.Results {
				if !strings.HasPrefix(strings.ToLower(result.LineItem), "gpt-live") {
					continue
				}
				value, err := result.Amount.Value.Float64()
				if err != nil {
					return 0, fmt.Errorf("invalid OpenAI cost amount")
				}
				total += value
			}
		}
		if !body.HasMore || body.NextPage == "" {
			return total, nil
		}
		page = body.NextPage
	}
}
