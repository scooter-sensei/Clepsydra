// SipHash-2-4 (Aumasson & Bernstein), the keyed PRF behind Config::Key
// (go/internal/siphash).
#pragma once

#include <array>
#include <bit>
#include <cstdint>
#include <cstring>
#include <span>
#include <string_view>

namespace clepsydra::detail::siphash {

inline std::uint64_t le64(const unsigned char* b) {
  return std::uint64_t{b[0]} | std::uint64_t{b[1]} << 8 | std::uint64_t{b[2]} << 16 | std::uint64_t{b[3]} << 24 |
         std::uint64_t{b[4]} << 32 | std::uint64_t{b[5]} << 40 | std::uint64_t{b[6]} << 48 |
         std::uint64_t{b[7]} << 56;
}

// Key is a 128-bit SipHash key.
struct Key {
  std::uint64_t k0 = 0, k1 = 0;

  // NewKey reads k as two little-endian words.
  static Key New(const std::array<std::uint8_t, 16>& k) { return Key{le64(k.data()), le64(k.data() + 8)}; }

  static void round(std::uint64_t& v0, std::uint64_t& v1, std::uint64_t& v2, std::uint64_t& v3) {
    v0 += v1;
    v1 = std::rotl(v1, 13) ^ v0;
    v0 = std::rotl(v0, 32);
    v2 += v3;
    v3 = std::rotl(v3, 16) ^ v2;
    v0 += v3;
    v3 = std::rotl(v3, 21) ^ v0;
    v2 += v1;
    v1 = std::rotl(v1, 17) ^ v2;
    v2 = std::rotl(v2, 32);
  }

  static std::uint64_t finish(std::uint64_t v0, std::uint64_t v1, std::uint64_t v2, std::uint64_t v3,
                              std::uint64_t last) {
    v3 ^= last;
    round(v0, v1, v2, v3);
    round(v0, v1, v2, v3);
    v0 ^= last;
    v2 ^= 0xff;
    for (int i = 0; i < 4; i++) round(v0, v1, v2, v3);
    return v0 ^ v1 ^ v2 ^ v3;
  }

  std::uint64_t HashString(std::string_view s) const {
    std::uint64_t v0 = k0 ^ 0x736f6d6570736575, v1 = k1 ^ 0x646f72616e646f6d, v2 = k0 ^ 0x6c7967656e657261,
                  v3 = k1 ^ 0x7465646279746573;
    const auto* b = reinterpret_cast<const unsigned char*>(s.data());
    std::size_t n = s.size(), i = 0;
    for (; n - i >= 8; i += 8) {
      std::uint64_t m = le64(b + i);
      v3 ^= m;
      round(v0, v1, v2, v3);
      round(v0, v1, v2, v3);
      v0 ^= m;
    }
    std::uint64_t last = static_cast<std::uint64_t>(n) << 56;
    for (std::size_t k = 0; i + k < n; k++) last |= std::uint64_t{b[i + k]} << (8 * k);
    return finish(v0, v1, v2, v3, last);
  }

  // HashUint64 hashes x's 8 little-endian bytes.
  std::uint64_t HashUint64(std::uint64_t x) const {
    std::uint64_t v0 = k0 ^ 0x736f6d6570736575, v1 = k1 ^ 0x646f72616e646f6d, v2 = k0 ^ 0x6c7967656e657261,
                  v3 = k1 ^ 0x7465646279746573;
    v3 ^= x;
    round(v0, v1, v2, v3);
    round(v0, v1, v2, v3);
    v0 ^= x;
    return finish(v0, v1, v2, v3, std::uint64_t{8} << 56);
  }

  // HashUint64s sets out[i] = HashUint64(keys[i]) for every i. (The
  // reference hashes 8 keys at once on AVX-512F; this port hashes one at a
  // time everywhere, with the same results.)
  void HashUint64s(std::span<const std::uint64_t> keys, std::span<std::uint64_t> out) const {
    for (std::size_t i = 0; i < keys.size(); i++) out[i] = HashUint64(keys[i]);
  }
};

}  // namespace clepsydra::detail::siphash
