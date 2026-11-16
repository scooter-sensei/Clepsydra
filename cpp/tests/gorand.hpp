// Go's math/rand/v2 Rand over its PCG source, bit for bit: the Go suite's
// seeded streams (the fuzzer's cases, the conformance vectors' scenarios,
// the scorecard's workloads) replay here exactly as there.
#pragma once

#include <cmath>
#include <cstdint>
#include <stdexcept>
#include <utility>
#include <vector>

#include "bits.hpp"

namespace gorand {

#include "goexp.inc"

// PCG is rand.NewPCG: a 128-bit LCG with the DXSM output function.
struct PCG {
  std::uint64_t hi, lo;
  PCG(std::uint64_t seed1, std::uint64_t seed2) : hi(seed1), lo(seed2) {}

  void next(std::uint64_t& h, std::uint64_t& l) {
    constexpr std::uint64_t mulHi = 2549297995355413924ull, mulLo = 4865540595714422341ull,
                            incHi = 6364136223846793005ull, incLo = 1442695040888963407ull;
    clepsydra::detail::mul64(lo, mulLo, h, l);
    h += hi * mulLo + lo * mulHi;
    std::uint64_t nl = l + incLo;
    std::uint64_t c = nl < l ? 1 : 0;
    h = h + incHi + c;
    l = nl;
    lo = l;
    hi = h;
  }

  std::uint64_t Uint64() {
    std::uint64_t h, l;
    next(h, l);
    constexpr std::uint64_t cheapMul = 0xda942042e4dd58b5ull;
    h ^= h >> 32;
    h *= cheapMul;
    h ^= h >> 48;
    h *= (l | 1);
    return h;
  }
};

// Rand is rand.New(src) for a PCG source.
struct Rand {
  PCG src;
  Rand(std::uint64_t seed1, std::uint64_t seed2) : src(seed1, seed2) {}

  std::uint64_t Uint64() { return src.Uint64(); }
  std::int64_t Int64() { return static_cast<std::int64_t>(src.Uint64() & ~(std::uint64_t{1} << 63)); }

  // uint64n is Lemire's bounded draw, as Go's.
  std::uint64_t uint64n(std::uint64_t n) {
    if ((n & (n - 1)) == 0) return Uint64() & (n - 1);  // a power of two: mask
    std::uint64_t hi, lo;
    clepsydra::detail::mul64(Uint64(), n, hi, lo);
    if (lo < n) {
      std::uint64_t thresh = (0 - n) % n;
      while (lo < thresh) clepsydra::detail::mul64(Uint64(), n, hi, lo);
    }
    return hi;
  }
  std::uint64_t Uint64N(std::uint64_t n) {
    if (n == 0) throw std::invalid_argument("invalid argument to Uint64N");
    return uint64n(n);
  }
  std::int64_t IntN(std::int64_t n) {
    if (n <= 0) throw std::invalid_argument("invalid argument to IntN");
    return static_cast<std::int64_t>(uint64n(static_cast<std::uint64_t>(n)));
  }
  double Float64() { return static_cast<double>((Uint64() << 11) >> 11) / 9007199254740992.0; }

  // ExpFloat64 is Go's ziggurat draw (Marsaglia & Tsang) over Go's own
  // tables, so a seeded stream of exponential gaps replays as in Go. The
  // tail and the wedge test call log and exp: libm's here, Go's assembly
  // there; TestGoRand checks a long stream against Go's values.
  double ExpFloat64() {
    constexpr double re = 7.69711747013104972;
    for (;;) {
      std::uint64_t u = Uint64();
      std::uint32_t j = static_cast<std::uint32_t>(u);
      std::uint8_t i = static_cast<std::uint8_t>(u >> 32);
      double x = static_cast<double>(j) * static_cast<double>(expWe[i]);
      if (j < expKe[i]) return x;
      if (i == 0) return re - std::log(Float64());
      if (expFe[i] + static_cast<float>(Float64()) * (expFe[i - 1] - expFe[i]) < static_cast<float>(std::exp(-x)))
        return x;
    }
  }

  // Shuffle is Go's Fisher-Yates shuffle.
  template <class Swap>
  void Shuffle(std::int64_t n, Swap swap) {
    for (std::int64_t i = n - 1; i > 0; i--)
      swap(i, static_cast<std::int64_t>(uint64n(static_cast<std::uint64_t>(i + 1))));
  }

  // Perm is Go's rand.Perm: a permutation of [0, n).
  std::vector<std::int64_t> Perm(std::int64_t n) {
    std::vector<std::int64_t> p(static_cast<std::size_t>(n));
    for (std::int64_t i = 0; i < n; i++) p[static_cast<std::size_t>(i)] = i;
    Shuffle(n, [&](std::int64_t a, std::int64_t b) {
      std::swap(p[static_cast<std::size_t>(a)], p[static_cast<std::size_t>(b)]);
    });
    return p;
  }
};

}  // namespace gorand
