package memory

import (
	"regexp"
	"strings"
)

// smallTalk matches messages that never need long-term memory.
var smallTalk = regexp.MustCompile(`(?i)^(hi|hello|hey|yo|thanks|thank you|thx|ok|okay|k|yes|yep|no|nope|sure|cool|great|nice|perfect|done|good (morning|afternoon|evening|night)|bye|goodbye|lgtm)( there| you| so much| all)?[\s!.?]*$`)

// RecallGate decides cheaply whether a query needs long-term memory. It
// returns why recall is skipped, or "" when it should run: greetings and
// acknowledgements, and queries without a word to match, skip the lookup.
func RecallGate(query string) string {
	q := strings.TrimSpace(query)
	switch {
	case q == "":
		return "empty query"
	case smallTalk.MatchString(q):
		return "small talk"
	case len(Words(q)) == 0:
		return "no words to look up"
	}
	return ""
}
