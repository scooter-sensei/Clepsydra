// The filter's platform-specific primitives (go/internal/cpu): a 16-byte
// compare-and-swap and line prefetches, as instructions where the
// architecture has them and plain C++ elsewhere.
#pragma once

#include <atomic>
#include <cstdint>
#include <cstdlib>

#if defined(_MSC_VER) && !defined(__clang__)
#include <intrin.h>
#elif defined(__x86_64__)
#include <cpuid.h>
#endif

namespace clepsydra::detail::cpu {

// cpuHasCX16 reports CMPXCHG16B (CPUID leaf 1, ECX bit 13), as the reference
// probes it: x86-64's baseline doesn't promise the instruction.
inline bool cpuHasCX16() {
#if defined(_MSC_VER) && !defined(__clang__) && defined(_M_X64)
  int r[4];
  __cpuid(r, 1);
  return (r[2] >> 13) & 1;
#elif defined(__x86_64__)
  unsigned a, b, c, d;
  if (!__get_cpuid(1, &a, &b, &c, &d)) return false;
  return (c >> 13) & 1;
#else
  return false;  // arm64's CASP would do; not yet measured (as the reference)
#endif
}

// HasCAS2: realign, stamp, unpoint and a demotion use one 16-byte CAS instead
// of a hold of word1. Tests: TF_NOCAS2=1 turns it off, so the slot is held as
// on other architectures.
inline bool HasCAS2 = cpuHasCX16();

// CAS2 atomically replaces the 16-byte pair at p (16-byte aligned: a slot)
// with new0, new1 if it still holds old0, old1. Only called when HasCAS2.
//
// The pair is two std::atomic<std::uint64_t> that other calls load and CAS
// one at a time; a 16-byte locked CMPXCHG16B over them is what the reference
// does too (mixed-size atomics are outside the C++ memory model, but every
// x86-64 CPU orders a locked instruction against plain and locked accesses
// to the same lines).
inline bool CAS2(std::atomic<std::uint64_t>* p, std::uint64_t old0, std::uint64_t old1, std::uint64_t new0,
                 std::uint64_t new1) {
#if defined(_MSC_VER) && !defined(__clang__) && defined(_M_X64)
  alignas(16) long long cmp[2] = {static_cast<long long>(old0), static_cast<long long>(old1)};
  return _InterlockedCompareExchange128(reinterpret_cast<volatile long long*>(p), static_cast<long long>(new1),
                                        static_cast<long long>(new0), cmp) != 0;
#elif defined(__x86_64__)
  bool ok;
  __asm__ __volatile__("lock cmpxchg16b %1"
                       : "=@ccz"(ok), "+m"(*reinterpret_cast<volatile unsigned __int128*>(p)), "+a"(old0),
                         "+d"(old1)
                       : "b"(new0), "c"(new1)
                       : "memory");
  return ok;
#else
  (void)p, (void)old0, (void)old1, (void)new0, (void)new1;
  std::abort();  // unreachable: HasCAS2 is false here
#endif
}

// PrefetchLines asks for the 64-byte lines at words + 8*lines[i], i < n
// (PREFETCHT0, PRFM PLDL1KEEP), without waiting for them.
inline void PrefetchLines(const void* words, const std::uint64_t* lines, int n) {
  const char* base = static_cast<const char*>(words);
  for (int i = 0; i < n; i++) {
#if defined(_MSC_VER) && !defined(__clang__)
    _mm_prefetch(base + lines[i] * 64, _MM_HINT_T0);
#else
    __builtin_prefetch(base + lines[i] * 64, 0, 3);
#endif
  }
}

}  // namespace clepsydra::detail::cpu
