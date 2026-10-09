package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Reminders is the box's one-off reminder store. The file holds the reminders, Lemon's zone and the
// phone's push token. The phone owns its alarm; `agency cloud watch` reads the file to fire
// connected desktops.
type Reminders struct {
	Push *Pusher // nil without an FCM key

	mu   sync.Mutex
	path string // empty keeps everything in memory
	data reminderFile
	emit func(Update)
}

type Reminder struct {
	ID    int    `json:"id"`
	At    int64  `json:"at"` // Unix milliseconds
	Text  string `json:"text"`
	Armed bool   `json:"armed,omitempty"` // the phone has the alarm
}

type reminderFile struct {
	Zone      string     `json:"zone,omitempty"`
	Token     string     `json:"token,omitempty"`
	Next      int        `json:"next"`
	Reminders []Reminder `json:"reminders"`
}

func OpenReminders(path string, emit func(Update)) *Reminders {
	r := &Reminders{path: path, emit: emit}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &r.data)
	}
	return r
}

// Zone is Lemon's, from the phone; the box's own until the phone has said.
func (r *Reminders) Zone() *time.Location {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data.Zone != "" {
		if zone, err := time.LoadLocation(r.data.Zone); err == nil {
			return zone
		}
	}
	return time.Local
}

func (r *Reminders) SetZone(name string) {
	if _, err := time.LoadLocation(name); name == "" || err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data.Zone != name {
		r.data.Zone = name
		r.save()
	}
}

func (r *Reminders) SetToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token != "" && r.data.Token != token {
		r.data.Token = token
		r.save()
	}
}

// Add stores a reminder for when (see parseWhen) and wakes the phone.
func (r *Reminders) Add(when, text string, now time.Time) (map[string]any, error) {
	at, err := parseWhen(when, now)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("say what the reminder is for")
	}
	r.mu.Lock()
	r.data.Next++
	reminder := Reminder{ID: r.data.Next, At: at.UnixMilli(), Text: truncate(text, 200)}
	var kept []Reminder
	for _, old := range r.data.Reminders {
		if old.At > now.Add(-time.Hour).UnixMilli() {
			kept = append(kept, old)
		}
	}
	r.data.Reminders = append(kept, reminder)
	r.save()
	token := r.data.Token
	r.mu.Unlock()
	if token != "" {
		go r.wake(token, "reminders")
	}
	return map[string]any{"id": reminder.ID, "result": "Reminder set for " + at.Format("Monday 2 January, 15:04 MST") + ": the phone and any connected desktop notify Lemon then."}, nil
}

// parseWhen reads "+20m" (a Go duration from now) or a local wall clock "2006-01-02T15:04".
func parseWhen(when string, now time.Time) (time.Time, error) {
	if span, ok := strings.CutPrefix(when, "+"); ok {
		d, err := time.ParseDuration(span)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("use +20m or YYYY-MM-DDTHH:MM in local time")
		}
		return now.Add(d), nil
	}
	at, err := time.ParseInLocation("2006-01-02T15:04", when, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("use +20m or YYYY-MM-DDTHH:MM in local time")
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("that time has already passed; it is %s", clock(now))
	}
	return at, nil
}

// Wake tells the phone to sync what, if it has registered.
func (r *Reminders) Wake(what string) {
	r.mu.Lock()
	token := r.data.Token
	r.mu.Unlock()
	if token != "" {
		r.wake(token, what)
	}
}

// wake tells the phone to sync what; an unregistered token is forgotten.
func (r *Reminders) wake(token, what string) {
	if r.Push == nil {
		return
	}
	err := r.Push.Send(token, what)
	if err == nil {
		return
	}
	log.Printf("push %s: %v", what, err)
	if errors.Is(err, ErrTokenGone) {
		r.mu.Lock()
		if r.data.Token == token {
			r.data.Token = ""
			r.save()
		}
		r.mu.Unlock()
	}
}

// Pending is what the phone has not armed yet and that is still ahead.
func (r *Reminders) Pending(now time.Time) []Reminder {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := []Reminder{}
	for _, reminder := range r.data.Reminders {
		if !reminder.Armed && reminder.At > now.UnixMilli() {
			pending = append(pending, reminder)
		}
	}
	return pending
}

// Upcoming is every reminder still ahead, armed or not, so a reinstalled phone can arm them again.
func (r *Reminders) Upcoming(now time.Time) []Reminder {
	r.mu.Lock()
	defer r.mu.Unlock()
	upcoming := []Reminder{}
	for _, reminder := range r.data.Reminders {
		if reminder.At > now.UnixMilli() {
			upcoming = append(upcoming, reminder)
		}
	}
	return upcoming
}

// Done records the phone's outcome; a failure is said out loud.
func (r *Reminders) Done(id int, errText string) {
	r.mu.Lock()
	text := ""
	for i := range r.data.Reminders {
		if reminder := &r.data.Reminders[i]; reminder.ID == id && !reminder.Armed {
			reminder.Armed = true
			text = reminder.Text
			r.save()
		}
	}
	r.mu.Unlock()
	if text != "" && errText != "" {
		r.emit(Update{true, "The reminder \"" + truncate(text, 80) + "\" did not get onto the phone: " + truncate(errText, 200)})
	}
}

// save writes the file; callers hold mu.
func (r *Reminders) save() {
	if r.path == "" {
		return
	}
	data, _ := json.Marshal(r.data)
	tmp := r.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err == nil {
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, r.path)
		}
		if err != nil {
			log.Printf("reminders: save: %v", err)
		}
	}
}

// DueReminders reads the store at path and returns reminders that came due in (after, upto].
func DueReminders(path string, after, upto time.Time) []Reminder {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var data reminderFile
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	var due []Reminder
	for _, reminder := range data.Reminders {
		if reminder.At > after.UnixMilli() && reminder.At <= upto.UnixMilli() {
			due = append(due, reminder)
		}
	}
	return due
}
