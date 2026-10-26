package dsweb

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// chat.deepseek.com gates /api/v0/chat/completion on a proof of work: the
// server publishes a SHA3-256-shaped challenge and the client must find the
// nonce whose DeepSeekHashV1 digest equals it.
//
// DeepSeekHashV1 is SHA3-256 with the last 23 KECCAK-p[1600] rounds instead of
// the full 24. Go's crypto/sha3 only exposes the 24-round permutation, so the
// sponge is spelled out here; the round constants and rotation offsets are the
// public FIPS 202 tables and only the round count is provider-specific.

const (
	powRateBytes             = 136
	powDigestBytes           = 32
	powDomainSuffix          = 0x06
	powRounds                = 23
	powMaxDifficulty         = 250_000
	powDeadlineCheckInterval = 1024
)

var powRoundConstants = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var powRotationOffsets = [25]uint{
	0, 1, 62, 28, 27,
	36, 44, 6, 55, 20,
	3, 10, 43, 25, 39,
	41, 45, 15, 21, 8,
	18, 2, 61, 56, 14,
}

type powState [25]uint64

func rotl64(v uint64, n uint) uint64 {
	if n == 0 {
		return v
	}
	return v<<n | v>>(64-n)
}

func keccakP1600(a *powState) {
	var b powState
	for round := 24 - powRounds; round < 24; round++ {
		var c [5]uint64
		for x := range 5 {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		for x := range 5 {
			d := c[(x+4)%5] ^ rotl64(c[(x+1)%5], 1)
			for y := range 5 {
				a[x+5*y] ^= d
			}
		}
		for y := range 5 {
			for x := range 5 {
				lane := x + 5*y
				b[y+5*((2*x+3*y)%5)] = rotl64(a[lane], powRotationOffsets[lane])
			}
		}
		for y := range 5 {
			row := 5 * y
			for x := range 5 {
				a[x+row] = b[x+row] ^ (^b[(x+1)%5+row] & b[(x+2)%5+row])
			}
		}
		a[0] ^= powRoundConstants[round]
	}
}

func (s *powState) xorByte(pos int, b byte) {
	s[pos/8] ^= uint64(b) << (8 * uint(pos%8))
}

func (s *powState) digest() [powDigestBytes]byte {
	var out [powDigestBytes]byte
	for lane := range powDigestBytes / 8 {
		binary.LittleEndian.PutUint64(out[lane*8:], s[lane])
	}
	return out
}

func absorbBlocks(state *powState, data []byte) int {
	offset := 0
	for offset+powRateBytes <= len(data) {
		for i := range powRateBytes {
			state.xorByte(i, data[offset+i])
		}
		keccakP1600(state)
		offset += powRateBytes
	}
	return offset
}

func padAndPermute(state *powState, tailPos int) {
	state.xorByte(tailPos, powDomainSuffix)
	state.xorByte(powRateBytes-1, 0x80)
	keccakP1600(state)
}

// DeepSeekHashV1 is the reference digest over an arbitrary input.
func DeepSeekHashV1(input []byte) [powDigestBytes]byte {
	var state powState
	offset := absorbBlocks(&state, input)
	tail := input[offset:]
	for i, b := range tail {
		state.xorByte(i, b)
	}
	padAndPermute(&state, len(tail))
	return state.digest()
}

func deepSeekHashV1Hex(input string) string {
	sum := DeepSeekHashV1([]byte(input))
	return hex.EncodeToString(sum[:])
}

// findPowNonce searches prefix+str(nonce) for the nonce whose digest equals
// challenge (hex), over nonce in [0, difficulty). -1 means no match.
func findPowNonce(ctx context.Context, prefix, challenge string, difficulty int, timeout time.Duration) (int, error) {
	if difficulty < 1 || difficulty > powMaxDifficulty {
		return -1, fmt.Errorf("deepseek-web: pow difficulty %d out of range 1-%d", difficulty, powMaxDifficulty)
	}
	target, err := hex.DecodeString(challenge)
	if err != nil || len(target) != powDigestBytes {
		return -1, fmt.Errorf("deepseek-web: pow challenge must be %d hex characters", powDigestBytes*2)
	}

	searchCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		searchCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	prefixBytes := []byte(prefix)
	var base powState
	prefixOffset := absorbBlocks(&base, prefixBytes)
	tailLength := len(prefixBytes) - prefixOffset

	nonceBuf := make([]byte, 0, 20)
	for nonce := range difficulty {
		if nonce%powDeadlineCheckInterval == 0 {
			if err := searchCtx.Err(); err != nil {
				return -1, fmt.Errorf("deepseek-web: pow search stopped at nonce %d of %d: %w", nonce, difficulty, err)
			}
		}

		state := base
		for i := range tailLength {
			state.xorByte(i, prefixBytes[prefixOffset+i])
		}

		position := tailLength
		nonceBuf = strconv.AppendInt(nonceBuf[:0], int64(nonce), 10)
		for _, c := range nonceBuf {
			state.xorByte(position, c)
			position++
			if position == powRateBytes {
				keccakP1600(&state)
				position = 0
			}
		}

		padAndPermute(&state, position)
		if digestMatches(&state, target) {
			return nonce, nil
		}
	}
	return -1, nil
}

func digestMatches(state *powState, target []byte) bool {
	for lane := range powDigestBytes / 8 {
		if state[lane] != binary.LittleEndian.Uint64(target[lane*8:]) {
			return false
		}
	}
	return true
}
