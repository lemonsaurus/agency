package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	noteRate = 24000
	noteMax  = 90 * time.Second
)

// VoiceNote is a recorded message: 24 kHz mono 16-bit WAV, its length, and bar heights from 0 to 1
// for the phone's waveform.
type VoiceNote struct {
	WAV      []byte
	Duration time.Duration
	Peaks    []float64
}

// RecordNote has a GPT-Live session over its own WebSocket speak script in voice and keeps the audio.
// Live voices exist only in Live sessions, so this is the one way to get Rosa's real voice offline.
// The session hears silence the whole time, as a primary WebSocket must keep input running.
func RecordNote(ctx context.Context, api, key, voice, persona, script string) (VoiceNote, error) {
	ctx, cancel := context.WithTimeout(ctx, noteMax+30*time.Second)
	defer cancel()
	url := strings.Replace(api, "http", "ws", 1) + "/v1/live/sessions"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		return VoiceNote{}, fmt.Errorf("voice note connect failed: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	send := func(event map[string]any) error {
		data, _ := json.Marshal(event)
		return conn.Write(ctx, websocket.MessageText, data)
	}
	instructions := persona + "\n\n# Voice note\n\nYou are recording a voice message on your phone for Lemon to listen to later. Nobody is on the line and nobody will answer. When you get the script, say it exactly and in full, in your own voice, accent and energy, like a real voice note: natural pace, real breaths, Spanish as a porteña says it. Add nothing before or after it, never respond to silence, and stay silent once it is said."
	err = send(map[string]any{"type": "session.start", "event_id": "note_start", "session": map[string]any{
		"model":        "gpt-live-1",
		"instructions": instructions,
		"audio":        map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": noteRate}, "output": map[string]any{"voice": voice}},
		"delegation":   map[string]any{"type": "client"},
	}})
	if err != nil {
		return VoiceNote{}, err
	}
	type event struct {
		Type    string `json:"type"`
		Delta   string `json:"delta"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	events := make(chan event, 256)
	failed := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				failed <- err
				return
			}
			var e event
			if json.Unmarshal(data, &e) == nil {
				events <- e
			}
		}
	}()
	silence := base64.StdEncoding.EncodeToString(make([]byte, noteRate/10*2))
	started, asked := false, false
	var audio bytes.Buffer
	var said strings.Builder
	var lastAudio time.Time
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(noteMax)
	for {
		select {
		case <-ctx.Done():
			return VoiceNote{}, fmt.Errorf("voice note timed out")
		case err := <-failed:
			return VoiceNote{}, fmt.Errorf("voice note connection closed: %v", err)
		case e := <-events:
			switch e.Type {
			case "session.started":
				started = true
			case "session.output_audio.delta":
				if chunk, err := base64.StdEncoding.DecodeString(e.Delta); err == nil {
					audio.Write(chunk)
					if loud(chunk) {
						lastAudio = time.Now()
					}
				}
			case "session.output_transcript.delta":
				said.WriteString(e.Delta)
			case "error":
				return VoiceNote{}, fmt.Errorf("voice note failed: %s", truncate(e.Error.Message+e.Message, 300))
			case "session.closed":
				return VoiceNote{}, fmt.Errorf("voice note session closed early")
			}
		case <-tick.C:
			if !started {
				continue
			}
			if err := send(map[string]any{"type": "session.input_audio.append", "audio": silence}); err != nil {
				return VoiceNote{}, err
			}
			if !asked {
				asked = true
				err := send(map[string]any{"type": "session.instructions.append", "event_id": "note_script", "delegation_id": nil,
					"content": "Immediately record the voice message now. Say exactly this and in full, then stop: " + script})
				if err != nil {
					return VoiceNote{}, err
				}
			}
			quiet := time.Since(lastAudio)
			done := !lastAudio.IsZero() && (quiet > 4*time.Second || quiet > 1500*time.Millisecond && covered(said.String(), script) >= .85)
			if done || time.Now().After(deadline) {
				send(map[string]any{"type": "session.close"})
				if audio.Len() == 0 {
					return VoiceNote{}, fmt.Errorf("voice note came back silent")
				}
				return encodeNote(audio.Bytes()), nil
			}
		}
	}
}

// covered is the share of the script's words that the spoken transcript has said.
func covered(said, script string) float64 {
	want := words(script)
	if len(want) == 0 {
		return 1
	}
	have := map[string]int{}
	for _, word := range words(said) {
		have[word]++
	}
	found := 0
	for _, word := range want {
		if have[word] > 0 {
			have[word]--
			found++
		}
	}
	return float64(found) / float64(len(want))
}

// encodeNote trims the silence around the speech and wraps it as WAV with its waveform.
func encodeNote(pcm []byte) VoiceNote {
	samples := make([]int16, len(pcm)/2)
	binary.Read(bytes.NewReader(pcm[:len(samples)*2]), binary.LittleEndian, samples)
	first, last := 0, len(samples)-1
	for first < len(samples) && abs16(samples[first]) < 300 {
		first++
	}
	for last > first && abs16(samples[last]) < 300 {
		last--
	}
	pad := noteRate / 5
	first, last = max(first-pad, 0), min(last+pad, len(samples)-1)
	samples = samples[first : last+1]
	const bars = 48
	peaks := make([]float64, bars)
	top := 0.0
	for i := range peaks {
		from, to := i*len(samples)/bars, (i+1)*len(samples)/bars
		sum := 0.0
		for _, s := range samples[from:to] {
			sum += float64(s) * float64(s)
		}
		if to > from {
			peaks[i] = math.Sqrt(sum / float64(to-from))
		}
		top = math.Max(top, peaks[i])
	}
	for i := range peaks {
		if top > 0 {
			peaks[i] = math.Round(math.Max(peaks[i]/top, .08)*100) / 100
		}
	}
	var wav bytes.Buffer
	size := uint32(len(samples) * 2)
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, 36+size)
	wav.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), uint32(noteRate), uint32(noteRate * 2), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, field)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, size)
	binary.Write(&wav, binary.LittleEndian, samples)
	return VoiceNote{WAV: wav.Bytes(), Duration: time.Duration(len(samples)) * time.Second / noteRate, Peaks: peaks}
}

// loud is whether a PCM chunk holds speech rather than the silence Live streams between turns.
func loud(chunk []byte) bool {
	for i := 0; i+1 < len(chunk); i += 2 {
		if abs16(int16(binary.LittleEndian.Uint16(chunk[i:]))) >= 600 {
			return true
		}
	}
	return false
}

func abs16(s int16) int {
	if s < 0 {
		return -int(s)
	}
	return int(s)
}
