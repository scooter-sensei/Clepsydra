// Go's math/bits and the few runtime helpers the reference uses, for every
// compiler this port builds with (GCC and Clang: unsigned __int128, as u128; MSVC: its
// intrinsics).
#pragma once

#include <bit>
#include <cstdint>
#include <thread>

#if defined(_MSC_VER) && !defined(__clang__)
#include <intrin.h>
#endif

namespace clepsydra::detail {

#if !(defined(_MSC_VER) && !defined(__clang__))
// u128 is GCC's and Clang's 128-bit integer. ISO C++ has none, so -Wpedantic
// flags the keyword (an error under GCC with -Werror); __extension__ on its
// one declaration keeps every use quiet.
__extension__ typedef unsigned __int128 u128;
#endif

// mulHi is bits.Mul64's high word.
inline std::uint64_t mulHi(std::uint64_t a, std::uint64_t b) {
#if defined(_MSC_VER) && !defined(__clang__)
  return __umulh(a, b);
#else
  return static_cast<std::uint64_t>((static_cast<u128>(a) * b) >> 64);
#endif
}

// mul64 is bits.Mul64: the 128-bit product of a and b.
inline void mul64(std::uint64_t a, std::uint64_t b, std::uint64_t& hi, std::uint64_t& lo) {
#if defined(_MSC_VER) && !defined(__clang__)
  lo = _umul128(a, b, &hi);
#else
  u128 p = static_cast<u128>(a) * b;
  hi = static_cast<std::uint64_t>(p >> 64);
  lo = static_cast<std::uint64_t>(p);
#endif
}

// div64 is bits.Div64's quotient of (hi, lo) by y; hi < y (Go panics
// otherwise, and every caller here guarantees it).
inline std::uint64_t div64(std::uint64_t hi, std::uint64_t lo, std::uint64_t y) {
#if defined(_MSC_VER) && !defined(__clang__)
  std::uint64_t rem;
  return _udiv128(hi, lo, y, &rem);
#else
  u128 n = (static_cast<u128>(hi) << 64) | lo;
  return static_cast<std::uint64_t>(n / y);
#endif
}

// gosched is runtime.Gosched.
inline void gosched() { std::this_thread::yield(); }

}  // namespace clepsydra::detail
