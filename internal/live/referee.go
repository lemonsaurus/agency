package live

import (
	"strings"
	"unicode"
)

// Verdicts on Lemon's attempt at a target sentence. Rosa may take the floor after right, attempt and
// unsure; partial means he is still building the answer.
const (
	verdictRight   = "right"
	verdictAttempt = "attempt"
	verdictUnsure  = "unsure"
	verdictPartial = "partial"
)

var accents = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n", "à", "a", "è", "e", "ò", "o")

var contractions = map[string]string{
	"its": "it is", "isnt": "is not", "dont": "do not", "doesnt": "does not", "im": "i am", "cant": "can not",
	"cannot": "can not", "wont": "will not", "youre": "you are", "theyre": "they are", "thats": "that is",
	"whats": "what is", "ive": "i have", "id": "i would", "arent": "are not", "wasnt": "was not",
}

var fillers = map[string]bool{
	"uh": true, "uhh": true, "uhm": true, "um": true, "umm": true, "hm": true, "hmm": true, "hmmm": true,
	"mm": true, "mmm": true, "eh": true, "ehm": true, "er": true, "erm": true, "ah": true, "ahh": true,
	"maybe": true, "like": true, "so": true, "okay": true, "ok": true, "wait": true, "well": true,
}

var backchannels = map[string]bool{
	"mm": true, "mmm": true, "mhm": true, "hm": true, "hmm": true, "aja": true, "aha": true, "uh": true, "huh": true,
	"si": true, "dale": true, "ok": true, "okay": true, "yeah": true, "yes": true, "claro": true, "bien": true, "eso": true,
}

var unsure = []string{
	"i do not know", "dunno", "no idea", "not sure", "no se", "ni idea", "no tengo idea", "no clue", "i forgot",
	"i have forgotten", "can not remember", "do not remember", "i give up", "no me acuerdo",
}

// words lowercases text, drops accents and punctuation, and expands English contractions.
func words(text string) []string {
	text = accents.Replace(strings.ToLower(text))
	text = strings.ReplaceAll(text, "'", "")
	text = strings.ReplaceAll(text, "’", "")
	fields := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []string
	for _, field := range fields {
		if expanded, ok := contractions[field]; ok {
			out = append(out, strings.Fields(expanded)...)
		} else {
			out = append(out, field)
		}
	}
	return out
}

// squash is the spoken content without fillers or spaces, so "cancelar lo" matches "cancelarlo".
func squash(tokens []string) []rune {
	var b strings.Builder
	for _, token := range tokens {
		if !fillers[token] {
			b.WriteString(token)
		}
	}
	return []rune(b.String())
}

// judge decides whether an attempt at sentence is right, a complete attempt, an "I don't know",
// or still partial. A trailing question mark ends an attempt, as a questioning inflection does; a
// trailing hesitation keeps it open. Right allows one slip per twelve letters for transcription.
func judge(attempt string, sentence Sentence) string {
	tokens := words(attempt)
	padded := " " + strings.Join(tokens, " ") + " "
	for _, phrase := range unsure {
		if strings.Contains(padded, " "+phrase+" ") {
			return verdictUnsure
		}
	}
	said := squash(tokens)
	for _, answer := range append([]string{sentence.ES}, sentence.Also...) {
		expected := squash(words(answer))
		if len(expected) > 0 && within(expected, said) <= len(expected)/12 {
			return verdictRight
		}
	}
	if strings.HasSuffix(strings.TrimSpace(attempt), "?") {
		return verdictAttempt
	}
	if trimmed := strings.TrimSpace(attempt); strings.HasSuffix(trimmed, "...") || strings.HasSuffix(trimmed, "…") || len(tokens) > 0 && fillers[tokens[len(tokens)-1]] {
		return verdictPartial
	}
	if expected := squash(words(sentence.ES)); len(said) > 0 && len(said)*4 >= len(expected)*3 {
		return verdictAttempt
	}
	return verdictPartial
}

// within is the smallest edit distance between pattern and any substring of text.
func within(pattern, text []rune) int {
	previous := make([]int, len(text)+1)
	current := make([]int, len(text)+1)
	for i := 1; i <= len(pattern); i++ {
		current[0] = i
		for j := 1; j <= len(text); j++ {
			cost := 1
			if pattern[i-1] == text[j-1] {
				cost = 0
			}
			current[j] = min(previous[j-1]+cost, previous[j]+1, current[j-1]+1)
		}
		previous, current = current, previous
	}
	best := len(pattern)
	for _, distance := range previous {
		best = min(best, distance)
	}
	return best
}

// cued reports where the English cue ends inside Rosa's turn, or -1. The cue's words must appear in
// order within a few words of each other, so a paraphrase with one extra word still counts.
func cued(turn []string, cue string) int {
	want := words(cue)
	if len(want) == 0 {
		return -1
	}
	end := -1
	for start := range turn {
		if turn[start] != want[0] {
			continue
		}
		next, last := 1, start
		for i := start + 1; i < len(turn) && i <= start+len(want)+2 && next < len(want); i++ {
			if turn[i] == want[next] {
				next, last = next+1, i
			}
		}
		if next == len(want) {
			end = last
		}
	}
	return end
}

// backchannel is a turn of at most two listening sounds.
func backchannel(turn []string) bool {
	if len(turn) > 2 {
		return false
	}
	for _, word := range turn {
		if !backchannels[word] {
			return false
		}
	}
	return true
}

// says reports whether Rosa's turn contains the Spanish answer itself.
func says(turn []string, answer string) bool {
	expected := squash(words(answer))
	return len(expected) > 0 && within(expected, squash(turn)) <= len(expected)/12
}
