package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// linkedConversation remembers an open Gemini conversation thread so a
// follow-up turn from a history-replaying client (Open WebUI, or any other
// OpenAI-compatible client) can continue it instead of resending the whole
// transcript as text on every call.
type linkedConversation struct {
	ConversationID string
	ResponseID     string
	ChoiceID       string
	WorkerID       string
	expiresAt      time.Time
}

// conversationLinkTTL bounds both memory growth and the window in which two
// different chats whose transcripts happen to collide (see
// conversationFingerprint) could bleed into each other. Kept short
// deliberately: this is a latency optimization, not a durability guarantee,
// so there's no reason to hold links longer than a normal back-and-forth
// chat's pace.
const conversationLinkTTL = 20 * time.Minute

var (
	convLinkMu    sync.Mutex
	convLinkCache = make(map[string]linkedConversation)
)

// conversationFingerprint derives a stable key for "the conversation state
// as of this exact point in the transcript" from the session and the
// message history formatted so far. Chained turn-to-turn (each turn's
// stored key covers one more exchange than the last), this is safe: two
// unrelated chats sharing an identical multi-message transcript verbatim is
// practically impossible. The one accepted exception is the very first
// link, keyed on a single prior message — two different callers under the
// same sessionID opening with byte-identical text within the TTL window
// could collide onto the same cached thread. Bounded by the short TTL above.
func conversationFingerprint(sessionID, transcript string) string {
	h := sha256.Sum256([]byte(sessionID + "\x00" + transcript))
	return hex.EncodeToString(h[:])
}

func loadLinkedConversation(key string) (linkedConversation, bool) {
	convLinkMu.Lock()
	defer convLinkMu.Unlock()

	lc, ok := convLinkCache[key]
	if !ok {
		return linkedConversation{}, false
	}
	if time.Now().After(lc.expiresAt) {
		delete(convLinkCache, key)
		return linkedConversation{}, false
	}
	return lc, true
}

func storeLinkedConversation(key string, lc linkedConversation) {
	convLinkMu.Lock()
	defer convLinkMu.Unlock()

	lc.expiresAt = time.Now().Add(conversationLinkTTL)
	convLinkCache[key] = lc

	// Lazy sweep so one-off keys that are never revisited (e.g. every
	// single-shot curl/automation call that happened to have >1 message)
	// don't accumulate forever between restarts.
	if len(convLinkCache) > 2000 {
		now := time.Now()
		for k, v := range convLinkCache {
			if now.After(v.expiresAt) {
				delete(convLinkCache, k)
			}
		}
	}
}
