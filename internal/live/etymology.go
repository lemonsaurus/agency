package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WiktionaryAPI is where etymologies are checked.
var WiktionaryAPI = "https://en.wiktionary.org/w/api.php"

var languageHeading = regexp.MustCompile(`(?m)^==([^=].*?)==\s*$`)

// Etymology returns the etymology sections Wiktionary has for word in language (Spanish, English,
// Norwegian Bokmål, Latin...), as wikitext. It is the source every etymology claim is checked against.
func Etymology(ctx context.Context, word, language string) (string, error) {
	query := url.Values{"action": {"parse"}, "page": {word}, "prop": {"wikitext"}, "format": {"json"}, "formatversion": {"2"}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, WiktionaryAPI+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "agency-rosa/1.0 (Spanish tutor; etymology checks)")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("wiktionary is unreachable")
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	var page struct {
		Parse struct {
			Wikitext string `json:"wikitext"`
		} `json:"parse"`
		Error struct {
			Info string `json:"info"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &page) != nil || page.Parse.Wikitext == "" {
		return "", fmt.Errorf("wiktionary has no entry for %q", word)
	}
	text := page.Parse.Wikitext
	headings := languageHeading.FindAllStringSubmatchIndex(text, -1)
	var languages []string
	for i, heading := range headings {
		name := strings.TrimSpace(text[heading[2]:heading[3]])
		languages = append(languages, name)
		if !strings.EqualFold(name, language) {
			continue
		}
		end := len(text)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		var sections []string
		for _, part := range strings.Split(text[heading[1]:end], "\n===")[1:] {
			if strings.HasPrefix(part, "Etymology") {
				sections = append(sections, "==="+truncate(part, 1500))
			}
		}
		if len(sections) == 0 {
			return "", fmt.Errorf("wiktionary's %s entry for %q has no etymology", language, word)
		}
		return strings.Join(sections, "\n"), nil
	}
	return "", fmt.Errorf("wiktionary has %q only in: %s", word, strings.Join(languages, ", "))
}
