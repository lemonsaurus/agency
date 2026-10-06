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
	// A complete attempt with English where Spanish words are missing: a vocabulary gap, not a miss.
	verdictGap = "gap"
	// English with no Spanish in it: he is asking to hear the phrase again or checking it. Never an answer.
	verdictClarify = "clarify"
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

// englishWords and spanish mark which language a word is in, for code-switch detection. Words both
// languages share (no, me, a, he, come, sea, red, real) are in neither.
var englishWords = setOf("the to of and in is it you that was for on are with as i his they be at one have this from or had by but what " +
	"some we can out other were all there when up use your how said an each she which do their time if will way about many then them " +
	"would like these her long make thing see him two has look more day could go did number sound most people my over know water than " +
	"call first who may down been now find any new work part take get place made live where after back little only man year came show " +
	"every good our under name very through just great think say help line turn much mean before move right too same tell does set " +
	"three want well also play small end put home read hand large add even here must big high such follow why ask change went light " +
	"kind off need house try us again point mother world near build father head stand own page should country found answer school grow " +
	"study still learn keep never last let thought city hard start might story saw far left late run while close night life few open " +
	"seem together next white children begin got walk example ease paper often always music those both mark book until mile river feel " +
	"talk bird soon body dog family song door friend fast faster quickly slowly really different topics topic things stuff because")

var spanish = setOf("quiero queres quieres es pero ahora yo vos tu te lo la el los las un una uno de del en con por para que muy mas y o " +
	"sobre porque cuando como donde estoy esta estas soy sos eres tengo tenes tienes puedo podes puedes voy vas hablar hablo intento hay " +
	"eso esto este mi su nos les le se ya tambien bien mucho muchos muchas poco hoy manana ayer todo nada algo siempre nunca aqui alla " +
	"tambien entonces creo sabes sabe sé fue era estaba tiene hace hacer ser estar tener ir decir")

func setOf(text string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(text) {
		set[word] = true
	}
	return set
}

// spanishEnding marks words that only Spanish builds: -ción, -mente, and verbs with lo, la, me, te or se
// hooked on.
func spanishEnding(word string) bool {
	for _, ending := range []string{"cion", "mente", "arlo", "erlo", "irlo", "arla", "arme", "arte", "arse", "erse", "irse"} {
		if strings.HasSuffix(word, ending) {
			return true
		}
	}
	return false
}

// english is whether a turn is English with no Spanish in it: at least one English word and nothing
// that only Spanish says.
func english(tokens []string) bool {
	en := 0
	for _, word := range tokens {
		switch {
		case fillers[word] || backchannels[word]:
		case spanish[word] || spanishEnding(word):
			return false
		case englishWords[word] || strings.HasSuffix(word, "ly") || strings.HasSuffix(word, "ing") || strings.Contains(word, "th") || strings.ContainsAny(word, "wk"):
			en++
		}
	}
	return en > 0
}

// switched is whether a mainly Spanish turn carries English content words: a quiet request for the
// Spanish he is missing. It takes two Spanish words and no more English words than the rest, so an
// English question quoting "el mundo" doesn't count; fillers and listening sounds count as neither.
func switched(text string) bool {
	es, en, other := 0, 0, 0
	for _, word := range words(text) {
		switch {
		case fillers[word] || backchannels[word]:
		case spanish[word] || spanishEnding(word):
			es++
		case englishWords[word] || strings.HasSuffix(word, "ly") || strings.HasSuffix(word, "ing") || strings.Contains(word, "th") || strings.ContainsAny(word, "wk"):
			en++
		default:
			other++
		}
	}
	return es > 1 && en > 0 && es+other >= en
}

var unsure = []string{
	"i do not know", "dunno", "no idea", "not sure", "no se", "ni idea", "no tengo idea", "no clue", "i forgot",
	"i have forgotten", "can not remember", "do not remember", "i give up", "no me acuerdo",
	"how do i say", "how do you say", "what is the word", "what was the word", "como se dice",
}

// improvised finds a translation prompt Rosa made up herself, "how would you say X?" or "say X", in
// the last sentence of her turn (or the one before a short tail like "dale"), and returns X. A
// pronoun like "that" or "it" points back at a word, so X is empty. Questions about Spanish ("where's
// the stress?") are not translation prompts.
func improvised(turn string) (string, bool) {
	sentences := strings.FieldsFunc(turn, func(r rune) bool { return r == '.' || r == '?' || r == '!' })
	for i := len(sentences) - 1; i >= 0 && i >= len(sentences)-2; i-- {
		sentence := " " + strings.Join(words(sentences[i]), " ") + " "
		for _, lead := range []string{" how would you say ", " how do you say ", " how d you say ", " how would you translate "} {
			if at := strings.LastIndex(sentence, lead); at >= 0 {
				return prompted(sentence[at+len(lead):]), true
			}
		}
		if trimmed := strings.TrimSpace(sentence); strings.HasPrefix(trimmed, "say ") || strings.HasPrefix(trimmed, "now say ") || strings.HasPrefix(trimmed, "okay say ") || strings.HasPrefix(trimmed, "so say ") || strings.HasPrefix(trimmed, "and say ") {
			return prompted(trimmed[strings.Index(trimmed, "say ")+4:]), true
		}
		if len(strings.Fields(sentence)) > 3 {
			break
		}
	}
	return "", false
}

