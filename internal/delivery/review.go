package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

var (
	ErrReviewConflict = errors.New("draft changed, was superseded, or already has a different review")
	ErrInvalidReview  = errors.New("review requires a draft digest, operator identity, and message of at most 16000 bytes")
)

type Review struct {
	Body        string `json:"body"`
	Actor       string `json:"actor"`
	DraftDigest string `json:"draft_digest"`
}

func DraftDigest(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

func (r Review) Validate() error {
	digest, err := hex.DecodeString(r.DraftDigest)
	if err != nil || len(digest) != sha256.Size || strings.ToLower(r.DraftDigest) != r.DraftDigest ||
		strings.TrimSpace(r.Actor) == "" || len(r.Actor) > 256 || !utf8.ValidString(r.Actor) || strings.ContainsRune(r.Actor, 0) ||
		strings.TrimSpace(r.Body) == "" || len(r.Body) > 16000 || !utf8.ValidString(r.Body) || strings.ContainsRune(r.Body, 0) {
		return ErrInvalidReview
	}
	return nil
}
