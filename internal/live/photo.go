package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"
)

// ImagesURL is the ChatGPT plan's gpt-image-2 route, the one Pi's openai_generate_image uses.
const ImagesURL = "https://chatgpt.com/backend-api/codex/images"

// Photographer draws Rosa's photos with gpt-image-2 on Lemon's ChatGPT plan. Photos she appears in
// are edits of her reference look, so her face, hair, piercings and tattoos stay the same.
type Photographer struct {
	Client *http.Client
	URL    string
	Auth   func(ctx context.Context) (token, account string, err error)
}

// Take returns a portrait JPEG of scene; with look, a JPEG of Rosa, she is in it.
func (p *Photographer) Take(ctx context.Context, scene string, look []byte) ([]byte, error) {
	token, account, err := p.Auth(ctx)
	if err != nil {
		return nil, err
	}
	route := "/generations"
	fields := map[string]any{"model": "gpt-image-2", "prompt": scene, "n": 1, "size": "1024x1536", "quality": "medium", "background": "auto"}
	if look != nil {
		route = "/edits"
		fields["images"] = []map[string]string{{"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(look)}}
	}
	body, _ := json.Marshal(fields)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", account)
	request.Header.Set("originator", "agency")
	request.Header.Set("Content-Type", "application/json")
	response, err := p.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("image request failed")
	}
	defer response.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("images HTTP %d: %s", response.StatusCode, truncate(strings.ReplaceAll(string(reply), token, "[token]"), 300))
	}
	var result struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(reply, &result) != nil || len(result.Data) != 1 {
		return nil, fmt.Errorf("images returned no picture")
	}
	raw, err := base64.StdEncoding.DecodeString(result.Data[0].B64)
	if err != nil {
		return nil, fmt.Errorf("images returned a broken picture")
	}
	return toJPEG(raw)
}

// toJPEG re-encodes a picture as a JPEG small enough for the phone to fetch over SSH.
func toJPEG(raw []byte) ([]byte, error) {
	picture, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("unreadable picture: %v", err)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, picture, &jpeg.Options{Quality: 84}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// PhotoPrompt turns a scene Rosa describes into an image prompt: a phone photo from her day.
func PhotoPrompt(scene string, selfie bool) string {
	style := " Realistic, unretouched smartphone photo, as a 34-year-old porteña would snap it in Buenos Aires: natural light, slight grain and blur, casual framing, nothing staged or glossy. No text overlays."
	if selfie {
		return "The woman in the reference photo, Rosa: keep her face, short choppy hot-pink hair with the shaved side, silver hoop in her left nostril, silver septum ring and her tattoos exactly the same. Clothes and setting follow the scene. Scene: " + scene + style
	}
	return "Scene: " + scene + " Rosa is behind the camera and not in the picture, apart from at most her tattooed hand or forearm." + style
}
