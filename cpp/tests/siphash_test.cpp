// SipHash-2-4 reference vectors (go/internal/siphash/siphash_test.go) and the
// batch hash (go/internal/siphash/batch_test.go).
#include <array>
#include <cstdint>
#include <span>
#include <vector>

#include "check.hpp"
#include "siphash.hpp"

using clepsydra::detail::siphash::Key;

TEST(TestSipHashVectors) {
  std::array<std::uint8_t, 16> key{};
  for (int i = 0; i < 16; i++) key[i] = static_cast<std::uint8_t>(i);
  Key k = Key::New(key);
  std::string msg(15, '\0');
  for (int i = 0; i < 15; i++) msg[i] = static_cast<char>(i);
  // Reference outputs from the SipHash paper and its test vectors.
  CHECK_EQ(k.HashString(std::string_view(msg.data(), 0)), std::uint64_t{0x726fdb47dd0e0e31});
  CHECK_EQ(k.HashString(std::string_view(msg.data(), 15)), std::uint64_t{0xa129ca6149be45e5});
  for (std::uint64_t x : {std::uint64_t{0}, std::uint64_t{1}, std::uint64_t{0x0706050403020100}, ~std::uint64_t{0}}) {
    char b[8];
    for (int i = 0; i < 8; i++) b[i] = static_cast<char>(x >> (8 * i));
    CHECK_EQ(k.HashUint64(x), k.HashString(std::string_view(b, 8)));
  }
  // HashUint64s equals HashUint64 key by key (D25).
  std::uint64_t keys[11], out[11];
  for (int i = 0; i < 11; i++) keys[i] = static_cast<std::uint64_t>(i) * 0x9e3779b97f4a7c15;
  k.HashUint64s(keys, out);
  for (int i = 0; i < 11; i++) CHECK_EQ(out[i], k.HashUint64(keys[i]));
}

// TestHashUint64sMatches (go/internal/siphash/batch_test.go): HashUint64s
// equals HashUint64 on every batch length 0-130 and on 1M keys in batches of
// random lengths, under random keys, and writes nothing past the batch. (The
// reference runs it twice, through its AVX-512 hash8x and without; this port
// has the one scalar path.)
TEST(TestHashUint64sMatches) {
  std::uint64_t x = 0x13198a2e03707344;
  auto next = [&x] {
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    return x;
  };
  std::vector<std::uint64_t> keys(130), out(130);
  int total = 0;
  for (int c = 0; total < 1'000'000; c++) {
    std::array<std::uint8_t, 16> kb{};
    for (int half = 0; half < 2; half++) {
      std::uint64_t v = next();
      for (int i = 0; i < 8; i++) kb[static_cast<std::size_t>(8 * half + i)] = static_cast<std::uint8_t>(v >> (8 * i));
    }
    Key k = Key::New(kb);
    int n = c % 131;  // every length 0-130 first, then the same again
    if (c > 131) n = static_cast<int>(next() % 131);
    for (int i = 0; i < n; i++) keys[static_cast<std::size_t>(i)] = next();
    for (auto& o : out) o = 0xdeadbeef;
    k.HashUint64s(std::span<const std::uint64_t>(keys.data(), static_cast<std::size_t>(n)), out);
    for (int i = 0; i < n; i++) {
      if (std::uint64_t want = k.HashUint64(keys[static_cast<std::size_t>(i)]); out[static_cast<std::size_t>(i)] != want)
        FATALF("batch of %d, key %d (%#x): %#x, want %#x", n, i, keys[static_cast<std::size_t>(i)],
               out[static_cast<std::size_t>(i)], want);
    }
    for (int i = n; i < 130; i++) {
      if (out[static_cast<std::size_t>(i)] != 0xdeadbeef) FATALF("batch of %d: out[%d] written", n, i);
    }
    total += n;
  }
}
