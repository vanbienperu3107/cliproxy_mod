package dsweb

import (
	"context"
	"crypto/sha3"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The vectors come from the sibling implementation's clean-room corpus: the
// round count is not in any specification, so these are the only ground truth.
var hashVectors = []struct {
	input string
	want  string
}{
	{"1122334455667788_1778891543095_0", "311b26ae1e0fe7375e242958ce46db5552a6c67fea3f96880dcd846c63a74286"},
	{"1122334455667788_1778891543095_1", "526f9103dfe22bcda9481b7f304a157b8edb18c5bb96a2061eefec1fbd0db706"},
	{"vector_42_0", "1813da791ddac0da3a225e5878ff3d3ea07577e048f8e9575b82a274b71e3810"},
	{"vector_42_1", "b582fdb4f83662baec734e23373efc860515c03c40decbef076697eb5832f83c"},
	{"vector_42_2", "48b348ad54c372f78ccb6a26cbf156668f4c6a5b7d5a1cf19ccd78888586019b"},
	{"vector_42_3", "2ffed26ea9e1d6f4bbe49a266d98fe04ad9a5ad4c6d765862de8d18c513c3815"},
	{"bb_1778891543095_0", "705e5d630f02d09a8179c6a0fcb0caf7265f08fb206fadca0301224f4422fc64"},
}

func TestDeepSeekHashV1Vectors(t *testing.T) {
	for _, tc := range hashVectors {
		sum := DeepSeekHashV1([]byte(tc.input))
		if got := hex.EncodeToString(sum[:]); got != tc.want {
			t.Fatalf("DeepSeekHashV1(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

// A digest that matches standard SHA3-256 would mean the 23-round permutation
// is not being applied, and every nonce search would fail upstream.
func TestDeepSeekHashV1IsNotFIPS202(t *testing.T) {
	ours := DeepSeekHashV1([]byte("vector_42_0"))
	h := sha3.New256()
	h.Write([]byte("vector_42_0"))
	fips := h.Sum(nil)
	if hex.EncodeToString(ours[:]) == hex.EncodeToString(fips) {
		t.Fatal("DeepSeekHashV1 matches FIPS 202 SHA3-256; the round count is wrong")
	}
}

func TestFindPowNonceFindsIncrementalDigest(t *testing.T) {
	prefix := "1122334455667788_1778891543095_"
	challenge := "311b26ae1e0fe7375e242958ce46db5552a6c67fea3f96880dcd846c63a74286"

	nonce, err := findPowNonce(t.Context(), prefix, challenge, 4, 5*time.Second)
	if err != nil {
		t.Fatalf("findPowNonce: %v", err)
	}
	if nonce < 0 {
		t.Fatal("no nonce found")
	}
	if got := deepSeekHashV1Hex(prefix + strconv.Itoa(nonce)); got != challenge {
		t.Fatalf("nonce %d hashes to %s, want %s", nonce, got, challenge)
	}
}

func TestFindPowNonceRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := findPowNonce(ctx, "p_", strings.Repeat("0", 64), 250_000, time.Second)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
}
