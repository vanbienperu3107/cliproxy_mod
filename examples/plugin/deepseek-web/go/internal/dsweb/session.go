package dsweb

import (
	"context"
	"strings"
	"sync"
)

// Session persistence: one upstream chat session per client conversation.
//
// /api/v0/chat/completion takes a single prompt string and a
// parent_message_id, and the site keeps the conversation on its side. A gateway
// that opens a fresh session and re-sends the whole transcript on every turn
// reproduces nothing a browser does: every message in the upstream session then
// embeds every previous turn, which grows quadratically and is the signal the
// sibling implementation names as the main cause of account-level mutes.
//
// Three conditions have to hold, and each failure falls back to the plain
// behaviour rather than guessing:
//
//   - The client must name its conversation. No session id means no
//     persistence, because sharing one session across two conversations would
//     put one user's context in the other's turn.
//   - The turn must reuse a session no other turn is holding. Two turns in one
//     conversation are a fork of the parent chain, so the second one gets its
//     own session and does not touch the pinned one.
//   - The new prompt must extend what was already sent. A diverging prompt
//     cannot be expressed as a tail, so the session is discarded and a fresh
//     one carries the whole prompt.

type deepSeekSessionEntry struct {
	sessionID       string
	sentPrompt      string
	parentMessageID int64
	busy            bool
}

type deepSeekSessionStore struct {
	mu    sync.Mutex
	items map[string]*deepSeekSessionEntry
}

const maxDeepSeekSessions = 512

var deepSeekSessions = &deepSeekSessionStore{items: make(map[string]*deepSeekSessionEntry)}

// deepSeekSessionKey identifies one conversation. The transport mode is part of
// the key because a session created for a streamed turn and reused by a
// non-streamed one answers with an empty body.
func deepSeekSessionKey(userToken, clientSessionID string, stream bool) string {
	if userToken == "" || clientSessionID == "" {
		return ""
	}
	mode := "nonstream"
	if stream {
		mode = "stream"
	}
	return userToken + "|" + mode + "|" + clientSessionID
}

type deepSeekSessionClaim struct {
	reuse           bool
	sessionID       string
	parentMessageID *int64
	tail            string
	pin             bool
}

// claim takes the conversation's session for one turn. It never blocks: a
// conversation whose session is already in use is served by a fresh,
// unpersisted session for that turn.
func (s *deepSeekSessionStore) claim(key, fullPrompt string) deepSeekSessionClaim {
	if key == "" {
		return deepSeekSessionClaim{tail: fullPrompt}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.items[key]
	if ok && entry.busy {
		return deepSeekSessionClaim{tail: fullPrompt}
	}
	if !ok {
		return deepSeekSessionClaim{tail: fullPrompt, pin: true}
	}

	tail, reusable := deepSeekPromptTail(entry.sentPrompt, fullPrompt, entry.parentMessageID > 0)
	if !reusable {
		delete(s.items, key)
		return deepSeekSessionClaim{tail: fullPrompt, pin: true}
	}

	entry.busy = true
	parent := entry.parentMessageID
	return deepSeekSessionClaim{
		reuse:           true,
		sessionID:       entry.sessionID,
		parentMessageID: &parent,
		tail:            tail,
		pin:             true,
	}
}

func (s *deepSeekSessionStore) commit(claim deepSeekSessionClaim, key, sessionID, fullPrompt string, responseMessageID *int64) {
	if key == "" || !claim.pin {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if sessionID == "" || responseMessageID == nil {
		delete(s.items, key)
		return
	}
	if _, exists := s.items[key]; !exists && len(s.items) >= maxDeepSeekSessions {
		s.evictOldestLocked()
	}
	s.items[key] = &deepSeekSessionEntry{
		sessionID:       sessionID,
		sentPrompt:      fullPrompt,
		parentMessageID: *responseMessageID,
	}
}

// forget drops a conversation's entry, but never one another turn holds.
func (s *deepSeekSessionStore) forget(key, sessionID string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.items[key]; ok && sessionID != "" && entry.sessionID != sessionID {
		return
	}
	delete(s.items, key)
}

func (s *deepSeekSessionStore) holds(key, sessionID string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.items[key]
	return ok && entry.sessionID == sessionID
}

func (s *deepSeekSessionStore) evictOldestLocked() {
	for key := range s.items {
		delete(s.items, key)
		return
	}
}

func (s *deepSeekSessionStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]*deepSeekSessionEntry)
}

// deepSeekPromptTail returns the part of fullPrompt that sentPrompt does not
// already contain, and whether the upstream can be given only that part.
func deepSeekPromptTail(sentPrompt, fullPrompt string, haveParent bool) (string, bool) {
	if !haveParent {
		return fullPrompt, false
	}
	if sentPrompt == fullPrompt {
		return fullPrompt, true
	}
	if !strings.HasPrefix(fullPrompt, sentPrompt) {
		return fullPrompt, false
	}
	tail := strings.TrimLeft(fullPrompt[len(sentPrompt):], "\n")
	if tail == "" {
		return fullPrompt, true
	}
	return tail, true
}

type deepSeekSession struct {
	id          string
	claim       deepSeekSessionClaim
	key         string
	createdHere bool
}

// deepSeekAcquireSession resolves the session one turn runs against.
func deepSeekAcquireSession(
	ctx context.Context,
	turn deepSeekTurn,
	accessToken string,
	creds Credentials,
	clientSessionID string,
	stream bool,
	prompt string,
) (deepSeekSession, error) {
	session := deepSeekSession{claim: deepSeekSessionClaim{tail: prompt}}

	if creds.PersistSession {
		session.key = deepSeekSessionKey(creds.UserToken, clientSessionID, stream)
		session.claim = deepSeekSessions.claim(session.key, prompt)
	}

	session.id = session.claim.sessionID
	if session.claim.reuse {
		return session, nil
	}

	id, err := turn.createSession(ctx, accessToken)
	if err != nil {
		return deepSeekSession{}, err
	}
	session.id = id
	session.createdHere = true
	return session, nil
}

// deepSeekSettleSession keeps the conversation's session when the turn produced
// something the next turn can continue from, and drops it otherwise. It reports
// whether the store now owns the session: false means the caller must close it
// upstream.
func deepSeekSettleSession(session deepSeekSession, prompt string, accumulator *deepSeekStreamAccumulator, runErr error) bool {
	if session.key == "" || !session.claim.pin {
		return false
	}
	switch {
	case runErr != nil, accumulator.enforcement != nil, !accumulator.sawFinished:
		deepSeekSessions.forget(session.key, session.id)
		return false
	}
	deepSeekSessions.commit(session.claim, session.key, session.id, prompt, accumulator.responseMessageID)
	return deepSeekSessions.holds(session.key, session.id)
}

// deepSeekAbandonSession drops a conversation's session after a turn that must
// not continue it. A session this turn created is closed upstream; a reused one
// is only forgotten.
func deepSeekAbandonSession(session deepSeekSession, ctx context.Context, turn deepSeekTurn, accessToken string) {
	if session.key != "" && session.claim.pin {
		deepSeekSessions.forget(session.key, session.id)
	}
	if session.createdHere {
		turn.deleteSession(ctx, accessToken, session.id)
	}
}
