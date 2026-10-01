package live

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrTokenGone means FCM no longer knows the phone's token.
var ErrTokenGone = errors.New("push token is no longer registered")

// Pusher wakes the phone through Firebase Cloud Messaging. The message carries no reminder text:
// the phone fetches reminders from the box over SSH.
type Pusher struct {
	API    string // https://fcm.googleapis.com
	client *http.Client
	key    *rsa.PrivateKey
	email  string
	tokens string // OAuth token endpoint
	app    string // Firebase project id

	mu      sync.Mutex
	access  string
	expires time.Time
}

// NewPusher reads a Google service-account key file.
func NewPusher(keyPath string) (*Pusher, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	var account struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &account); err != nil {
		return nil, fmt.Errorf("invalid service-account key: %v", err)
	}
	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("service-account key has no private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, fmt.Errorf("service-account private key is not RSA PKCS#8")
	}
	return &Pusher{API: "https://fcm.googleapis.com", client: &http.Client{Timeout: 15 * time.Second}, key: key,
		email: account.ClientEmail, tokens: account.TokenURI, app: account.ProjectID}, nil
}

// Send delivers a high-priority data message that makes the phone sync reminders.
func (p *Pusher) Send(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	access, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token":   token,
		"data":    map[string]string{"sync": "reminders"},
		"android": map[string]any{"priority": "HIGH", "collapse_key": "reminders"},
	}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.API+"/v1/projects/"+p.app+"/messages:send", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("fcm request failed")
	}
	defer response.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	case response.StatusCode == http.StatusNotFound:
		return ErrTokenGone
	}
	return fmt.Errorf("fcm HTTP %d: %s", response.StatusCode, truncate(string(reply), 300))
}

// accessToken signs a JWT with the service-account key and trades it for an OAuth token.
func (p *Pusher) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.access != "" && time.Now().Before(p.expires) {
		return p.access, nil
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": p.email, "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": p.tokens, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	encode := base64.RawURLEncoding.EncodeToString
	signing := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {signing + "." + encode(signature)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokens, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("google token request failed")
	}
	defer response.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	var token struct {
		Access  string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(reply, &token) != nil || token.Access == "" {
		return "", fmt.Errorf("google token HTTP %d: %s", response.StatusCode, truncate(string(reply), 300))
	}
	p.access, p.expires = token.Access, now.Add(time.Duration(token.Expires-60)*time.Second)
	return p.access, nil
}