func prompted(rest string) string {
	rest = strings.TrimSpace(rest)
	if rest == "that" || rest == "it" || rest == "this" || rest == "that one" {
		return ""
	}
	return rest
}

// addressed is whether Lemon is talking to Rosa rather than answering: checking she is there, or
// asking her something.
func addressed(text string) bool {
	padded := " " + strings.Join(words(text), " ") + " "
	for _, phrase := range []string{" hello ", " hola ", " rosa ", " are you there ", " you there ", " dropped off ", " can you hear ", " me escuchas ", " me oyes ", " what happened ", " you still there "} {
		if strings.Contains(padded, phrase) {
			return true
		}
	}
	return false
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

// judge decides whether an attempt at sentence is right, a complete attempt, a complete attempt
// with English filling a vocabulary gap, an "I don't know", or still partial. English words count
// toward the length of an attempt and never end it. A trailing question mark ends an attempt, as a questioning inflection does; a
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
		if contains(tokens, answer) {
			return verdictRight
		}
	}
	if english(tokens) {
		return verdictClarify
	}
	trimmed := strings.TrimSpace(attempt)
	trailing := strings.HasSuffix(trimmed, "...") || strings.HasSuffix(trimmed, "…") || len(tokens) > 0 && fillers[tokens[len(tokens)-1]]
	expected := squash(words(sentence.ES))
	// An improvised prompt has no expected Spanish, so only a trailing hesitation keeps it open.
	long := len(said) > 0 && (sentence.ES == "" || len(said)*4 >= len(expected)*3)
	if strings.HasSuffix(trimmed, "?") || !trailing && long {
		if switched(attempt) {
			return verdictGap
		}
		return verdictAttempt
	}
	return verdictPartial
}

// contains is whether some run of his words sounds like answer. The transcript is speech
// recognition, so spelling slips (doubled letters, a stray letter, anglicised spelling) never count:
// words are compared by sound, word for word with a slip per six letters, or joined up exactly when
// the recogniser split or merged words ("cancelar lo"). A difference counts only when it is audible:
// a missing or extra word or syllable, a word out of place, a plural ending, a swapped or dropped
// final vowel.
func contains(tokens []string, answer string) bool {
	expected := words(answer)
	if len(expected) == 0 {
		return false
	}
	var said []string
	for _, token := range tokens {
		if !fillers[token] {
			said = append(said, sound(token))
		}
	}
	want := make([]string, len(expected))
	joined := ""
	for k, word := range expected {
		want[k] = sound(word)
		joined += want[k]
	}
	for i := range said {
		if i+len(want) <= len(said) && alike(said[i:i+len(want)], want) {
			return true
		}
		run := ""
		for j := i; j < len(said) && len(run) <= len(joined)+1; j++ {
			run += said[j]
			if j-i+1 == len(want) {
				continue
			}
			if run == joined {
				return true
			}
		}
	}
	return false
}

// alike is whether each word sounds like its expected word, allowing a slip per six letters but no
// change of form.
func alike(said, want []string) bool {
	for k := range want {
		if said[k] == want[k] {
			continue
		}
		if reformed(said[k], want[k]) || distance([]rune(said[k]), []rune(want[k])) > len(want[k])/6 {
			return false
		}
	}
	return true
}

// reformed is whether a differs from b by a real change of form: a plural ending added or dropped,
// or the final vowel swapped or dropped (diferentes, diferenta, diferent for diferente).
func reformed(a, b string) bool {
	if a == b || len(a) < 3 || len(b) < 3 {
		return false
	}
	if a == b+"s" || b == a+"s" || a == b+"es" || b == a+"es" {
		return true
	}
	if vowelEnd(b) && a == b[:len(b)-1] || vowelEnd(a) && b == a[:len(a)-1] {
		return true
	}
	return len(a) == len(b) && a[:len(a)-1] == b[:len(b)-1] && strings.ContainsRune("aeo", rune(a[len(a)-1])) && strings.ContainsRune("aeo", rune(b[len(b)-1]))
}

func vowelEnd(word string) bool {
	return len(word) > 0 && strings.ContainsRune("aeiou", rune(word[len(word)-1]))
}

// sound is a word as Spanish says it: silent h dropped, b and v merged, soft c and z as s, hard c
// and qu as k, ll as y, doubled letters collapsed.
func sound(word string) string {
	word = soundShifts.Replace(word)
	var b strings.Builder
	var last rune
	for _, r := range word {
		if r == 'h' || r == last {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return b.String()
}

var soundShifts = strings.NewReplacer("ch", "C", "qu", "k", "ce", "se", "ci", "si", "c", "k", "z", "s", "v", "b", "ll", "y", "ge", "je", "gi", "ji", "x", "ks", "w", "u")

// distance is the edit distance between a and b.
func distance(a, b []rune) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j-1]+cost, previous[j]+1, current[j-1]+1)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

// hints is whether Rosa's turn gives away a Spanish word of the expected answer that the English cue
// doesn't already contain.
func hints(turn []string, sentence Sentence) bool {
	cue := map[string]bool{}
	for _, word := range words(sentence.EN) {
		cue[word] = true
	}
	said := map[string]bool{}
	for _, word := range turn {
		said[word] = true
	}
	for _, word := range words(sentence.ES) {
		if len(word) >= 4 && !cue[word] && said[word] {
			return true
		}
	}
	return false
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
	return contains(turn, answer)
}
