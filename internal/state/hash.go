// Package state reads and writes Ninja's on-disk state: the build log
// (.ninja_log) and the deps log (.ninja_deps), in the formats Ninja 1.10
// through 1.13 use, so kiln and Ninja can share one build directory.
package state

import (
	"encoding/binary"
	"math/bits"
)

// HashCommand returns the command hash a build log of the given version
// records. Hash Edge.Command(true), which includes response file content.
// Versions 5 and 6 use MurmurHash64A; version 7 (Ninja 1.13) uses
// rapidhash.
func HashCommand(logVersion int, command string) uint64 {
	if logVersion >= 7 {
		return rapidhash([]byte(command))
	}
	return murmurHash64A([]byte(command))
}

// murmurHash64A is Austin Appleby's MurmurHash64A with Ninja's seed,
// ported from Ninja's src/build_log.cc (1.10–1.12).
func murmurHash64A(data []byte) uint64 {
	const (
		seed = 0xDECAFBADDECAFBAD
		m    = 0xc6a4a7935bd1e995
		r    = 47
	)
	h := uint64(seed) ^ uint64(len(data))*m
	for len(data) >= 8 {
		k := binary.LittleEndian.Uint64(data)
		k *= m
		k ^= k >> r
		k *= m
		h ^= k
		h *= m
		data = data[8:]
	}
	if n := len(data); n > 0 {
		for i := n - 1; i >= 1; i-- {
			h ^= uint64(data[i]) << (8 * i)
		}
		h ^= uint64(data[0])
		h *= m
	}
	h ^= h >> r
	h *= m
	h ^= h >> r
	return h
}

// rapidhash is the version Ninja 1.13 vendors (third_party/rapidhash,
// RAPIDHASH_FAST and RAPIDHASH_UNROLLED, little-endian), with its default
// seed.
func rapidhash(p []byte) uint64 {
	const seed0 = 0xbdd89aa982704029
	secret := [3]uint64{0x2d358dccaa6c78a5, 0x8bb84b93962eacc9, 0x4b33a62ed433d4a3}
	n := uint64(len(p))
	seed := seed0 ^ rapidMix(seed0^secret[0], secret[1]) ^ n
	var a, b uint64
	if n <= 16 {
		switch {
		case n >= 4:
			last := n - 4
			a = read32(p, 0)<<32 | read32(p, last)
			delta := (n & 24) >> (n >> 3)
			b = read32(p, delta)<<32 | read32(p, last-delta)
		case n > 0:
			a = uint64(p[0])<<56 | uint64(p[n>>1])<<32 | uint64(p[n-1])
		}
	} else {
		i := n
		off := uint64(0)
		if i > 48 {
			see1, see2 := seed, seed
			for i >= 96 {
				seed = rapidMix(read64(p, off)^secret[0], read64(p, off+8)^seed)
				see1 = rapidMix(read64(p, off+16)^secret[1], read64(p, off+24)^see1)
				see2 = rapidMix(read64(p, off+32)^secret[2], read64(p, off+40)^see2)
				seed = rapidMix(read64(p, off+48)^secret[0], read64(p, off+56)^seed)
				see1 = rapidMix(read64(p, off+64)^secret[1], read64(p, off+72)^see1)
				see2 = rapidMix(read64(p, off+80)^secret[2], read64(p, off+88)^see2)
				off += 96
				i -= 96
			}
			if i >= 48 {
				seed = rapidMix(read64(p, off)^secret[0], read64(p, off+8)^seed)
				see1 = rapidMix(read64(p, off+16)^secret[1], read64(p, off+24)^see1)
				see2 = rapidMix(read64(p, off+32)^secret[2], read64(p, off+40)^see2)
				off += 48
				i -= 48
			}
			seed ^= see1 ^ see2
		}
		if i > 16 {
			seed = rapidMix(read64(p, off)^secret[2], read64(p, off+8)^seed^secret[1])
			if i > 32 {
				seed = rapidMix(read64(p, off+16)^secret[2], read64(p, off+24)^seed)
			}
		}
		a = read64(p, off+i-16)
		b = read64(p, off+i-8)
	}
	a ^= secret[1]
	b ^= seed
	a, b = rapidMum(a, b)
	return rapidMix(a^secret[0]^n, b^secret[1])
}

// rapidMum returns the low and high halves of the 128-bit product.
func rapidMum(a, b uint64) (lo, hi uint64) {
	hi, lo = bits.Mul64(a, b)
	return lo, hi
}

func rapidMix(a, b uint64) uint64 {
	lo, hi := rapidMum(a, b)
	return lo ^ hi
}

func read64(p []byte, off uint64) uint64 { return binary.LittleEndian.Uint64(p[off:]) }

func read32(p []byte, off uint64) uint64 { return uint64(binary.LittleEndian.Uint32(p[off:])) }
